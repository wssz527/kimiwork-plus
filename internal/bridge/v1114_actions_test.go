package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func TestNativeExplicitOperationTakesSlotAfterAutoSync(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{true: "cancel", false: "handoff"}[cancelWait], func(t *testing.T) {
			n := &Native{}
			n.autoSync = newAutoSyncCoordinator(context.Background(), n)
			defer n.autoSync.stop()
			background, claimed := n.beginAutoSyncOperation()
			if !claimed {
				t.Fatal("auto-sync could not claim the empty slot")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type claim struct {
				id  string
				err error
			}
			next := make(chan claim, 1)
			go func() {
				id, err := n.beginOperation(ctx, "open-work")
				next <- claim{id, err}
			}()
			waitAutoSync(t, "explicit action reservation", func() bool {
				n.mu.Lock()
				defer n.mu.Unlock()
				return n.opWaiter
			})
			if _, err := n.beginOperation(context.Background(), "account-switch"); core.ErrorCode(err) != "operation_busy" {
				t.Fatalf("additional explicit request was queued: %v", err)
			}
			if cancelWait {
				cancel()
				if got := <-next; core.ErrorCode(got.err) != "operation_wait_cancelled" {
					t.Fatalf("cancelled claim = %+v", got)
				}
				n.releaseOperation(background)
			} else {
				n.finishOperation(background, "applied", nil, nil)
				got := <-next
				if got.err != nil || got.id == "" {
					t.Fatalf("explicit claim = %+v", got)
				}
				if _, ok := n.beginAutoSyncOperation(); ok {
					t.Fatal("auto-sync preempted the explicit action")
				}
				n.releaseOperation(got.id)
			}
			n.autoSync.mu.Lock()
			suspended := n.autoSync.suspended
			n.autoSync.mu.Unlock()
			if suspended != 0 || !n.autoSyncOperationIdle() {
				t.Fatal("operation release did not resume auto-sync")
			}
		})
	}
}

func TestDisconnectModelsReportsOptionalDNSOutcome(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("managed hosts setup requires macOS or Windows")
	}
	for _, tc := range []struct {
		name    string
		remove  bool
		managed bool
		warning string
	}{
		{name: "preserve", remove: false, managed: true},
		{name: "no managed hosts", remove: true},
		{name: "remove managed hosts", remove: true, managed: true},
		{name: "authorization refused", remove: true, managed: true, warning: "setup_cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newBYOKTestNative(t)
			n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
				return kimi.DisconnectResult{OK: true, State: "removed", Sources: 2}, nil
			}
			hosts := []byte("127.0.0.1 localhost\n")
			if tc.managed {
				var err error
				hosts, _, err = byokdns.UpdateHosts(hosts, "prepare")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(n.hostsPath, hosts, 0o600); err != nil {
				t.Fatal(err)
			}
			adminCalls := 0
			n.admin = func(_ context.Context, _, report string) error {
				adminCalls++
				if tc.warning != "" {
					return errors.New(tc.warning)
				}
				remaining, _, err := byokdns.UpdateHosts(hosts, "remove-dns")
				if err != nil {
					return err
				}
				if err := os.WriteFile(n.hostsPath, remaining, 0o600); err != nil {
					return err
				}
				if report != "" {
					return os.WriteFile(report, []byte("done\n"), 0o600)
				}
				return nil
			}
			if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models", "removeDns": tc.remove}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			data, _ := json.Marshal(op["result"])
			var result core.Document
			_ = json.Unmarshal(data, &result)
			if op["state"] != "removed" || result["ok"] != true || result["sources"] != float64(2) {
				t.Fatalf("model removal = %v", op)
			}
			switch {
			case !tc.remove:
				if _, exists := result["dnsRemoved"]; exists || adminCalls != 0 || !byokdns.HasManagedHostsEntry(n.hostsPath) {
					t.Fatalf("DNS changed on ordinary removal: %v", result)
				}
			case tc.warning != "":
				if result["dnsRemoved"] != false || result["dnsWarning"] != tc.warning || adminCalls != 1 {
					t.Fatalf("DNS warning = %v, admin calls = %d", result, adminCalls)
				}
			case tc.managed:
				if result["dnsRemoved"] != true || adminCalls != 1 || byokdns.HasManagedHostsEntry(n.hostsPath) {
					t.Fatalf("managed DNS removal = %v, admin calls = %d", result, adminCalls)
				}
			default:
				if result["dnsRemoved"] != true || adminCalls != 0 {
					t.Fatalf("no-op DNS removal = %v, admin calls = %d", result, adminCalls)
				}
			}
		})
	}
}

func TestPanelModelUseErrorsIncludeDisplayNames(t *testing.T) {
	store, err := core.Open(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err = store.Update(rev, func(d core.Document) error {
		if _, err := core.MergeConnections(d, []any{core.Document{"id": "test", "name": "Test", "baseURL": "https://example.com", "protocols": []any{"responses"}}}); err != nil {
			return err
		}
		if err := core.MergeModels(d, []any{
			core.Document{"id": "model-a", "name": "模型甲", "connectionId": "test", "protocol": "responses"},
			core.Document{"id": "model-b", "upstreamModelId": "upstream-b", "connectionId": "test", "protocol": "responses"},
			core.Document{"id": "model-c", "name": "模型甲", "connectionId": "test", "protocol": "responses"},
		}); err != nil {
			return err
		}
		d["subagents"] = core.Document{"mode": "force", "model": "model-a"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Endpoint: Endpoint{URL: "http://127.0.0.1:4242", Key: "panel"}, Store: store}
	for _, tc := range []struct {
		path, rows, code string
		status           int
		models           []string
	}{
		{"connections", `"connections":[]`, "connection_in_use", 409, []string{"模型甲", "upstream-b"}},
		{"connections", `"connections":[{"id":"test","name":"Test","baseURL":"https://example.com","protocols":["chat"]}]`, "invalid_connection_protocol_in_use", 400, []string{"模型甲", "upstream-b"}},
		{"models", `"models":[]`, "subagent_model_in_use", 409, []string{"模型甲"}},
		{"models", `"models":[{"id":"model-a"},{"id":"orphan","protocol":"responses","connectionId":"missing"}]`, "connection_unavailable", 409, nil},
	} {
		w := callPanel(t, service, http.MethodPost, "/api/"+tc.path, `{"revision":"`+rev+`",`+tc.rows+`}`)
		var body struct {
			Error struct {
				Code, Message string
				Models        []string
			}
		}
		if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != tc.code || !slices.Equal(body.Error.Models, tc.models) {
			t.Fatalf("%s HTTP %d: %s", tc.code, w.Code, w.Body.String())
		}
		for _, name := range tc.models {
			if !strings.Contains(body.Error.Message, name) {
				t.Fatalf("error message omitted %s: %q", name, body.Error.Message)
			}
		}
	}
}

func TestOpenWorkActionWaitsForResultAndCanStartManagedKimi(t *testing.T) {
	started := false
	workErr := errors.New("native_bridge_waiting_work")
	n := &Native{
		autoOpenWork: func(context.Context) error {
			if started {
				return nil
			}
			return workErr
		},
		autoLaunch: func(context.Context) error { started = true; return nil },
		controller: &kimi.Controller{},
	}
	result, err := n.Action(context.Background(), core.Document{"action": "open-work"})
	if err != nil || !started || core.Object(core.Object(result)["operation"])["state"] != "done" {
		t.Fatalf("open-work result = %v (%v), launched = %v", result, err, started)
	}
	workErr = errors.New("native_bridge_activation_refused")
	started = false
	if _, err := n.Action(context.Background(), core.Document{"action": "open-work"}); !errors.Is(err, workErr) || started {
		t.Fatalf("open-work failed silently: %v", err)
	}
}
