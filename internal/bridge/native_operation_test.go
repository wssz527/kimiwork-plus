package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

func TestNativeOperationConcurrentStateRead(t *testing.T) {
	n := &Native{controller: &kimi.Controller{}}
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
