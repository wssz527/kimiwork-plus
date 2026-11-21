package byokdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const HostsMarker = "# kimi-work-gateway native BYOK"

type Administrator func(context.Context, string, string) error

type Setup struct {
	FlagFile     string
	Platform     string
	ResolverPath string
	HostsPath    string
	Admin        Administrator
	Probe        *Probe
	// RequireStaticHosts makes macOS setup prefer a static /etc/hosts entry over
	// the scoped resolver. It is what lets the user stop gateway-daemon and still
	// resolve the intranet name; false keeps the v1.0.2 gateway-mode behavior,
	// where an existing resolver is reused without an administrator prompt.
	RequireStaticHosts bool
}

func (s *Setup) Run(ctx context.Context, action string) error {
	if action != "prepare" && action != "remove-dns" {
		return errors.New("invalid_setup_action")
	}
	platform := s.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "darwin" && platform != "windows" && platform != "win32" {
		return errors.New("unsupported_platform")
	}
	if s.FlagFile == "" {
		return errors.New("invalid_setup_flag")
	}
	if action == "prepare" {
		profile := filepath.Dir(filepath.Dir(s.FlagFile))
		info, err := os.Stat(profile)
		if err != nil || !info.IsDir() {
			return errors.New("kimi_data_missing")
		}
		if _, _, err := readFlag(s.FlagFile); err != nil {
			return err
		}
	}

	admin := s.Admin
	if admin == nil {
		if platform == "darwin" {
			admin = darwinAdministrator
		} else {
			admin = windowsAdministrator
		}
	}
	if platform == "darwin" {
		// runDarwin reveals the flag itself after a successful prepare, because the
		// zero-prompt reuse paths return before the shared tail below.
		if err := s.runDarwin(ctx, admin, action); err != nil {
			return setupFailure(ctx, err)
		}
		return nil
	}
	temporary, err := os.MkdirTemp("", "kimi-work-byok-")
	if err != nil {
		return errors.New("native_setup_failed")
	}
	defer os.RemoveAll(temporary)

	hostsPath := s.HostsPath
	if hostsPath == "" {
		hostsPath = DefaultHostsPath(nil)
	}
	report := filepath.Join(temporary, "report")
	err = admin(ctx, HostsScript(action, hostsPath, true, report), report)
	if err == nil {
		data, readErr := os.ReadFile(report)
		if readErr != nil || strings.TrimSpace(string(data)) != "done" {
			return errors.New("setup_report_missing")
		}
	}
	if err != nil {
		return setupFailure(ctx, err)
	}
	if action == "prepare" {
		return RevealFlag(s.FlagFile)
	}
	return nil
}

// runDarwin manages two files in one elevated step: the static hosts entry and
// the scoped resolver older releases installed for the gateway's DNS server.
//
// The zero-prompt paths come first. A machine that already resolves the name (a
// managed hosts entry, or, in gateway mode, our own resolver) needs no
// administrator prompt at all.
func (s *Setup) runDarwin(ctx context.Context, admin Administrator, action string) error {
	resolverPath := s.ResolverPath
	if resolverPath == "" {
		resolverPath = "/etc/resolver/" + Domain
	}
	configuration, err := ResolverConfiguration(DNSPort, resolverPath)
	if err != nil {
		return err
	}
	hostsPath := s.HostsPath
	if hostsPath == "" {
		hostsPath = DarwinHostsPath
	}
	if action == "prepare" {
		ready, err := s.reuseHosts(ctx, hostsPath, resolverPath)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if !s.RequireStaticHosts {
			ready, err := s.reuseResolver(ctx, resolverPath, configuration)
			if err != nil {
				return err
			}
			if ready {
				return nil
			}
			temporary, err := os.MkdirTemp("", "kimi-work-byok-")
			if err != nil {
				return errors.New("native_setup_failed")
			}
			defer os.RemoveAll(temporary)
			source := filepath.Join(temporary, "resolver")
			if err := os.WriteFile(source, []byte(configuration), 0o600); err != nil {
				return errors.New("native_setup_failed")
			}
			if err := admin(ctx, ResolverScript(action, source, resolverPath, true), ""); err != nil {
				return err
			}
			return RevealFlag(s.FlagFile)
		}
	}
	beforeHosts, err := readHostsForSetup(hostsPath)
	if err != nil {
		return err
	}
	nextHosts, changed, err := UpdateHosts(beforeHosts, action)
	if err != nil {
		return err
	}
	resolverPresent := false
	if _, err := os.Lstat(resolverPath); err == nil {
		resolverPresent = true
	}
	if action == "remove-dns" && !changed && !resolverPresent {
		// Nothing of ours is left: a clean removal does not need the administrator.
		return nil
	}
	temporary, err := os.MkdirTemp("", "kimi-work-byok-")
	if err != nil {
		return errors.New("native_setup_failed")
	}
	defer os.RemoveAll(temporary)
	beforePath := filepath.Join(temporary, "hosts-before")
	afterPath := filepath.Join(temporary, "hosts-after")
	sourcePath := filepath.Join(temporary, "resolver")
	if err := os.WriteFile(beforePath, beforeHosts, 0o600); err != nil {
		return errors.New("native_setup_failed")
	}
	if err := os.WriteFile(afterPath, nextHosts, 0o600); err != nil {
		return errors.New("native_setup_failed")
	}
	if err := os.WriteFile(sourcePath, []byte(configuration), 0o600); err != nil {
		return errors.New("native_setup_failed")
	}
	if err := admin(ctx, DarwinHostsScript(action, beforePath, afterPath, hostsPath, sourcePath, resolverPath, true), ""); err != nil {
		return err
	}
	if action == "prepare" {
		return RevealFlag(s.FlagFile)
	}
	return nil
}

// reuseHosts is the zero-prompt path for a machine that already resolves the
// name from /etc/hosts.
//
// In static mode a legacy resolver still pointed at the daemon port must be
// removed, and that needs the administrator, so it is not a reuse. In gateway
// mode the resolver may stay: either file already answers the lookup.
func (s *Setup) reuseHosts(ctx context.Context, hostsPath, resolverPath string) (bool, error) {
	if !HasManagedHostsEntry(hostsPath) {
		return false, nil
	}
	if s.Probe == nil || !s.Probe.DNS(ctx).Passed {
		return false, nil
	}
	if s.RequireStaticHosts {
		if _, err := os.Lstat(resolverPath); err == nil {
			return false, nil
		}
	}
	return true, RevealFlag(s.FlagFile)
}

// readHostsForSetup reads the file the elevated step is about to replace. A
// symlink or a non-regular file is refused here, before any prompt: the entry
// must land in the real hosts file.
func readHostsForSetup(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("native_setup_failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("resolver_conflict")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("native_setup_failed")
	}
	return content, nil
}

// setupFailure maps an administrator or script failure onto the code the UI
// shows. The shell contract is exit 73 for a conflict and the named strings for
// everything else.
func setupFailure(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("setup_timeout")
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return errors.New("setup_cancelled")
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "resolver_conflict"), strings.Contains(text, "exit status 73"):
		return errors.New("resolver_conflict")
	case strings.Contains(text, "setup_cancelled"), strings.Contains(text, "user cancel"), strings.Contains(text, "operation was canceled"), strings.Contains(text, "cancelled by the user"), strings.Contains(text, "(-128)"):
		return errors.New("setup_cancelled")
	case strings.Contains(text, "unsupported_hosts_encoding"):
		return errors.New("unsupported_hosts_encoding")
	case strings.Contains(text, "hosts_verify_failed"):
		return errors.New("hosts_verify_failed")
	case strings.Contains(text, "(-60007)"), strings.Contains(text, "authorization_failed"), strings.Contains(text, "not authorized"), strings.Contains(text, "administrator name or password"), strings.Contains(text, "authentication_failed"):
		return errors.New("setup_authorization_failed")
	default:
		return errors.New("native_setup_failed")
	}
}

func (s *Setup) reuseResolver(ctx context.Context, path, configuration string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, []byte(configuration)) || s.Probe == nil || !s.Probe.DNS(ctx).Passed {
		return false, nil
	}
	return true, RevealFlag(s.FlagFile)
}

func RevealFlag(file string) error {
	value, _, err := readFlag(file)
	if err != nil {
		return err
	}
	value["revealed"] = true
	parent := filepath.Dir(file)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("invalid_setup_flag")
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid_setup_flag")
	}
	temporary, err := os.CreateTemp(parent, ".byok-availability-")
	if err != nil {
		return errors.New("invalid_setup_flag")
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("invalid_setup_flag")
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		temporary.Close()
		return errors.New("invalid_setup_flag")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("invalid_setup_flag")
	}
	if err := os.Rename(name, file); err != nil {
		return errors.New("invalid_setup_flag")
	}
	return nil
}
