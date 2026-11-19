package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func TestNativeCheckUpstreamReturnsNamedConnectionResult(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("unexpected upstream request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"alpha"}]}`))
	}))
	defer upstream.Close()
	store, err := core.Open(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Update(rev, func(d core.Document) error {
		_, err := core.MergeConnections(d, []any{core.Document{"id": "named", "name": "Named", "baseURL": upstream.URL + "/v1"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{"KIMI_WORK_NAMED_KEY": "synthetic"})
	n := &Native{service: &Service{Store: store, Keys: keys}}
	result, err := n.Action(context.Background(), core.Document{"action": "check-upstream", "connectionId": "named"})
	if err != nil {
		t.Fatal(err)
	}
	operation := core.Object(result.(core.Document)["operation"])
	if operation["state"] != "passed" || operation["connectionId"] != "named" || operation["modelCount"] != 1 || operation["httpStatus"] != 200 {
		t.Fatalf("operation %+v", operation)
	}
	if _, ok := operation["models"]; ok {
		t.Fatal("operation returned the catalog")
	}
	if _, err = n.Action(context.Background(), core.Document{"action": "check-upstream", "connectionId": "missing"}); err == nil || err.Error() != "connection_unavailable" {
		t.Fatalf("unknown connection: %v", err)
	}
	if requests != 1 {
		t.Fatalf("upstream requests %d", requests)
	}
}

func TestNativeOperationConcurrentStateRead(t *testing.T) {
	n := newBYOKTestNative(t)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					n.mu.Lock()
					for range n.operation {
					}
					n.mu.Unlock()
				}
			}
		}()
	}
	for range 64 {
		if _, err := n.Action(context.Background(), core.Document{"action": "prepare-native"}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			n.mu.Lock()
			running := n.operation["state"] == "running"
			_, hasResult := n.operation["result"]
			n.mu.Unlock()
			if !running {
				if !hasResult {
					t.Fatal("completed operation has no import result")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("operation did not finish")
			}
			time.Sleep(time.Millisecond)
		}
	}
	close(stop)
	readers.Wait()
}

func TestNativeOperationIgnoresStaleCompletion(t *testing.T) {
	n := &Native{operation: core.Document{"id": "current", "state": "running"}}
	n.finishOperation("stale", "prepared", kimi.ImportResult{OK: true}, nil)
	if n.operation["state"] != "running" || n.operation["result"] != nil {
		t.Fatalf("stale completion changed current operation: %+v", n.operation)
	}
	n.finishOperation("current", "prepared", kimi.ImportResult{OK: true, Applied: true}, nil)
	if n.operation["state"] != "prepared" || n.operation["result"] == nil {
		t.Fatalf("current completion was not committed: %+v", n.operation)
	}
}
