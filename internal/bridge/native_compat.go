package bridge

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/kimi"
)

func initialCompatibilityReport() kimi.CompatibilityReport {
	return kimi.CompatibilityReport{
		Status:           kimi.CompatStatusChecking,
		Summary:          "正在检查官方 Kimi 兼容性",
		VerifiedVersions: append([]string(nil), kimi.VerifiedOfficialVersions...),
		AffectedFeatures: []kimi.AffectedFeature{},
	}
}

func (n *Native) compatibilitySnapshot() kimi.CompatibilityReport {
	report := n.compatibility
	if report.VerifiedVersions == nil {
		report.VerifiedVersions = append([]string(nil), kimi.VerifiedOfficialVersions...)
	}
	if report.AffectedFeatures == nil {
		report.AffectedFeatures = []kimi.AffectedFeature{}
	}
	return report
}

func (n *Native) modelSyncWriteBlocked() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.compatibility.Guards.ModelSyncWriteBlocked
}

func (n *Native) accountSwitchWriteBlocked() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.compatibility.Guards.AccountSwitchWriteBlocked
}

func currentEnvMap() map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	return env
}

// runCompatibilityCheck runs the read-only self-check suite and records the
// resulting guards. When reuseContract is non-nil, it skips a duplicate
// Inspect subprocess run.
func (n *Native) runCompatibilityCheck(ctx context.Context, reuseContract *kimi.ContractResult, forceRedetect bool) kimi.CompatibilityReport {
	n.compatMu.Lock()
	defer n.compatMu.Unlock()

	settings := n.controller.Settings()
	install := settings.Installation
	if forceRedetect || install.Archive == "" {
		if installs := kimi.Detect(runtime.GOOS, currentEnvMap()); len(installs) > 0 {
			install = installs[0]
			if install != settings.Installation {
				settings.Installation = install
				_ = n.controller.Configure(settings)
			}
		}
	}

	capabilities := kimi.ProbeWorkCapabilities(install)
	if n.policies != nil {
		n.policies.setCapabilities(capabilities.Subagents)
	}
	workHome, _ := n.controller.WorkHome()
	previous := kimi.LoadOfficialObservation(n.service.StateDir)

	var liveAPIs *kimi.AccountAPICompatCheck
	status := n.controller.Status(ctx)
	livePID := 0
	if status.ManagedPID > 0 && status.State == kimi.StateReady {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if probed, ok := n.controller.ProbeAccountAPIsLive(probeCtx); ok {
			liveAPIs = probed
			livePID = status.ManagedPID
		}
		cancel()
	}

	opts := kimi.CompatibilityOptions{
		Installation: install,
		WorkHome:     workHome,
		Contract:     reuseContract,
		Capabilities: &capabilities,
		LiveAPIs:     liveAPIs,
		Previous:     previous,
	}
	report, contract := kimi.CheckCompatibility(ctx, opts)
	fp := kimi.ReadOfficialFingerprint(install, workHome)
	_ = kimi.SaveOfficialObservation(n.service.StateDir, report)

	n.mu.Lock()
	contractCopy := contract
	n.contract = &contractCopy
	n.workCapabilities = capabilities
	n.compatibility = report
	n.compatFingerprint = fp
	if livePID > 0 {
		n.compatLiveProbedPID = livePID
	}
	n.mu.Unlock()
	return report
}

// maybeRecheckCompatibility checks lightweight stat/metadata fingerprints on
// each watch tick. It re-runs the full self-check only when the official
// install, kernel commit, or BYOK store changed, or when a newly ready managed
// Kimi child has not yet been live-probed for page APIs.
func (n *Native) maybeRecheckCompatibility(ctx context.Context) {
	if !n.compatMu.TryLock() {
		return
	}
	settings := n.controller.Settings()
	install := settings.Installation
	workHome, _ := n.controller.WorkHome()
	fp := kimi.ReadOfficialFingerprint(install, workHome)

	n.mu.Lock()
	prevFP := n.compatFingerprint
	prevPID := n.compatLiveProbedPID
	cachedContract := n.contract
	caps := n.workCapabilities
	n.mu.Unlock()

	if fp != prevFP {
		n.compatMu.Unlock()
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n.runCompatibilityCheck(checkCtx, nil, true)
		return
	}

	status := n.controller.Status(ctx)
	if status.ManagedPID > 0 && status.State == kimi.StateReady && status.ManagedPID != prevPID && cachedContract != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		live, ok := n.controller.ProbeAccountAPIsLive(probeCtx)
		cancel()
		if ok {
			previous := kimi.LoadOfficialObservation(n.service.StateDir)
			report, _ := kimi.CheckCompatibility(ctx, kimi.CompatibilityOptions{
				Installation: install,
				WorkHome:     workHome,
				Contract:     cachedContract,
				Capabilities: &caps,
				LiveAPIs:     live,
				Previous:     previous,
			})
			_ = kimi.SaveOfficialObservation(n.service.StateDir, report)
			n.mu.Lock()
			n.compatibility = report
			n.compatLiveProbedPID = status.ManagedPID
			n.mu.Unlock()
		}
	}
	n.compatMu.Unlock()
}
