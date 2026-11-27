package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

type byokDNS interface {
	Start(int) error
	Close() error
	Port() int
}

func (n *Native) initBYOK() {
	n.dns = &byokdns.Server{}
	n.probe = &byokdns.Probe{}
	if runtime.GOOS == "darwin" {
		if err := n.dns.Start(byokdns.DNSPort); err != nil {
			n.dnsReason = core.ErrorCode(err)
		}
	}
}

func (n *Native) closeBYOK() {
	if n.dns != nil {
		_ = n.dns.Close()
	}
}

func (n *Native) byokFlagFile() string {
	settings := n.controller.Settings()
	profile := settings.UserDataDir
	if settings.ProfileMode == kimi.ProfileModeExistingAuthorized && settings.LoginDataDir != "" {
		profile = settings.LoginDataDir
	}
	return filepath.Join(profile, "kimi-agent", "byok-availability.json")
}

func (n *Native) byokSetup() *byokdns.Setup {
	return &byokdns.Setup{FlagFile: n.byokFlagFile(), Admin: n.admin, Probe: n.probe, ResolverPath: n.resolverPath,
		HostsPath: n.hostsPath, RequireStaticHosts: n.isDirectMode()}
}

func (n *Native) byokState() core.Document {
	n.mu.Lock()
	dnsReason := n.dnsReason
	n.mu.Unlock()
	status := byokdns.Status{Flag: "missing", Intranet: byokdns.ProbeResult{}}
	if n.probe != nil {
		status = n.probe.Check(context.Background(), n.byokFlagFile())
	}
	out := core.Document{}
	raw, _ := json.Marshal(status)
	_ = json.Unmarshal(raw, &out)
	dns := core.Document{"state": "hosts"}
	if runtime.GOOS == "darwin" {
		// A static /etc/hosts entry answers the lookup with no listener running,
		// which is the state Direct Mode needs; the listener's own state stays
		// reported, because a gateway-mode installation still uses it.
		staticHosts := byokdns.HasManagedHostsEntry(n.darwinHostsPath())
		dns["state"] = "stopped"
		if n.dns != nil && n.dns.Port() != 0 {
			dns["state"] = "ready"
		}
		if dnsReason != "" {
			dns["state"] = "unavailable"
			dns["reason"] = dnsReason
		}
		if staticHosts {
			dns["state"] = "hosts"
			dns["staticHosts"] = true
		} else {
			dns["staticHosts"] = false
		}
	} else {
		dns["staticHosts"] = true
	}
	out["dns"] = dns
	return out
}

func (n *Native) prepareBYOK(ctx context.Context) error {
	if n.byokPrepare != nil {
		return n.byokPrepare(ctx)
	}
	return n.prepareBYOKWith(ctx, n.byokSetup())
}

// prepareBYOKSilent is prepareBYOK for a background run: it never opens an
// administrator prompt. A machine that still needs the elevated step is
// reported as needing it, and the panel action performs it when the user asks.
func (n *Native) prepareBYOKSilent(ctx context.Context) error {
	if n.byokPrepare != nil {
		return n.byokPrepare(ctx)
	}
	setup := n.byokSetup()
	refused := false
	if setup.Admin == nil {
		setup.Admin = func(context.Context, string, string) error {
			refused = true
			return errors.New(autoSyncReasonPrepareNeeded)
		}
	}
	err := n.prepareBYOKWith(ctx, setup)
	if refused {
		// The setup layer maps unknown failures onto its own codes, so the
		// refusal is remembered here instead of through the error chain.
		return errors.New(autoSyncReasonPrepareNeeded)
	}
	return err
}

func (n *Native) prepareBYOKWith(ctx context.Context, setup *byokdns.Setup) error {
	n.probe.Invalidate()
	// Direct Mode resolves the intranet name from a static hosts entry, so a
	// listener that cannot bind is not a failure: the port is not used at all.
	// The one administrator step writes that entry and removes the resolver an
	// earlier release installed; once the entry is in place the call costs none.
	if runtime.GOOS == "darwin" && n.isDirectMode() {
		if err := setup.Run(ctx, "prepare"); err != nil {
			return err
		}
		if n.dns != nil {
			_ = n.dns.Close()
		}
		n.probe.Invalidate()
		status := n.probe.Check(ctx, n.byokFlagFile())
		if !status.Intranet.Passed {
			return errors.New("native_byok_intranet_unavailable")
		}
		return nil
	}
	if runtime.GOOS == "darwin" {
		if err := n.dns.Start(byokdns.DNSPort); err != nil {
			n.mu.Lock()
			n.dnsReason = core.ErrorCode(err)
			n.mu.Unlock()
			if !n.probe.DNS(ctx).Passed {
				return err
			}
		} else {
			n.mu.Lock()
			n.dnsReason = ""
			n.mu.Unlock()
		}
	}
	if err := setup.Run(ctx, "prepare"); err != nil {
		return err
	}
	n.probe.Invalidate()
	status := n.probe.Check(ctx, n.byokFlagFile())
	if !status.Revealed {
		return errors.New("native_byok_reveal_failed")
	}
	if !status.Intranet.Passed {
		return errors.New("native_byok_intranet_unavailable")
	}
	return nil
}

// byokIntranetReady reports whether the official gate would pass right now: the
// availability flag is revealed and the intranet name answers with a private
// address. It is the evidence that a feature-disabled answer comes from the
// running application's own cache, not from a machine that is still unready.
func (n *Native) byokIntranetReady(ctx context.Context) bool {
	if n.probe == nil {
		return false
	}
	n.probe.Invalidate()
	status := n.probe.Check(ctx, n.byokFlagFile())
	return status.Revealed && status.Intranet.Passed
}

// isFeatureDisabledReason reports the official build's own gate answer. Kimi
// hides the BYOK surface when its availability flag is missing or the intranet
// name does not resolve to a private address, and every `byokSources` call then
// answers `reasonCode: "feature-disabled"`.
func isFeatureDisabledReason(reason string) bool {
	switch reason {
	case "native_import_feature-disabled", "native_import_feature_disabled", "feature-disabled",
		"model_disconnect_feature-disabled", "model_disconnect_feature_disabled":
		return true
	}
	return false
}

// staleFeatureDisabled reports the one recoverable feature-disabled answer: the
// flag and the intranet name are both in place, so only the running official
// build's one minute negative cache still refuses. A managed restart clears it.
func (n *Native) staleFeatureDisabled(ctx context.Context, reason string) bool {
	if !isFeatureDisabledReason(reason) || !n.managedKimiRunning() {
		return false
	}
	return n.byokIntranetReady(ctx)
}

// restartManagedOfficial restarts the managed application once. Explicit
// actions use it to clear the official build's cached gate answer; nothing in a
// background run may restart Kimi.
func (n *Native) restartManagedOfficial(ctx context.Context) error {
	if n.restartOfficial != nil {
		return n.restartOfficial(ctx)
	}
	if err := n.refreshControllerInstallationForce(); err != nil {
		return err
	}
	if err := n.controller.RestartOfficial(ctx); err != nil {
		return err
	}
	return n.syncWorkHome()
}

// importAfterStaleFeatureDisabled retries one import after a managed restart
// when the page refused the BYOK surface although the machine is ready. The
// retry runs at most once: a second refusal is a real gate, not a stale cache.
func (n *Native) importAfterStaleFeatureDisabled(ctx context.Context, imported kimi.ImportResult, err error,
	run func(context.Context) (kimi.ImportResult, error)) (kimi.ImportResult, error) {
	if !n.staleFeatureDisabled(ctx, firstCode(imported.Reason, core.ErrorCode(err))) {
		return imported, err
	}
	if restartErr := n.restartManagedOfficial(ctx); restartErr != nil {
		return imported, err
	}
	return run(ctx)
}

// disconnectAfterStaleFeatureDisabled is the same one-shot retry for an
// explicit disconnect.
func (n *Native) disconnectAfterStaleFeatureDisabled(ctx context.Context, removed kimi.DisconnectResult, err error,
	run func(context.Context) (kimi.DisconnectResult, error)) (kimi.DisconnectResult, error) {
	if !n.staleFeatureDisabled(ctx, removed.Reason) {
		return removed, err
	}
	if restartErr := n.restartManagedOfficial(ctx); restartErr != nil {
		return removed, err
	}
	return run(ctx)
}

// recoverDisconnectFeatureDisabled keeps an explicit disconnect working when the
// official build refuses its BYOK surface.
//
// The refusal means two different things. When the profile's store holds no
// managed source for the signed-in account, only this product's local import
// record is left, and clearing it needs no official call. Otherwise the source
// is still in Kimi and the official disconnect has to run: the flag and the
// intranet name are settled first, and one managed restart clears the official
// build's own one minute cache if it still refuses.
func (n *Native) recoverDisconnectFeatureDisabled(ctx context.Context, removed kimi.DisconnectResult, err error) (kimi.DisconnectResult, error) {
	if !isFeatureDisabledReason(removed.Reason) {
		return removed, err
	}
	if hasActive, verified := n.controller.HasActiveManagedStoreSources(); verified && !hasActive {
		local, localErr := n.controller.DisconnectLocalStateAuthorized(ctx)
		if localErr != nil {
			return removed, err
		}
		return local, nil
	}
	if n.probe != nil {
		if prepareErr := n.prepareBYOK(ctx); prepareErr != nil {
			return removed, err
		}
	}
	retried, retryErr := n.disconnectModelsWithAutoStart(ctx)
	if retried.OK || !isFeatureDisabledReason(retried.Reason) {
		return retried, retryErr
	}
	return n.disconnectAfterStaleFeatureDisabled(ctx, retried, retryErr, n.disconnectModelsWithAutoStart)
}

func (n *Native) removeBYOK(ctx context.Context) error {
	if err := n.byokSetup().Run(ctx, "remove-dns"); err != nil {
		return err
	}
	n.probe.Invalidate()
	if runtime.GOOS == "darwin" && n.dns != nil {
		if err := n.dns.Close(); err != nil {
			return errors.New("native_byok_dns_stop_failed")
		}
	}
	return nil
}
