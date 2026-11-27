package kimi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func buildTestAsar(t *testing.T, files map[string]string) string {
	t.Helper()
	type fileEntry struct {
		path string
		body []byte
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	names = SortedIDs(names)

	root := map[string]any{"files": map[string]any{}}
	var payload []byte
	for _, name := range names {
		body := []byte(files[name])
		offset := len(payload)
		payload = append(payload, body...)
		parts := splitSlash(name)
		cur := root
		for i, part := range parts {
			dir := cur["files"].(map[string]any)
			if i == len(parts)-1 {
				dir[part] = map[string]any{
					"size":   len(body),
					"offset": json.Number(intToString(offset)),
				}
			} else {
				next, ok := dir[part].(map[string]any)
				if !ok {
					next = map[string]any{"files": map[string]any{}}
					dir[part] = next
				}
				cur = next
			}
		}
	}

	headerJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	// Pad header JSON to 4-byte boundary as asar pickle expects.
	pad := (4 - (len(headerJSON) % 4)) % 4
	padded := append(headerJSON, make([]byte, pad)...)
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], 4)
	binary.LittleEndian.PutUint32(header[4:8], uint32(8+len(padded)))
	binary.LittleEndian.PutUint32(header[8:12], uint32(4+len(padded)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(headerJSON)))

	dir := t.TempDir()
	path := filepath.Join(dir, "app.asar")
	out := append(header, padded...)
	out = append(out, payload...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func splitSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func TestInspectStoreFormat(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		got := InspectStoreFormat(dir)
		if got.Status != CheckStatusAbsent || got.Format != StoreFormatAbsent {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("profiles", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, NativeWorkStore), []byte(`{"v":2,"profiles":{}}`), 0o600)
		got := InspectStoreFormat(dir)
		if got.Status != CheckStatusPass || got.Format != StoreFormatProfiles {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("accounts", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, NativeWorkStore), []byte(`{"v":3,"accounts":{}}`), 0o600)
		got := InspectStoreFormat(dir)
		if got.Status != CheckStatusPass || got.Format != StoreFormatAccounts {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, NativeWorkStore), []byte(`{"v":4,"registries":[]}`), 0o600)
		got := InspectStoreFormat(dir)
		if got.Status != CheckStatusFail || got.Format != StoreFormatUnknown {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, NativeWorkStore), []byte(`{bad json`), 0o600)
		got := InspectStoreFormat(dir)
		if got.Status != CheckStatusFail || got.Format != StoreFormatInvalid {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestCheckCompatibilityVerdictsAndGuards(t *testing.T) {
	verifiedContract := &ContractResult{
		State:      StateVerified,
		Reason:     "models_accepted",
		ModelCount: 5,
		Accepted:   []string{"a", "b", "c", "d", "e"},
		Version:    "3.2.28",
		SHA256:     "abc123",
	}

	t.Run("verified version passes", func(t *testing.T) {
		contract := *verifiedContract
		contract.Version = "3.2.29"
		archive := buildTestAsar(t, map[string]string{
			"package.json":                       `{"version":"3.2.29"}`,
			"out/preload/preload-kimi-agent.mjs": `contextBridge.exposeInMainWorld("kimiAgentAPI",{getUserProfile:()=>{},byokSources:{list:()=>{}}})`,
			"out/preload/preload-claw.mjs":       `contextBridge.exposeInMainWorld("clawBridgeAPI",{byokSources:{list:()=>{},preview:()=>{},save:()=>{}}})`,
		})
		workHome := t.TempDir()
		_ = os.WriteFile(filepath.Join(workHome, NativeWorkStore), []byte(`{"v":3,"accounts":{}}`), 0o600)

		report, _ := CheckCompatibility(context.Background(), CompatibilityOptions{
			Installation: Installation{Archive: archive, Version: "3.2.29"},
			WorkHome:     workHome,
			Contract:     &contract,
			Capabilities: &WorkCapabilities{KernelVersion: "1.0.14", KernelCommit: "303f4196e"},
			Previous:     &OfficialObservation{OfficialVersion: "3.2.27", KernelCommit: "ecb904a69"},
		})
		if report.Status != CompatStatusVerified || !report.VerifiedVersion || report.Summary != "兼容" {
			t.Fatalf("unexpected report: %+v", report)
		}
		if !report.VersionChanged || report.PreviousVersion != "3.2.27" || report.PreviousKernelCommit != "ecb904a69" {
			t.Fatalf("expected version change recorded: %+v", report)
		}
		if report.Guards.ModelSyncWriteBlocked || report.Guards.AccountSwitchWriteBlocked {
			t.Fatalf("unexpected write guards: %+v", report.Guards)
		}
		if report.Checks.StoreFormat.Format != StoreFormatAccounts {
			t.Fatalf("expected accounts store format, got %+v", report.Checks.StoreFormat)
		}
	})

	t.Run("unlisted new version passes basic checks", func(t *testing.T) {
		archive := buildTestAsar(t, map[string]string{
			"package.json":                       `{"version":"3.2.99"}`,
			"out/preload/preload-kimi-agent.mjs": `contextBridge.exposeInMainWorld("kimiAgentAPI",{getUserProfile:()=>{},byokSources:{list:()=>{}}})`,
			"out/preload/preload-claw.mjs":       `contextBridge.exposeInMainWorld("clawBridgeAPI",{byokSources:{list:()=>{},preview:()=>{},save:()=>{}}})`,
		})
		newContract := *verifiedContract
		newContract.Version = "3.2.99"
		report, _ := CheckCompatibility(context.Background(), CompatibilityOptions{
			Installation: Installation{Archive: archive, Version: "3.2.99"},
			WorkHome:     t.TempDir(),
			Contract:     &newContract,
		})
		if report.Status != CompatStatusNewVersionPass || report.Summary != "新版本，自检兼容" {
			t.Fatalf("unexpected report: %+v", report)
		}
		if report.VerifiedVersion || report.Guards.ModelSyncWriteBlocked || report.Guards.AccountSwitchWriteBlocked {
			t.Fatalf("unexpected flags/guards: %+v", report)
		}
	})

	t.Run("catalog failure blocks model sync only", func(t *testing.T) {
		archive := buildTestAsar(t, map[string]string{
			"package.json":                       `{"version":"3.3.0"}`,
			"out/preload/preload-kimi-agent.mjs": `contextBridge.exposeInMainWorld("kimiAgentAPI",{getUserProfile:()=>{},byokSources:{list:()=>{}}})`,
			"out/preload/preload-claw.mjs":       `contextBridge.exposeInMainWorld("clawBridgeAPI",{byokSources:{list:()=>{},preview:()=>{},save:()=>{}}})`,
		})
		failedContract := &ContractResult{State: StateIncompatible, Reason: "models_skipped", Version: "3.3.0"}
		report, _ := CheckCompatibility(context.Background(), CompatibilityOptions{
			Installation: Installation{Archive: archive, Version: "3.3.0"},
			WorkHome:     t.TempDir(),
			Contract:     failedContract,
		})
		if report.Status != CompatStatusDegraded || report.Summary != "模型同步不可用" {
			t.Fatalf("unexpected report: %+v", report)
		}
		if !report.Guards.ModelSyncWriteBlocked || report.Guards.AccountSwitchWriteBlocked {
			t.Fatalf("expected only model sync blocked: %+v", report.Guards)
		}
	})

	t.Run("missing getUserProfile blocks account switch only", func(t *testing.T) {
		archive := buildTestAsar(t, map[string]string{
			"package.json":                       `{"version":"3.3.0"}`,
			"out/preload/preload-kimi-agent.mjs": `contextBridge.exposeInMainWorld("kimiAgentAPI",{byokSources:{list:()=>{}}})`,
			"out/preload/preload-claw.mjs":       `contextBridge.exposeInMainWorld("clawBridgeAPI",{byokSources:{list:()=>{},preview:()=>{},save:()=>{}}})`,
		})
		c := *verifiedContract
		c.Version = "3.3.0"
		report, _ := CheckCompatibility(context.Background(), CompatibilityOptions{
			Installation: Installation{Archive: archive, Version: "3.3.0"},
			WorkHome:     t.TempDir(),
			Contract:     &c,
		})
		if report.Status != CompatStatusDegraded || report.Summary != "账号切换不可用" {
			t.Fatalf("unexpected report: %+v", report)
		}
		if report.Guards.ModelSyncWriteBlocked || !report.Guards.AccountSwitchWriteBlocked {
			t.Fatalf("expected only account switch blocked: %+v", report.Guards)
		}
	})

	t.Run("unknown store format blocks model sync only", func(t *testing.T) {
		archive := buildTestAsar(t, map[string]string{
			"package.json":                       `{"version":"3.2.28"}`,
			"out/preload/preload-kimi-agent.mjs": `contextBridge.exposeInMainWorld("kimiAgentAPI",{getUserProfile:()=>{},getByokProfile:()=>{}})`,
			"out/preload/preload-claw.mjs":       `contextBridge.exposeInMainWorld("clawBridgeAPI",{importByokModels:()=>{}})`,
		})
		workHome := t.TempDir()
		_ = os.WriteFile(filepath.Join(workHome, NativeWorkStore), []byte(`{"v":4,"other":{}}`), 0o600)
		report, _ := CheckCompatibility(context.Background(), CompatibilityOptions{
			Installation: Installation{Archive: archive, Version: "3.2.28"},
			WorkHome:     workHome,
			Contract:     verifiedContract,
		})
		if report.Status != CompatStatusDegraded || report.Summary != "模型同步不可用" {
			t.Fatalf("unexpected report: %+v", report)
		}
		if !report.Guards.ModelSyncWriteBlocked || report.Guards.AccountSwitchWriteBlocked {
			t.Fatalf("expected only model sync blocked: %+v", report.Guards)
		}
	})

	t.Run("observation persistence round trip", func(t *testing.T) {
		stateDir := t.TempDir()
		sample := CompatibilityReport{
			Status:          CompatStatusVerified,
			OfficialVersion: "3.2.28",
			KernelVersion:   "1.0.14",
			KernelCommit:    "303f4196e",
			ArchiveSHA256:   "deadbeef",
			CheckedAt:       123456,
			Checks:          CompatibilityChecks{StoreFormat: StoreFormatCompatCheck{Format: StoreFormatAccounts}},
		}
		if err := SaveOfficialObservation(stateDir, sample); err != nil {
			t.Fatal(err)
		}
		got := LoadOfficialObservation(stateDir)
		if got == nil || got.OfficialVersion != "3.2.28" || got.KernelCommit != "303f4196e" || got.StoreFormat != StoreFormatAccounts {
			t.Fatalf("unexpected loaded observation: %+v", got)
		}
	})
}

func TestInstalledCompatibilityReadOnly(t *testing.T) {
	if os.Getenv("KIMI_SKIP_LIVE") == "1" {
		t.Skip("live probe disabled")
	}
	archive := firstExisting(ArchiveCandidates(runtime.GOOS, environMap()))
	if archive == "" {
		t.Skip("no Kimi installation on this host")
	}
	if _, err := os.Stat(archive); err != nil {
		t.Skip("archive not readable")
	}
	profileDir, _ := OfficialProfileDirectory()
	workHome, _ := ResolveOfficialKimiHome(profileDir, "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	install := Installation{Archive: archive, Version: InstalledVersion(archive), Platform: runtime.GOOS}
	caps := ProbeWorkCapabilities(install)
	report, _ := CheckCompatibility(ctx, CompatibilityOptions{
		Installation: install,
		WorkHome:     workHome,
		Capabilities: &caps,
	})
	t.Logf("compat status=%s summary=%s version=%s verified=%v kernel=%s/%s catalog=%s api=%+v store=%+v guards=%+v",
		report.Status, report.Summary, report.OfficialVersion, report.VerifiedVersion,
		report.KernelVersion, report.KernelCommit, report.Checks.Catalog.Status,
		report.Checks.AccountAPI, report.Checks.StoreFormat, report.Guards)
	if report.Status == CompatStatusUnavailable {
		t.Fatalf("installed Kimi reported unavailable: %+v", report)
	}
}
