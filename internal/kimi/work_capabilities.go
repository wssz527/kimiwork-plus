package kimi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

const capabilityKernel = "app/daimon/dist/kimi-code-v2/kernel-bundle.mjs"

func ProbeWorkCapabilities(installation Installation) WorkCapabilities {
	result := WorkCapabilities{State: "unverified", Swarm: WorkCapability{State: "unverified", Reason: "work_kernel_unverified"}, Tower: WorkCapability{State: "unverified", Reason: "work_kernel_unverified"}}
	if installation.Archive == "" {
		return result
	}
	manifest, kernel, err := readCapabilityBundle(filepath.Join(filepath.Dir(installation.Archive), "resources"))
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

func readCapabilityBundle(resources string) (manifest, kernel []byte, err error) {
	const manifestLimit, kernelLimit = 64 << 10, 16 << 20
	bundle := filepath.Join(resources, "daimon-bundle")
	manifest, err = readCapabilityFile(filepath.Join(bundle, "bundle.json"), manifestLimit)
	if err == nil {
		kernel, err = readCapabilityFile(filepath.Join(bundle, filepath.FromSlash(capabilityKernel)), kernelLimit)
		return manifest, kernel, err
	}
	if !os.IsNotExist(err) {
		return nil, nil, err
	}

	const archiveLimit = 96 << 20
	file, err := openCapabilityFile(filepath.Join(resources, "daimon-core.tar.gz"), archiveLimit)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(io.LimitReader(file, archiveLimit))
	if err != nil {
		return nil, nil, err
	}
	defer gzipReader.Close()
	archive := tar.NewReader(io.LimitReader(gzipReader, archiveLimit))
	for manifest == nil || kernel == nil {
		entry, err := archive.Next()
		if err != nil {
			return nil, nil, err
		}
		var target *[]byte
		var limit int64
		switch entry.Name {
		case "bundle.json", "./bundle.json":
			target, limit = &manifest, manifestLimit
		case capabilityKernel, "./" + capabilityKernel:
			target, limit = &kernel, kernelLimit
		default:
			continue
		}
		if *target != nil || entry.Typeflag != tar.TypeReg || entry.Size <= 0 || entry.Size > limit {
			return nil, nil, ErrRendererUnconfirmed
		}
		*target, err = io.ReadAll(archive)
		if err != nil {
			return nil, nil, err
		}
	}
	return manifest, kernel, nil
}

func readCapabilityFile(path string, maximum int64) ([]byte, error) {
	file, err := openCapabilityFile(path, maximum)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, ErrRendererUnconfirmed
	}
	return data, nil
}

func openCapabilityFile(path string, maximum int64) (*os.File, error) {
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
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, ErrRendererUnconfirmed
	}
	return file, nil
}
