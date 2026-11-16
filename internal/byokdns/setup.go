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
	if action == "prepare" && platform == "darwin" {
		resolverPath := s.ResolverPath
		if resolverPath == "" {
			resolverPath = "/etc/resolver/" + Domain
		}
		configuration, err := ResolverConfiguration(DNSPort, resolverPath)
		if err != nil {
			return err
		}
		ready, err := s.reuseResolver(ctx, resolverPath, configuration)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
	}
	temporary, err := os.MkdirTemp("", "kimi-work-byok-")
	if err != nil {
		return errors.New("native_setup_failed")
	}
	defer os.RemoveAll(temporary)

	admin := s.Admin
	if admin == nil {
		if platform == "darwin" {
			admin = darwinAdministrator
		} else {
			admin = windowsAdministrator
		}
	}
	if platform == "darwin" {
		resolverPath := s.ResolverPath
		if resolverPath == "" {
			resolverPath = "/etc/resolver/" + Domain
		}
		configuration, configurationErr := ResolverConfiguration(DNSPort, resolverPath)
		if configurationErr != nil {
			return configurationErr
		}
		source := filepath.Join(temporary, "resolver")
		if err := os.WriteFile(source, []byte(configuration), 0o600); err != nil {
			return errors.New("native_setup_failed")
		}
		err = admin(ctx, ResolverScript(action, source, resolverPath, true), "")
	} else {
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
	}
	if err != nil {
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
	if action == "prepare" {
		return RevealFlag(s.FlagFile)
	}
	return nil
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
