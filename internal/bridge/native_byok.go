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
	return n.prepareBYOKWith(ctx, n.byokSetup())
}

// prepareBYOKSilent is prepareBYOK for a background run: it never opens an
// administrator prompt. A machine that still needs the elevated step is
// reported as needing it, and the panel action performs it when the user asks.
func (n *Native) prepareBYOKSilent(ctx context.Context) error {
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
