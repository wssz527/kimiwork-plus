package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

// writeRawHistory drops a history file in place without going through the log,
// which is how a hand-edited, older or partially written file looks.
func writeRawHistory(t *testing.T, dir string, payload any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, diagnosticsLogFilename)
	var data []byte
	switch value := payload.(type) {
	case string:
		data = []byte(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		data = encoded
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiagnosticsLogSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	log := newDiagnosticsLog(root)
	log.record(diagnosticEvent{Time: 1000, ID: "operation-1", Action: "connect-kimi", Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
	log.record(diagnosticEvent{Time: 2000, ID: "operation-1", Action: "connect-kimi", Phase: diagnosticPhaseFinish, State: "prepared", Stage: diagnosticStageImportDirect, DurationMs: 1000})

	summary := log.summary()
	for _, key := range []string{"available", "retained", "limit", "lastAt", "reason"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("summary is missing %q: %+v", key, summary)
		}
	}
	if len(summary) != 5 || summary["available"] != true || summary["retained"] != 2 || summary["limit"] != diagnosticsEventLimit || summary["lastAt"] != int64(2000) || summary["reason"] != "" {
		t.Fatalf("summary %+v", summary)
	}
	if strings.Contains(core.String(summary["reason"]), root) {
		t.Fatal("summary leaked the state directory")
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(log.dir)
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0700 {
			t.Fatalf("diagnostics directory mode %o", dirInfo.Mode().Perm())
		}
		fileInfo, err := os.Stat(log.path)
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != 0600 {
			t.Fatalf("diagnostics file mode %o", fileInfo.Mode().Perm())
		}
	}

	restarted := newDiagnosticsLog(root)
	events := restarted.history()
	if len(events) != 2 {
		t.Fatalf("events after restart %d", len(events))
	}
	if events[0].Phase != diagnosticPhaseStart || events[0].State != diagnosticStateRunning || events[0].Version != Version || events[0].BuildID != BuildID {
		t.Fatalf("start event after restart %+v", events[0])
	}
	if events[1].Phase != diagnosticPhaseFinish || events[1].Stage != diagnosticStageImportDirect || events[1].DurationMs != 1000 {
		t.Fatalf("finish event after restart %+v", events[1])
	}
	if restarted.summary()["retained"] != 2 {
		t.Fatal("restarted summary did not read the file")
	}

	var file diagnosticsFile
	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file.SchemaVersion != diagnosticsSchemaVersion || len(file.Events) != 2 {
		t.Fatalf("stored file %+v", file)
	}
}

func TestDiagnosticsLogRetainsLastTwoHundredEvents(t *testing.T) {
	log := newDiagnosticsLog(t.TempDir())
	for i := 1; i <= 250; i++ {
		log.record(diagnosticEvent{Time: int64(i), Action: "connect-kimi", Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
	}
	events := log.history()
	if len(events) != diagnosticsEventLimit {
		t.Fatalf("retained %d events", len(events))
	}
	if events[0].Time != 51 || events[len(events)-1].Time != 250 {
		t.Fatalf("kept window %d..%d", events[0].Time, events[len(events)-1].Time)
	}
	if log.summary()["retained"] != diagnosticsEventLimit {
		t.Fatal("summary did not report the retained window")
	}

	restarted := newDiagnosticsLog(filepath.Dir(log.dir))
	events = restarted.history()
	if len(events) != diagnosticsEventLimit || events[0].Time != 51 || events[len(events)-1].Time != 250 {
		t.Fatalf("restarted window %d..%d", events[0].Time, events[len(events)-1].Time)
	}
}

// dirtyHistory exercises every field the loader must rebuild rather than trust.
func dirtyHistory() string {
	return `{
  "schemaVersion": 1,
  "events": [
    {
      "time": 500, "id": "kept", "version": "1.1.5", "buildId": "a26ef4a",
      "action": "connect-kimi", "phase": "finish", "state": "prepared",
      "reason": "native_byok_intranet_unavailable", "stage": "import-direct", "durationMs": 12,
      "import": {"ok": true, "state": "applied", "reason": "token-abc", "models": 3,
                 "sourceId": "source-secret", "aliases": {"a": "b"},
                 "readback": {"url": "https://secret.example/v1", "credentialRef": "credential-secret"}}
    },
    {
      "time": 600, "id": "https://evil.example/path", "version": "../../etc/passwd",
      "buildId": "token-abc", "action": "https://evil.example/x", "phase": "escalated",
      "state": "bearer abc", "reason": "apiKey=sk-live-123", "stage": "/tmp/secret",
      "durationMs": -5,
      "import": {"ok": false, "state": "applied", "reason": "https://secret.example/v1",
                 "models": -2}
    },
    {
      "time": 700, "id": "future", "version": "9.9.9", "buildId": "9.9.9-abc1234",
      "action": "launch-kimi", "phase": "start", "state": "running",
      "reason": "", "stage": "", "durationMs": 0,
      "unknownSensitiveField": "cookie-secret"
    }
  ],
  "extraTopLevel": {"token": "token-abc"}
}`
}

func TestDiagnosticsLogSanitizesLoadedHistory(t *testing.T) {
	root := t.TempDir()
	writeRawHistory(t, filepath.Join(root, "diagnostics"), dirtyHistory())

	log := newDiagnosticsLog(root)
	events := log.history()
	if len(events) != 3 {
		t.Fatalf("loaded %d events", len(events))
	}
	blob, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"source-secret", "secret.example", "credential-secret", "token-abc",
		"evil.example", "etc/passwd", "sk-live-123", "/tmp/secret", "cookie-secret",
	} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("loaded history kept %q: %s", secret, blob)
		}
	}

	kept := events[0]
	if kept.Version != "1.1.5" || kept.BuildID != "a26ef4a" {
		t.Fatalf("historical version was replaced: %+v", kept)
	}
	if kept.Action != "connect-kimi" || kept.Phase != diagnosticPhaseFinish || kept.State != "prepared" || kept.Stage != diagnosticStageImportDirect || kept.DurationMs != 12 {
		t.Fatalf("safe fields were changed: %+v", kept)
	}
	if kept.Import == nil || kept.Import.State != kimi.ImportStateApplied || kept.Import.Models != 3 || kept.Import.Reason != "redacted" {
		t.Fatalf("import %+v", kept.Import)
	}

	hostile := events[1]
	if hostile.ID != "" || hostile.Version != "redacted" || hostile.BuildID != "redacted" ||
		hostile.Action != "unknown" || hostile.Phase != diagnosticPhaseUnknown ||
		hostile.State != "redacted" || hostile.Reason != "redacted" || hostile.Stage != "redacted" || hostile.DurationMs != 0 {
		t.Fatalf("hostile event was not sanitized: %+v", hostile)
	}
	if hostile.Import == nil || hostile.Import.State != kimi.ImportStateApplied || hostile.Import.Reason != "redacted" || hostile.Import.Models != 0 {
		t.Fatalf("hostile import was not sanitized: %+v", hostile.Import)
	}

	future := events[2]
	if future.Version != "9.9.9" || future.BuildID != "9.9.9-abc1234" {
		t.Fatalf("future build identity was replaced: %+v", future)
	}

	// The next write must store the sanitized form, not the raw file.
	log.record(diagnosticEvent{Time: 800, Action: "open-work", Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
	data, err := os.ReadFile(filepath.Join(root, "diagnostics", diagnosticsLogFilename))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"source-secret", "secret.example", "credential-secret", "token-abc", "evil.example", "etc/passwd"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("rewritten history kept %q", secret)
		}
	}
	if len(log.history()) != 4 {
		t.Fatalf("rewritten history lost events: %d", len(log.history()))
	}
}

func TestDiagnosticsLogHistoryIsADeepCopy(t *testing.T) {
	log := newDiagnosticsLog(t.TempDir())
	closed := false
	log.record(diagnosticEvent{Time: 100, Action: "connect-kimi", Phase: diagnosticPhaseFinish, State: "prepared",
		Import: &diagnosticImport{OK: true, State: kimi.ImportStateApplied, Closed: &closed}})

	events := log.history()
	events[0].State = "mutated"
	events[0].Import.State = "mutated"
	*events[0].Import.Closed = true
	events = append(events, diagnosticEvent{Time: 999})

	again := log.history()
	if len(again) != 1 {
		t.Fatalf("caller appended to the log: %d", len(again))
	}
	if again[0].State != "prepared" || again[0].Import.State != kimi.ImportStateApplied || (again[0].Import.Closed != nil && *again[0].Import.Closed) {
		t.Fatalf("caller mutated the log through history: %+v", again[0])
	}
}

func TestDiagnosticsLogRefusesUnsafeHistoryFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the link and permission assertions are Unix only")
	}
	real := t.TempDir()
	target := filepath.Join(real, diagnosticsLogFilename)
	if err := os.WriteFile(target, []byte(`{"schemaVersion":1,"events":[]}`), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("symlinked directory", func(t *testing.T) {
		state := t.TempDir()
		if err := os.Symlink(real, filepath.Join(state, "diagnostics")); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		log := newDiagnosticsLog(state)
		if events := log.history(); len(events) != 0 {
			t.Fatalf("symlinked directory was read: %+v", events)
		}
		if log.summary()["reason"] != diagnosticsDirectoryInvalid.Error() {
			t.Fatalf("summary %+v", log.summary())
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("the link target disappeared")
		}
	})

	t.Run("wide directory permissions", func(t *testing.T) {
		state := t.TempDir()
		dir := filepath.Join(state, "diagnostics")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, diagnosticsLogFilename), []byte(`{"schemaVersion":1,"events":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		log := newDiagnosticsLog(state)
		if log.summary()["reason"] != diagnosticsDirectoryInvalid.Error() {
			t.Fatalf("summary %+v", log.summary())
		}
	})

	t.Run("wide file permissions", func(t *testing.T) {
		state := t.TempDir()
		dir := filepath.Join(state, "diagnostics")
		path := writeRawHistory(t, dir, `{"schemaVersion":1,"events":[]}`)
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		log := newDiagnosticsLog(state)
		if log.summary()["reason"] == "" || log.summary()["available"] != false {
			t.Fatalf("summary %+v", log.summary())
		}
	})

	t.Run("regular file where the directory belongs", func(t *testing.T) {
		state := t.TempDir()
		if err := os.WriteFile(filepath.Join(state, "diagnostics"), []byte("blocked"), 0600); err != nil {
			t.Fatal(err)
		}
		log := newDiagnosticsLog(state)
		if log.summary()["reason"] != diagnosticsDirectoryInvalid.Error() {
			t.Fatalf("summary %+v", log.summary())
		}
	})
}

func TestDiagnosticsLogReadFailureKeepsHistoryAndRecovers(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "diagnostics")
	path := writeRawHistory(t, dir, `{"schemaVersion":1,"events":[{"time":10,"action":"connect-kimi","phase":"start","state":"running","version":"1.1.5","buildId":"a26ef4a"}]}`)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	log := newDiagnosticsLog(root)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	log.now = func() time.Time { return now }

	if summary := log.summary(); summary["available"] != false || summary["retained"] != 0 {
		t.Fatalf("unreadable history reported as usable: %+v", summary)
	}
	log.record(diagnosticEvent{Time: 2000, Action: "open-work", Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(original) {
		t.Fatalf("recording overwrote an unread history: %s", data)
	}
	if summary := log.summary(); summary["available"] != false || summary["retained"] != 1 {
		t.Fatalf("session events were not kept in memory: %+v", summary)
	}

	// The retry interval has not elapsed yet: still no read.
	now = now.Add(diagnosticsRetryInterval - time.Second)
	if summary := log.summary(); summary["available"] != false {
		t.Fatalf("retried before the backoff elapsed: %+v", summary)
	}

	now = now.Add(2 * time.Second)
	summary := log.summary()
	if summary["available"] != true || summary["retained"] != 2 || summary["reason"] != "" {
		t.Fatalf("recovery did not merge the history: %+v", summary)
	}
	events := log.history()
	if len(events) != 2 || events[0].Time != 10 || events[1].Time != 2000 {
		t.Fatalf("merged events %+v", events)
	}
	if events[0].Version != "1.1.5" || events[0].BuildID != "a26ef4a" {
		t.Fatalf("recovered event lost its build identity: %+v", events[0])
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted diagnosticsFile
	if err = json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Events) != 2 || persisted.Events[0].Time != 10 || persisted.Events[1].Time != 2000 {
		t.Fatalf("recovered history was not persisted: %s", data)
	}
}

func TestDiagnosticsLogRecoveryMergesWithinTheLimit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "diagnostics")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := diagnosticsFile{SchemaVersion: diagnosticsSchemaVersion}
	for i := 1; i <= 150; i++ {
		file.Events = append(file.Events, diagnosticEvent{Time: int64(i), Action: "connect-kimi", Phase: diagnosticPhaseStart, State: diagnosticStateRunning, Version: "1.1.5", BuildID: "a26ef4a"})
	}
	if err := core.AtomicJSON(filepath.Join(dir, diagnosticsLogFilename), file); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, diagnosticsLogFilename)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}

	log := newDiagnosticsLog(root)
	now := time.Unix(2000, 0)
	log.now = func() time.Time { return now }
	for i := 1; i <= 100; i++ {
		log.record(diagnosticEvent{Time: int64(1000 + i), Action: "open-work", Phase: diagnosticPhaseStart, State: diagnosticStateRunning})
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * diagnosticsRetryInterval)
	log.record(diagnosticEvent{Time: 5000, Action: "open-work", Phase: diagnosticPhaseFinish, State: "done"})

	events := log.history()
	if len(events) != diagnosticsEventLimit {
		t.Fatalf("merged %d events", len(events))
	}
	if events[0].Time != 52 || events[len(events)-1].Time != 5000 {
		t.Fatalf("merged window %d..%d", events[0].Time, events[len(events)-1].Time)
	}
}

func TestDiagnosticsLogWriteFailureDoesNotFailAction(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "diagnostics"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	n := newBYOKTestNative(t)
	n.diagnostics = newDiagnosticsLog(root)

	if _, err := n.Action(context.Background(), core.Document{"action": "prepare-native"}); err != nil {
		t.Fatalf("action failed because diagnostics could not be written: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		n.mu.Lock()
		state := core.String(n.operation["state"])
		_, hasResult := n.operation["result"]
		n.mu.Unlock()
		if state != "running" {
			if !hasResult {
				t.Fatal("operation finished without its result")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	summary := n.diagnostics.summary()
	if summary["available"] != false || summary["reason"] != diagnosticsDirectoryInvalid.Error() {
		t.Fatalf("summary %+v", summary)
	}
	if summary["retained"] != 2 {
		t.Fatalf("in-memory history lost events: %+v", summary)
	}
}

func TestNativeStatePublishesOperationLogSummary(t *testing.T) {
	n := newBYOKTestNative(t)
	settings := n.controller.Settings()
	stateDir := filepath.Join(settings.StateDir, "state")
	store, err := core.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := newIsolatedTestPolicies(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	n.service = &Service{Store: store, StateDir: stateDir, Keys: CacheKeys(testKeys{})}
	n.policies = policies
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	n.diagnostics = newDiagnosticsLog(stateDir)
	n.logOperationRejected("set-swarm", "invalid_swarm_mode")

	operationLog := core.Object(core.Object(n.State()["diagnostics"])["operationLog"])
	if operationLog == nil {
		t.Fatal("state did not publish diagnostics.operationLog")
	}
	if operationLog["available"] != true || operationLog["retained"] != 1 || operationLog["limit"] != diagnosticsEventLimit || core.String(operationLog["reason"]) != "" {
		t.Fatalf("operation log %+v", operationLog)
	}
	if len(operationLog) != 5 {
		t.Fatalf("operation log has extra fields: %+v", operationLog)
	}
	blob, err := json.Marshal(operationLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), stateDir) {
		t.Fatalf("operation log leaked a path: %s", blob)
	}
}
