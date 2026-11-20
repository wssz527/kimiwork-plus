package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every writer is constructed per call, so a restart is just a second instance
// over the same state directory. These tests assert that what one instance wrote
// is still attributable, editable and reversible by the next one.

func countOccurrences(text, want string) int { return strings.Count(text, want) }

func TestContextPolicyTableSurvivesARestart(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	ratio := 0.75
	reserved := int64(8000)
	first, err := NewContextPolicyConfig(stateDir, kimiHome)
	if err != nil {
		t.Fatal(err)
	}
	if status := first.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio,
		ReservedTokens: &reserved}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if got := countOccurrences(text, "["+contextTable+"]"); got != 1 {
		t.Fatalf("%d [%s] headers, want 1:\n%s", got, contextTable, text)
	}
	if strings.Index(text, "compaction_trigger_ratio") > strings.Index(text, "reserved_context_size") {
		t.Fatalf("the keys are not in one table:\n%s", text)
	}

	// A restart must still see its own values as its own.
	second, _ := NewContextPolicyConfig(stateDir, kimiHome)
	state := second.State()
	if state.State != NativeStateFileApplied || state.Layout != "table" {
		t.Fatalf("state after restart: %+v", state)
	}
	if state.Policy == nil || state.Policy.TriggerRatio == nil || *state.Policy.TriggerRatio != ratio ||
		state.Policy.ReservedTokens == nil || *state.Policy.ReservedTokens != reserved {
		t.Fatalf("policy after restart: %+v", state.Policy)
	}

	// It must also be able to change a value the previous instance wrote.
	next := 0.85
	if status := second.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &next,
		ReservedTokens: &reserved}); status.State != NativeStateFileApplied {
		t.Fatalf("update after restart: %+v", status)
	}
	text = readConfig(t, kimiHome)
	if got := countOccurrences(text, "["+contextTable+"]"); got != 1 {
		t.Fatalf("%d [%s] headers after update, want 1:\n%s", got, contextTable, text)
	}
	if !strings.Contains(text, "compaction_trigger_ratio = 0.85") {
		t.Fatalf("the new ratio was not written:\n%s", text)
	}

	// And a third instance must be able to restore the file byte for byte.
	third, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := third.Update(ContextPolicy{Mode: contextModeInherit}); status.State != NativeStateRestored {
		t.Fatalf("release after restart: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("not restored across instances:\n%q\nwant\n%q", got, baseConfig)
	}
}

// A policy that drops a field must hand that field back on disk, or the value
// stays behind with no receipt claiming it and the next release reports it as a
// foreign value.
func TestContextPolicyTableReleasesAFieldThePolicyDropped(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	ratio := 0.75
	reserved := int64(8000)
	first, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := first.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio,
		ReservedTokens: &reserved}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	second, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := second.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio}); status.State != NativeStateFileApplied {
		t.Fatalf("apply without reserved: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if strings.Contains(text, "reserved_context_size") {
		t.Fatalf("the dropped field was left behind:\n%s", text)
	}
	third, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := third.Update(ContextPolicy{Mode: contextModeInherit}); status.State != NativeStateRestored {
		t.Fatalf("release: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("not restored:\n%q\nwant\n%q", got, baseConfig)
	}
}

func TestContextPolicyBlockSurvivesARestart(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	original := baseConfig + contextBlockBegin + "\n# owner=00000000-0000-4000-8000-000000000000 body-sha256=" +
		tomlDigest([]string{"[loop_control]", "compaction_trigger_ratio = 0.60"}) + " added-eol=0\n" +
		"[loop_control]\ncompaction_trigger_ratio = 0.60\n" + contextBlockEnd + "\n"
	writeConfig(t, kimiHome, original)
	ratio := 0.60
	first, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := first.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &ratio}); status.State != NativeStateFileApplied {
		t.Fatalf("adopt: %+v", status)
	}
	second, _ := NewContextPolicyConfig(stateDir, kimiHome)
	state := second.State()
	if state.State != NativeStateFileApplied || state.Layout != "block" {
		t.Fatalf("state after restart: %+v", state)
	}
	next := 0.70
	if status := second.Update(ContextPolicy{Mode: contextModeCustom, TriggerRatio: &next}); status.State != NativeStateFileApplied {
		t.Fatalf("update after restart: %+v", status)
	}
	text := readConfig(t, kimiHome)
	if got := countOccurrences(text, contextBlockBegin); got != 1 {
		t.Fatalf("%d blocks after update, want 1:\n%s", got, text)
	}
	if !strings.Contains(text, "compaction_trigger_ratio = 0.70") {
		t.Fatalf("the new ratio was not written:\n%s", text)
	}
	third, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := third.Update(ContextPolicy{Mode: contextModeInherit}); status.State != NativeStateRestored {
		t.Fatalf("release after restart: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("not restored across instances:\n%q\nwant\n%q", got, baseConfig)
	}
}

func TestValidReservedAcceptsZero(t *testing.T) {
	if !validReserved(0) {
		t.Fatal("zero is a value the panel allows and the released module accepts")
	}
	if validReserved(-1) {
		t.Fatal("a negative reservation is not a token count")
	}
	if !validReserved(1) {
		t.Fatal("a positive reservation must be accepted")
	}
}

func TestContextPolicyAcceptsZeroReservedTokens(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	reserved := int64(0)
	config, _ := NewContextPolicyConfig(stateDir, kimiHome)
	if status := config.Update(ContextPolicy{Mode: contextModeCustom, ReservedTokens: &reserved}); status.State != NativeStateFileApplied {
		t.Fatalf("zero reservation: %+v", status)
	}
	if !strings.Contains(readConfig(t, kimiHome), "reserved_context_size = 0") {
		t.Fatal("zero was not written")
	}
}

// ---------------------------------------------------------------------------
// services

func TestServicesSurviveARestart(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	first, _ := NewServicesConfig(stateDir, kimiHome)
	if status := first.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search", APIKey: "key-search-123"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}

	second, _ := NewServicesConfig(stateDir, kimiHome)
	state := second.State()
	if state.State != "enabled" || state.Search == nil || state.Search.State != "enabled" {
		t.Fatalf("state after restart: %+v", state)
	}

	// The saved key may be reused for the same endpoint, but only that one.
	if status := second.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://search.test/search"},
	}); status.State != NativeStateFileApplied {
		t.Fatalf("reuse after restart: %+v", status)
	}
	third, _ := NewServicesConfig(stateDir, kimiHome)
	if status := third.Update(ServicesInput{
		Search: &ServiceSettings{Enabled: true, BaseURL: "https://other.test/search"},
	}); status.State != NativeStateConflict || status.Reason != "invalid_service_key" {
		t.Fatalf("a new endpoint without a key: %+v", status)
	}

	// Disabling from a later instance must still recognise its own table.
	fourth, _ := NewServicesConfig(stateDir, kimiHome)
	if status := fourth.Update(ServicesInput{Search: &ServiceSettings{Enabled: false}}); status.State == NativeStateConflict {
		t.Fatalf("disable after restart: %+v", status)
	}
	if strings.Contains(readConfig(t, kimiHome), "[services.moonshot_search]") {
		t.Fatal("the table survived a disable from a later instance")
	}
}

// ---------------------------------------------------------------------------
// subagent

func TestSubagentPoolSurvivesARestart(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	policy := SubagentPolicy{Mode: SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a",
		Models: []SubagentPoolEntry{
			{ModelAlias: "byok-kth-kimi/kimi-a", Description: "Fast"},
			{ModelAlias: "byok-kth-kimi/kimi-b", Description: "Steady"},
		}}
	first, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := first.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	second, _ := NewSubagentConfig(stateDir, kimiHome)
	state := second.Inspect()
	if state.State != NativeStateFileApplied || state.Mode != SubagentModePool {
		t.Fatalf("inspect after restart: %+v", state)
	}
	// Rewriting the same policy from a new instance must not be a conflict.
	if status := second.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("re-apply after restart: %+v", status)
	}
	// A block a newer instance wrote is never deleted by an older one, and a
	// restarted instance that has not yet written is in that position.
	third, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := third.Update(SubagentPolicy{Mode: SubagentModeInherit}, SubagentCapabilities{}); status.State != NativeStateConflict ||
		status.Reason != "ownership_superseded" {
		t.Fatalf("a stale instance must not revoke: %+v", status)
	}
	if got := readConfig(t, kimiHome); !strings.Contains(got, subagentBlockBegin) {
		t.Fatal("a stale instance removed the block")
	}
	// Taking the block over is what makes it this instance's to remove.
	fourth, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := fourth.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("takeover: %+v", status)
	}
	if status := fourth.Update(SubagentPolicy{Mode: SubagentModeInherit}, SubagentCapabilities{}); status.State != SubagentModeInherit {
		t.Fatalf("inherit after takeover: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("not restored across instances:\n%q\nwant\n%q", got, baseConfig)
	}
}

func TestSubagentForceSurvivesARestart(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	policy := SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a", Effort: "high"}
	first, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := first.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	second, _ := NewSubagentConfig(stateDir, kimiHome)
	if state := second.Inspect(); state.State != NativeStateFileApplied || state.Mode != SubagentModeForce {
		t.Fatalf("inspect after restart: %+v", state)
	}
	if status := second.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("re-apply after restart: %+v", status)
	}
	if got := readConfig(t, kimiHome); countOccurrences(got, subagentBlockBegin) != 1 {
		t.Fatalf("the block was duplicated:\n%s", got)
	}
}

// A write that recorded the block it replaced but never reached the file leaves
// the receipt describing a block that is not on disk. That is recoverable, not a
// conflict, and the disable path still must not delete the block it cannot claim.
func TestSubagentRecoversAReceiptThatOutranTheFile(t *testing.T) {
	kimiHome, stateDir := nativeHome(t)
	writeConfig(t, kimiHome, baseConfig)
	policy := SubagentPolicy{Mode: SubagentModeForce, ModelAlias: "byok-kth-kimi/kimi-a"}
	first, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := first.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("apply: %+v", status)
	}
	afterApply := readConfig(t, kimiHome)

	// Simulate the crash: the receipt names a new block and records the block
	// still on disk as the one it replaced, but the config never changed.
	scan := scanConfig(afterApply)
	if scan.block == nil {
		t.Fatal("no managed block to interrupt")
	}
	receiptPath := filepath.Join(stateDir, subagentReceipt)
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt subagentReceiptFile
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	interrupted := subagentReceiptFile{SchemaVersion: 1, OwnerID: receipt.OwnerID, HomeDir: receipt.HomeDir,
		Managed: receipt.Managed, BlockHash: strings.Repeat("a", 64), Mode: SubagentModePool,
		Previous: &subagentBlockRef{OwnerID: scan.block.ownerID, BlockHash: scan.block.digest}}
	encoded, err := json.Marshal(&interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	second, _ := NewSubagentConfig(stateDir, kimiHome)
	state := second.Inspect()
	if state.State != NativeStateWaiting || state.Reason != "handoff_pending" {
		t.Fatalf("inspect on a recoverable receipt: %+v", state)
	}
	// The journalled block is this module's own, so an explicit inherit removes
	// it rather than reporting a conflict the user cannot resolve.
	if status := second.Update(SubagentPolicy{Mode: SubagentModeInherit}, SubagentCapabilities{}); status.State != SubagentModeInherit {
		t.Fatalf("inherit over a recoverable block: %+v", status)
	}
	if got := readConfig(t, kimiHome); got != baseConfig {
		t.Fatalf("the journalled block was not removed:\n%q", got)
	}
	// And a write over a recoverable block adopts it without duplicating it.
	third, _ := NewSubagentConfig(stateDir, kimiHome)
	if status := third.Update(policy, SubagentCapabilities{}); status.State != NativeStateFileApplied {
		t.Fatalf("write over a recoverable block: %+v", status)
	}
	if got := readConfig(t, kimiHome); countOccurrences(got, subagentBlockBegin) != 1 {
		t.Fatalf("the takeover duplicated the block:\n%s", got)
	}
}

func preserveRestartProbes(t *testing.T) {
	t.Helper()
	list := listOfficialProcesses
	identity := officialProcessIdentity
	selectTargets := selectQuitTargets
	quit := requestOfficialQuit
	t.Cleanup(func() {
		listOfficialProcesses = list
		officialProcessIdentity = identity
		selectQuitTargets = selectTargets
		requestOfficialQuit = quit
	})
}

func TestStopOfficialProcessesRequestsNormalQuitAndWaits(t *testing.T) {
	preserveRestartProbes(t)
	alive := true
	quitCalls := 0
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) { return []int{42}, true, "" }
	officialProcessIdentity = func(context.Context, int) ProcessIdentity {
		if !alive {
			return ProcessIdentity{Kind: IdentityNotFound}
		}
		return ProcessIdentity{Kind: IdentityDarwinLStart, Value: "start-42"}
	}
	requestOfficialQuit = func(context.Context, int) error {
		quitCalls++
		alive = false
		return nil
	}
	if err := stopOfficialProcesses(context.Background(), "/Applications/Kimi.app/Contents/MacOS/Kimi"); err != nil {
		t.Fatal(err)
	}
	if quitCalls != 1 {
		t.Fatalf("quit calls = %d", quitCalls)
	}
}

func TestStopOfficialProcessesRefusesUnknownIdentity(t *testing.T) {
	preserveRestartProbes(t)
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) { return []int{42}, true, "" }
	officialProcessIdentity = func(context.Context, int) ProcessIdentity { return ProcessIdentity{Kind: IdentityUnknown} }
	quitCalls := 0
	requestOfficialQuit = func(context.Context, int) error { quitCalls++; return nil }
	if err := stopOfficialProcesses(context.Background(), "/Kimi"); !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("error = %v", err)
	}
	if quitCalls != 0 {
		t.Fatal("quit was requested without a stable identity")
	}
}

func TestStopOfficialProcessesAllowsPIDReuse(t *testing.T) {
	preserveRestartProbes(t)
	calls := 0
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) { return []int{42}, true, "" }
	officialProcessIdentity = func(context.Context, int) ProcessIdentity {
		calls++
		if calls == 1 {
			return ProcessIdentity{Kind: IdentityDarwinLStart, Value: "original"}
		}
		return ProcessIdentity{Kind: IdentityDarwinLStart, Value: "replacement"}
	}
	requestOfficialQuit = func(context.Context, int) error { return nil }
	if err := stopOfficialProcesses(context.Background(), "/Kimi"); err != nil {
		t.Fatal(err)
	}
}

func TestStopOfficialProcessesReportsTimeout(t *testing.T) {
	preserveRestartProbes(t)
	ctx, cancel := context.WithCancel(context.Background())
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) { return []int{42}, true, "" }
	officialProcessIdentity = func(context.Context, int) ProcessIdentity {
		return ProcessIdentity{Kind: IdentityDarwinLStart, Value: "start-42"}
	}
	requestOfficialQuit = func(context.Context, int) error { cancel(); return nil }
	if err := stopOfficialProcesses(ctx, "/Kimi"); err == nil || err.Error() != "kimi_quit_timeout" {
		t.Fatalf("error = %v", err)
	}
}

func TestStopOfficialProcessesQuitsSelectedRootsAndWaitsForChildren(t *testing.T) {
	preserveRestartProbes(t)
	alive := map[int]int{100: 1, 201: 2, 202: 2}
	var quitPIDs []int
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) {
		return []int{100, 201, 202}, true, ""
	}
	selectQuitTargets = func(pids []int) []int { return []int{100} }
	officialProcessIdentity = func(_ context.Context, pid int) ProcessIdentity {
		remaining := alive[pid]
		if remaining <= 0 {
			return ProcessIdentity{Kind: IdentityNotFound}
		}
		return ProcessIdentity{Kind: IdentityDarwinLStart, Value: "start-" + string(rune('0'+pid%10))}
	}
	requestOfficialQuit = func(_ context.Context, pid int) error {
		quitPIDs = append(quitPIDs, pid)
		if pid != 100 {
			return errors.New("kimi_quit_refused")
		}
		alive[100] = 0
		alive[201] = 0
		alive[202] = 0
		return nil
	}
	if err := stopOfficialProcesses(context.Background(), `C:\Program Files\Kimi\Kimi.exe`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quitPIDs) != 1 || quitPIDs[0] != 100 {
		t.Fatalf("quit requested for %v, want [100]", quitPIDs)
	}
}
