package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestNativeDisconnectCompletionClearsOnlySettledImportReceipt(t *testing.T) {
	receipt := &nativeImportReceipt{result: kimi.ImportResult{OK: true, RequiresRestart: true}}
	n := &Native{operation: core.Document{"id": "remove", "action": "disconnect-models", "state": "running"}, receipt: receipt}
	n.finishOperation("remove", "removed", kimi.DisconnectResult{OK: true}, nil)
	if n.receipt != nil {
		t.Fatal("a completed disconnect kept the old import restart receipt")
	}
	n.receipt = receipt
	n.operation = core.Document{"id": "failed", "action": "disconnect-models", "state": "running"}
	n.finishOperation("failed", "failed", kimi.DisconnectResult{}, errors.New("model_disconnect_source_conflict"))
	if n.receipt != receipt {
		t.Fatal("a failed disconnect discarded evidence about the existing import")
	}
	if n.operation["reason"] != "model_disconnect_source_conflict" {
		t.Fatalf("normalized disconnect error code was swallowed: %v", n.operation["reason"])
	}
}

func TestDiagnosticSanitizerPreservesDisconnectModelsActionStageAndReason(t *testing.T) {
	sanitized := sanitizeDiagnosticEvent(diagnosticEvent{
		Time:   1000,
		ID:     "op-1",
		Action: "disconnect-models",
		Phase:  diagnosticPhaseFinish,
		State:  "failed",
		Stage:  diagnosticStageDisconnectModels,
		Reason: "model_disconnect_deferred",
	})
	if sanitized.Action != "disconnect-models" || sanitized.Stage != diagnosticStageDisconnectModels || sanitized.Reason != "model_disconnect_deferred" {
		t.Fatalf("disconnect diagnostic event was redacted: %+v", sanitized)
	}
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

func TestRunWithManagedKimiAutoLaunchAndWorkRetry(t *testing.T) {
	t.Run("unmanaged kimi stops before action or openWork", func(t *testing.T) {
		runCalls := 0
		openCalls := 0
		_, err := runWithManagedKimi(
			context.Background(),
			func(context.Context) error { return kimi.ErrUnmanagedKimiRunning },
			func(context.Context) error { openCalls++; return nil },
			func(context.Context) (kimi.ImportResult, error) { runCalls++; return kimi.ImportResult{OK: true}, nil },
			importWaitingWork,
		)
		if !errors.Is(err, kimi.ErrUnmanagedKimiRunning) {
			t.Fatalf("err = %v, want ErrUnmanagedKimiRunning", err)
		}
		if runCalls != 0 || openCalls != 0 {
			t.Fatalf("unexpected calls after unmanaged error: run=%d open=%d", runCalls, openCalls)
		}
	})

	t.Run("waiting work wakes once and retries", func(t *testing.T) {
		ensureCalls := 0
		openCalls := 0
		runCalls := 0
		result, err := runWithManagedKimi(
			context.Background(),
			func(context.Context) error { ensureCalls++; return nil },
			func(context.Context) error { openCalls++; return nil },
			func(context.Context) (kimi.ImportResult, error) {
				runCalls++
				if runCalls == 1 {
					return kimi.ImportResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
				}
				return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
			},
			importWaitingWork,
		)
		if err != nil || !result.OK || !result.Applied {
			t.Fatalf("retry result = %+v, err = %v", result, err)
		}
		if ensureCalls != 1 || openCalls != 1 || runCalls != 2 {
			t.Fatalf("calls = ensure:%d open:%d run:%d, want 1/1/2", ensureCalls, openCalls, runCalls)
		}
	})

	t.Run("persistent waiting work does not loop", func(t *testing.T) {
		openCalls := 0
		runCalls := 0
		result, err := runWithManagedKimi(
			context.Background(),
			func(context.Context) error { return nil },
			func(context.Context) error { openCalls++; return nil },
			func(context.Context) (kimi.DisconnectResult, error) {
				runCalls++
				return kimi.DisconnectResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
			},
			disconnectWaitingWork,
		)
		if err != nil || result.OK || result.State != kimi.ImportStateWaitingWork {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
		if openCalls != 1 || runCalls != 2 {
			t.Fatalf("calls = open:%d run:%d, want 1/2", openCalls, runCalls)
		}
	})

	t.Run("failed openWork skips second attempt", func(t *testing.T) {
		openCalls := 0
		runCalls := 0
		result, err := runWithManagedKimi(
			context.Background(),
			func(context.Context) error { return nil },
			func(context.Context) error { openCalls++; return errors.New("native_bridge_activation_refused") },
			func(context.Context) (kimi.ImportResult, error) {
				runCalls++
				return kimi.ImportResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
			},
			importWaitingWork,
		)
		if err != nil || result.State != kimi.ImportStateWaitingWork {
			t.Fatalf("result = %+v, err = %v", result, err)
		}
		if openCalls != 1 || runCalls != 1 {
			t.Fatalf("calls = open:%d run:%d, want 1/1", openCalls, runCalls)
		}
	})
}

func TestExplicitSyncAndDisconnectAutoStartBoundaries(t *testing.T) {
	for _, action := range []string{"connect-kimi", "prepare-native", "disconnect-models"} {
		t.Run(action+"/unmanaged_kimi_reports_waiting_exit", func(t *testing.T) {
			n := newBYOKTestNative(t)
			launches := 0
			n.autoLaunch = func(context.Context) error {
				launches++
				return kimi.ErrUnmanagedKimiRunning
			}
			receipt := &nativeImportReceipt{action: "connect-kimi", result: kimi.ImportResult{OK: true, RequiresRestart: true}}
			n.receipt = receipt
			if _, err := n.Action(context.Background(), core.Document{"action": action}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			if launches != 1 {
				t.Fatalf("autoLaunch calls = %d, want 1", launches)
			}
			if op["state"] != "waiting-kimi-exit" || op["reason"] != kimi.ErrUnmanagedKimiRunning.Error() {
				t.Fatalf("operation = %+v, want waiting-kimi-exit/unmanaged_kimi_running", op)
			}
			if n.receipt != receipt {
				t.Fatal("existing import receipt was cleared by waiting-kimi-exit")
			}
		})

		t.Run(action+"/auto_launch_failure_surfaces_reason", func(t *testing.T) {
			n := newBYOKTestNative(t)
			launches := 0
			n.autoLaunch = func(context.Context) error {
				launches++
				return errors.New("kimi_start_failed")
			}
			if _, err := n.Action(context.Background(), core.Document{"action": action}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			if launches != 1 {
				t.Fatalf("autoLaunch calls = %d, want 1", launches)
			}
			if op["state"] != "failed" || op["reason"] != "kimi_start_failed" {
				t.Fatalf("operation = %+v, want failed/kimi_start_failed", op)
			}
		})
	}

	t.Run("direct mode validates catalog before auto-launching Kimi", func(t *testing.T) {
		n := newBYOKTestNative(t)
		store, err := core.Open(t.TempDir() + "/state")
		if err != nil {
			t.Fatal(err)
		}
		n.service = &Service{Store: store, Keys: CacheKeys(testKeys{})}
		settings := n.controller.Settings()
		settings.IntegrationMode = kimi.IntegrationModeDirect
		if err := n.controller.Configure(settings); err != nil {
			t.Fatal(err)
		}
		launches := 0
		n.autoLaunch = func(context.Context) error {
			launches++
			return nil
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if launches != 0 {
			t.Fatalf("direct mode launched Kimi %d times despite invalid direct catalog", launches)
		}
		if op["state"] != "failed" {
			t.Fatalf("operation = %+v, want failed", op)
		}
	})
}

func TestRealStartManagedOfficialPreservesErrorCode(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("official profile is not supported on this platform")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AppData", dir)

	n := newBYOKTestNative(t)
	settings := n.controller.Settings()
	settings.Installation = kimi.Installation{Application: filepath.Join(dir, "missing-kimi")}
	if err := n.controller.Configure(settings); err != nil {
		t.Fatal(err)
	}
	n.autoLaunch = n.startManagedOfficial

	if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
		t.Fatal(err)
	}
	op := waitBYOKOperation(t, n)
	if op["state"] == "waiting-kimi-exit" {
		if op["reason"] != kimi.ErrUnmanagedKimiRunning.Error() {
			t.Fatalf("unmanaged operation = %+v", op)
		}
		return
	}
	if op["state"] != "failed" || op["reason"] != "kimi_start_failed" {
		t.Fatalf("real StartKimi failure was collapsed: operation = %+v, want failed/kimi_start_failed", op)
	}
}

func TestExplicitActionsWaitWorkRetryAndWaitingLogin(t *testing.T) {
	for _, action := range []string{"connect-kimi", "prepare-native"} {
		t.Run(action+"/gateway_waiting_work_wakes_once_and_succeeds", func(t *testing.T) {
			n := newBYOKTestNative(t)
			launches, opens, runs := 0, 0, 0
			n.autoLaunch = func(context.Context) error { launches++; return nil }
			n.autoOpenWork = func(context.Context) error { opens++; return nil }
			n.importModels = func(context.Context) (kimi.ImportResult, error) {
				runs++
				if runs == 1 {
					return kimi.ImportResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
				}
				return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
			}
			if _, err := n.Action(context.Background(), core.Document{"action": action}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			if op["state"] != "prepared" {
				t.Fatalf("operation = %+v, want prepared", op)
			}
			if launches != 1 || opens != 1 || runs != 2 {
				t.Fatalf("calls = launch:%d open:%d run:%d, want 1/1/2", launches, opens, runs)
			}
		})

		t.Run(action+"/waiting_login_reports_waiting_login_state", func(t *testing.T) {
			n := newBYOKTestNative(t)
			n.autoLaunch = func(context.Context) error { return nil }
			n.importModels = func(context.Context) (kimi.ImportResult, error) {
				return kimi.ImportResult{State: kimi.ImportStateWaitingLogin, Reason: kimi.NativeWaitingLogin}, nil
			}
			if _, err := n.Action(context.Background(), core.Document{"action": action}); err != nil {
				t.Fatal(err)
			}
			op := waitBYOKOperation(t, n)
			if op["state"] != kimi.ImportStateWaitingLogin || op["reason"] != nil {
				t.Fatalf("operation = %+v, want waiting-login without error reason", op)
			}
		})
	}

	t.Run("connect-kimi/direct_waiting_work_wakes_once_and_succeeds", func(t *testing.T) {
		n := newDirectTestNative(t, true)
		n.dns = &fakeBYOKDNS{}
		n.admin = func(_ context.Context, _, report string) error {
			if report != "" {
				return os.WriteFile(report, []byte("done\n"), 0o600)
			}
			return nil
		}
		store, err := core.Open(t.TempDir() + "/state")
		if err != nil {
			t.Fatal(err)
		}
		_, rev, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = store.Update(rev, func(d core.Document) error {
			d["integrationMode"] = core.IntegrationModeDirect
			if _, err := core.MergeConnections(d, []any{
				directConnection("ds", "DeepSeek", "deepseek", "https://api.deepseek.com/v1", "chat"),
			}); err != nil {
				return err
			}
			return core.MergeModels(d, []any{
				directModel("kimi-ds", "ds", "chat", "tools", true),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		n.service = &Service{Store: store, Keys: CacheKeys(testKeys{"KIMI_WORK_DS_KEY": "sk-direct"})}

		launches, opens, runs := 0, 0, 0
		n.autoLaunch = func(context.Context) error { launches++; return nil }
		n.autoOpenWork = func(context.Context) error { opens++; return nil }
		n.importDirectModels = func(_ context.Context, req kimi.DirectImportRequest) (kimi.ImportResult, error) {
			runs++
			if len(req.Models) == 0 {
				t.Fatal("direct request had no models")
			}
			if runs == 1 {
				return kimi.ImportResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
			}
			return kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, Applied: true}, nil
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "connect-kimi"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "prepared" {
			t.Fatalf("direct operation = %+v, want prepared", op)
		}
		if launches != 1 || opens != 1 || runs != 2 {
			t.Fatalf("direct calls = launch:%d open:%d run:%d, want 1/1/2", launches, opens, runs)
		}
	})

	t.Run("disconnect-models/waiting_work_wakes_once_and_removes", func(t *testing.T) {
		n := newBYOKTestNative(t)
		launches, opens, runs := 0, 0, 0
		n.autoLaunch = func(context.Context) error { launches++; return nil }
		n.autoOpenWork = func(context.Context) error { opens++; return nil }
		n.disconnectModels = func(context.Context) (kimi.DisconnectResult, error) {
			runs++
			if runs == 1 {
				return kimi.DisconnectResult{State: kimi.ImportStateWaitingWork, Reason: kimi.NativeWaitingWork}, nil
			}
			return kimi.DisconnectResult{OK: true, State: "disconnected"}, nil
		}
		if _, err := n.Action(context.Background(), core.Document{"action": "disconnect-models"}); err != nil {
			t.Fatal(err)
		}
		op := waitBYOKOperation(t, n)
		if op["state"] != "removed" {
			t.Fatalf("disconnect operation = %+v, want removed", op)
		}
		if launches != 1 || opens != 1 || runs != 2 {
			t.Fatalf("disconnect calls = launch:%d open:%d run:%d, want 1/1/2", launches, opens, runs)
		}
	})
}
