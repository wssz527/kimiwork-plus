package kimi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkCapabilitiesFollowActualKernelContract(t *testing.T) {
	resources := t.TempDir()
	bundle := filepath.Join(resources, "resources", "daimon-bundle")
	kernel := filepath.Join(bundle, "app", "daimon", "dist", "kimi-code-v2", "kernel-bundle.mjs")
	if err := os.MkdirAll(filepath.Dir(kernel), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "bundle.json"), []byte(`{"version":"1.0.10","kernelCommit":"ecb904a69"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel, []byte(`SECONDARY_MODEL_FORCE_REMOVED_MESSAGE force' is no longer supported and is ignored AgentSwarm Only the user can turn on swarm mode. TOWER_MODE_USER_ENABLED_ONLY TowerFeature`), 0600); err != nil {
		t.Fatal(err)
	}
	installation := Installation{Archive: filepath.Join(resources, "app.asar"), Version: "3.2.27"}
	got := ProbeWorkCapabilities(installation)
	if got.State != "inspected" || got.KernelVersion != "1.0.10" || got.Subagents.ForceKeySupported == nil || *got.Subagents.ForceKeySupported || got.Swarm.State != "user-controlled" || got.Tower.State != "unsupported" {
		t.Fatal(got)
	}
	installation.Version = "3.3.0"
	if got := ProbeWorkCapabilities(installation); got.Tower.State != "unverified" {
		t.Fatal("a future desktop version must be checked again", got)
	}
	if err := os.WriteFile(kernel, []byte(`old kernel without confirmed capability declarations`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ProbeWorkCapabilities(installation); got.Subagents.ForceKeySupported != nil || got.Swarm.State != "unverified" || got.Tower.KernelAvailable {
		t.Fatal(got)
	}
}

func TestWorkCapabilitiesMissingBundleDoesNotClaimSupport(t *testing.T) {
	got := ProbeWorkCapabilities(Installation{Archive: filepath.Join(t.TempDir(), "app.asar")})
	if got.State != "unverified" || got.Subagents.ForceKeySupported != nil || got.Swarm.KernelAvailable || got.Tower.KernelAvailable {
		t.Fatal(got)
	}
}
