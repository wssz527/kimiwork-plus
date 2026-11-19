package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/core"
	"github.com/wssz527/kimi-work-plus/internal/platform"
)

var (
	errBuildMismatch = errors.New("daemon_build_mismatch")
	errHealthRefused = errors.New("daemon_health_refused")
	errStopDaemon    = errors.New("daemon_stop_failed")
)

func DefaultStateDir() (string, error) {
	return platform.DefaultStateDir()
}

func LegacyPreviewStateDir() (string, error) {
	return platform.LegacyPreviewStateDir()
}

func ReadEndpoint(dir string) (Endpoint, error) {
	var endpoint Endpoint
	body, err := core.ReadPrivateFile(filepath.Join(dir, "desktop-endpoint.json"), 4096)
	if err != nil {
		return endpoint, err
	}
	if err = json.Unmarshal(body, &endpoint); err != nil {
		return endpoint, err
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" || parsed.Port() == "" || len(endpoint.Key) != 64 || endpoint.BuildID == "" {
		return Endpoint{}, errors.New("invalid_endpoint")
	}
	return endpoint, nil
}

func health(ctx context.Context, endpoint Endpoint) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL+"/api/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Key)
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var value struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
		BuildID string `json:"buildId"`
	}
	if response.StatusCode != http.StatusOK {
		return errHealthRefused
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&value); err != nil {
		return err
	}
	if value.PID != endpoint.PID || value.Version != Version || value.BuildID != BuildID || endpoint.BuildID != BuildID {
		return errBuildMismatch
	}
	return nil
}

func Healthy(endpoint Endpoint) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return health(ctx, endpoint) == nil
}

func StopDaemon(dir string) error {
	endpoint, err := ReadEndpoint(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := requestStop(ctx, endpoint); err != nil {
		return fmt.Errorf("%w: %w", errStopDaemon, err)
	}
	for {
		err := health(ctx, endpoint)
		if errors.Is(err, errHealthRefused) {
			return errors.New("daemon_stop_replaced")
		}
		if err == nil || errors.Is(err, errBuildMismatch) {
			if ctx.Err() != nil {
				return errors.New("daemon_stop_timeout")
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return nil
	}
}

func requestStop(ctx context.Context, endpoint Endpoint) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL+"/api/daemon/stop", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Key)
	req.Header.Set("Origin", endpoint.URL)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return errors.New("daemon_stop_refused")
	}
	return nil
}

func ReplaceDaemon(dir string) error {
	endpoint, err := ReadEndpoint(dir)
	if err != nil {
		return err
	}
	if endpoint.PID != os.Getpid() || !Healthy(endpoint) {
		return errors.New("daemon_restart_identity_mismatch")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "--state-dir", dir, "--port", "")
	hideDaemonConsole(cmd)
	cmd.Env = append(os.Environ(), "KIMI_WORK_REPLACE_DAEMON=1")
	log, err := daemonLog(dir)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	return cmd.Start()
}

func daemonLog(dir string) (*os.File, error) {
	path := filepath.Join(dir, "daemon.log")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return nil, errors.New("unsafe_daemon_log")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	opened, openErr := log.Stat()
	current, statErr := os.Lstat(path)
	if openErr != nil || statErr != nil || !current.Mode().IsRegular() || (runtime.GOOS != "windows" && current.Mode().Perm()&0077 != 0) || !os.SameFile(opened, current) {
		log.Close()
		return nil, errors.New("unsafe_daemon_log")
	}
	return log, nil
}

func Ensure(dir, binary string) (Endpoint, error) {
	previous, readErr := ReadEndpoint(dir)
	if readErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := health(ctx, previous)
		cancel()
		if err == nil {
			return previous, nil
		}
		if errors.Is(err, errBuildMismatch) {
			if err := StopDaemon(dir); err != nil {
				return Endpoint{}, errors.New("daemon_upgrade_requires_stop")
			}
		}
	}
	if err := core.PrivateDirectory(dir); err != nil {
		return Endpoint{}, err
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return Endpoint{}, err
	}
	log, err := daemonLog(dir)
	if err != nil {
		return Endpoint{}, err
	}
	defer log.Close()
	cmd := exec.Command(absolute, "--state-dir", dir, "--port", "")
	hideDaemonConsole(cmd)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		return Endpoint{}, err
	}
	go cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if endpoint, err := ReadEndpoint(dir); err == nil && Healthy(endpoint) {
			return endpoint, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return Endpoint{}, errors.New("daemon_start_timeout")
}
