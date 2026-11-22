package kimi

import (
	"archive/tar"
	"compress/gzip"
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
	if got := ProbeWorkCapabilities(installation); got.Tower.State != "unverified" || got.Tower.Reason != "work_tower_desktop_unverified" {
		t.Fatal("a future desktop version must be checked again without reporting missing kernel", got)
	}
	if err := os.WriteFile(kernel, []byte(`old kernel without confirmed capability declarations`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ProbeWorkCapabilities(installation); got.Subagents.ForceKeySupported != nil || got.Swarm.State != "unverified" || got.Swarm.Reason != "work_swarm_kernel_absent" || got.Tower.KernelAvailable || got.Tower.State != "unavailable" || got.Tower.Reason != "work_tower_kernel_absent" {
		t.Fatal(got)
	}
}

func TestWorkCapabilitiesReadPackagedCore(t *testing.T) {
	root := t.TempDir()
	resources := filepath.Join(root, "resources")
	if err := os.Mkdir(resources, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(resources, "daimon-core.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zip := gzip.NewWriter(file)
	archive := tar.NewWriter(zip)
	for name, data := range map[string]string{
		"./bundle.json":         `{"version":"1.0.14","kernelCommit":"303f4196e"}`,
		"./" + capabilityKernel: "AgentSwarm Only the user can turn on swarm mode.",
	} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got := ProbeWorkCapabilities(Installation{Archive: filepath.Join(root, "app.asar"), Version: "3.2.28"})
	if got.State != "inspected" || got.KernelVersion != "1.0.14" || got.KernelCommit != "303f4196e" || got.Swarm.State != "user-controlled" || got.Tower.State != "unavailable" || got.Tower.Reason != "work_tower_kernel_absent" {
		t.Fatal(got)
	}
}

func TestWorkCapabilitiesMissingBundleDoesNotClaimSupport(t *testing.T) {
	got := ProbeWorkCapabilities(Installation{Archive: filepath.Join(t.TempDir(), "app.asar")})
	if got.State != "unverified" || got.Subagents.ForceKeySupported != nil || got.Swarm.KernelAvailable || got.Tower.KernelAvailable {
		t.Fatal(got)
	}
}
