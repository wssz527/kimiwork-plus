package bridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
)

type countingKeys struct {
	mu      sync.Mutex
	values  map[string]string
	getByID map[string]int
}

func (k *countingKeys) Get(name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.getByID == nil {
		k.getByID = map[string]int{}
	}
	k.getByID[name]++
	return k.values[name], nil
}

func (k *countingKeys) count(name string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.getByID[name]
}

func (k *countingKeys) resetCounts() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.getByID = map[string]int{}
}

func (k *countingKeys) Set(name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.values[name] = value
	return nil
}

func (k *countingKeys) Delete(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.values, name)
	return nil
}

func TestServiceStateSkipsRedundantConnectionScanWithGateway(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	keys := &countingKeys{values: map[string]string{"conn-ref-1": "secret-1"}}
	if _, _, err := store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{
				"id":        "local",
				"name":      "Local",
				"baseURL":   "http://127.0.0.1:9999/v1",
				"apiKeyRef": "conn-ref-1",
			},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rt, err := NewRuntime(dir, store, keys)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, Keys: keys, StateDir: dir, Gateway: rt}

	keys.resetCounts()
	state, err := s.State()
	if err != nil {
		t.Fatal(err)
	}
	// Runtime.State scans active + saved once each (2 calls); Service.State must
	// not add a redundant third scan on top of it.
	if got := keys.count("conn-ref-1"); got != 2 {
		t.Fatalf("conn-ref-1 Get calls with Gateway = %d, want 2", got)
	}
	if len(core.Rows(core.Object(state["connections"])["saved"])) != 1 {
		t.Fatalf("connections missing from State(): %+v", state["connections"])
	}

	// Without Gateway, Service.State still scans connections once.
	s.SetGateway(nil)
	keys.resetCounts()
	state, err = s.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := keys.count("conn-ref-1"); got != 1 {
		t.Fatalf("conn-ref-1 Get calls without Gateway = %d, want 1", got)
	}
	if len(core.Rows(core.Object(state["connections"])["saved"])) != 1 {
		t.Fatalf("connections missing without Gateway: %+v", state["connections"])
	}
}

func TestWorkUsageCollectorLazySnapshotAndTruncationCursorRetention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	kimiHome := filepath.Join(t.TempDir(), "kimi-home")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	nowMS := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC).UnixMilli()
	usageLedger, err := ledger.New(ledger.Options{
		File: filepath.Join(dir, "usage", "ledger.jsonl"),
		Now:  func() int64 { return nowMS },
	})
	if err != nil {
		t.Fatal(err)
	}

	var snapshotCalls atomic.Int32
	collector := newWorkUsageCollector(dir, kimiHome, usageLedger, store, nil)
	collector.now = func() int64 { return nowMS }
	collector.snapshotGateway = func(l *ledger.Ledger) []gatewayDedupRecord {
		snapshotCalls.Add(1)
		return snapshotGatewayRecords(l)
	}

	writeWire := func(conv, content string) string {
		p := filepath.Join(kimiHome, "runtime", "kimi-code", "home", "sessions", "wd_1", conv, "agents", "main", "wire.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Non-usage lines must NOT construct gateway dedup snapshot.
	writeWire("conv-a", `{"type":"context.append_message","payload":{"role":"user","content":"hello"}}`+"\n")
	writeWire("conv-b", `{"type":"step.begin","payload":{}}`+"\n")
	if got := collector.Sync(); got != 0 {
		t.Fatalf("non-usage Sync() imported %d, want 0", got)
	}
	if got := snapshotCalls.Load(); got != 0 {
		t.Fatalf("snapshotGateway called %d times for non-usage lines, want 0", got)
	}

	// Usage lines across two files must construct gateway dedup snapshot exactly once.
	usageLine := `{"type":"usage.record","agentId":"main","model":"k3-agent","usage":{"inputOther":100,"output":20,"inputCacheRead":0,"inputCacheCreation":0},"usageScope":"turn","time":` + int64Str(nowMS-2000) + `}` + "\n"
	writeWire("conv-a", `{"type":"context.append_message","payload":{"role":"user","content":"hello"}}`+"\n"+usageLine)
	writeWire("conv-b", `{"type":"step.begin","payload":{}}`+"\n"+usageLine)
	writeWire("conv-c", `{"type":"step.begin","payload":{}}`+"\n"+usageLine)
	if got := collector.Sync(); got != 3 {
		t.Fatalf("usage Sync() imported %d, want 3", got)
	}
	if got := snapshotCalls.Load(); got != 1 {
		t.Fatalf("snapshotGateway called %d times across 3 files, want 1", got)
	}

	// When discovery hits maxFiles truncation, cursors for unvisited files must NOT be pruned.
	homePrefix := shortHomeTag(kimiHome) + ":"
	keyC := homePrefix + "wd_1/conv-c/agents/main/wire.jsonl"
	if _, ok := collector.cursor.Files[keyC]; !ok {
		t.Fatalf("expected cursor for %q", keyC)
	}
	collector.maxFiles = 2
	collector.Sync()
	if _, ok := collector.cursor.Files[keyC]; !ok {
		t.Fatalf("cursor for %q was deleted while discovery was truncated", keyC)
	}

	// Once discovery is not truncated and conv-c is genuinely removed, its cursor IS pruned.
	collector.maxFiles = workUsageMaxFiles
	if err := os.Remove(filepath.Join(kimiHome, "runtime", "kimi-code", "home", "sessions", "wd_1", "conv-c", "agents", "main", "wire.jsonl")); err != nil {
		t.Fatal(err)
	}
	collector.Sync()
	if _, ok := collector.cursor.Files[keyC]; ok {
		t.Fatalf("deleted file cursor %q was not pruned after complete discovery", keyC)
	}
}

func TestNativeWatchStepConcurrentWithState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	controller, err := kimi.NewController(kimi.ControllerSettings{
		StateDir:    dir,
		UserDataDir: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	home, err := controller.WorkHome()
	if err != nil {
		t.Fatal(err)
	}
	policies, err := newNativePolicies(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, StateDir: dir}
	n := &Native{
		service:       s,
		controller:    controller,
		policies:      policies,
		process:       core.Document{"running": nil, "state": "unknown"},
		compatibility: initialCompatibilityReport(),
		diagnostics:   newDiagnosticsLog(dir),
	}
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	defer n.mcp.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var observed os.FileInfo
		for range 5 {
			observed = n.watchStep(ctx, observed)
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			_ = n.State()
		}
	}()
	wg.Wait()
}

func TestStaticHostsAvailableChecksDarwinResolverAbsence(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only resolver check")
	}
	tmp := t.TempDir()
	hostsPath := filepath.Join(tmp, "hosts")
	resolverPath := filepath.Join(tmp, "dev.msh.team")
	content := byokdns.HostsMarker + "\n" + byokdns.HostsEntry() + "\n"
	if err := os.WriteFile(hostsPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !staticHostsAvailable(hostsPath, resolverPath, nil) {
		t.Fatal("expected staticHostsAvailable=true when managed hosts exists and legacy resolver is absent")
	}
	if err := os.WriteFile(resolverPath, []byte("nameserver 127.0.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if staticHostsAvailable(hostsPath, resolverPath, nil) {
		t.Fatal("expected staticHostsAvailable=false while legacy /etc/resolver entry still exists")
	}
}

func TestShortHomeTagWindowsCaseInsensitive(t *testing.T) {
	a := shortHomeTagForOS("windows", `C:\Users\Alice\Kimi`)
	b := shortHomeTagForOS("windows", `c:\users\alice\kimi`)
	if a != b {
		t.Fatalf("windows shortHomeTagForOS differed: %q vs %q", a, b)
	}
}
