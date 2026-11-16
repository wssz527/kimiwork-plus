package kimi

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

type WorkCapability struct {
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	KernelAvailable bool   `json:"kernelAvailable"`
}

type WorkCapabilities struct {
	State         string               `json:"state"`
	KernelVersion string               `json:"kernelVersion,omitempty"`
	KernelCommit  string               `json:"kernelCommit,omitempty"`
	Subagents     SubagentCapabilities `json:"subagents"`
	Swarm         WorkCapability       `json:"swarm"`
	Tower         WorkCapability       `json:"tower"`
}

func ProbeWorkCapabilities(installation Installation) WorkCapabilities {
	result := WorkCapabilities{State: "unverified", Swarm: WorkCapability{State: "unverified", Reason: "work_kernel_unverified"}, Tower: WorkCapability{State: "unverified", Reason: "work_kernel_unverified"}}
	bundle := filepath.Join(filepath.Dir(installation.Archive), "resources", "daimon-bundle")
	manifest, err := readCapabilityFile(filepath.Join(bundle, "bundle.json"), 64<<10)
	if err != nil {
		return result
	}
	var metadata struct {
		Version      string `json:"version"`
		KernelCommit string `json:"kernelCommit"`
	}
	if json.Unmarshal(manifest, &metadata) != nil || metadata.Version == "" || len(metadata.Version) > 128 || len(metadata.KernelCommit) > 128 {
		return result
	}
	kernel, err := readCapabilityFile(filepath.Join(bundle, "app", "daimon", "dist", "kimi-code-v2", "kernel-bundle.mjs"), 16<<20)
	if err != nil {
		return result
	}
	result.State = "inspected"
	result.KernelVersion = metadata.Version
	result.KernelCommit = metadata.KernelCommit
	if bytes.Contains(kernel, []byte("SECONDARY_MODEL_FORCE_REMOVED_MESSAGE")) && bytes.Contains(kernel, []byte("force' is no longer supported and is ignored")) {
		unsupported := false
		result.Subagents.ForceKeySupported = &unsupported
	}
	result.Swarm.KernelAvailable = bytes.Contains(kernel, []byte("Only the user can turn on swarm mode.")) && bytes.Contains(kernel, []byte("AgentSwarm"))
	if result.Swarm.KernelAvailable {
		result.Swarm.State = "user-controlled"
		result.Swarm.Reason = "work_swarm_requires_conversation"
	}
	result.Tower.KernelAvailable = bytes.Contains(kernel, []byte("TOWER_MODE_USER_ENABLED_ONLY")) && bytes.Contains(kernel, []byte("TowerFeature"))
	if result.Tower.KernelAvailable && installation.Version == "3.2.27" && metadata.KernelCommit == "ecb904a69" {
		result.Tower.State = "unsupported"
		result.Tower.Reason = "work_tower_desktop_unavailable"
	}
	return result
}

func readCapabilityFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, ErrRendererUnconfirmed
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrRendererUnconfirmed
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, ErrRendererUnconfirmed
	}
	return data, nil
}
