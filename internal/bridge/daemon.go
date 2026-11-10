package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"kimiworkplus.local/migration/internal/core"
)

func DefaultStateDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "kimi-work-plus-preview"), nil
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
		return errors.New("daemon_health_refused")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&value); err != nil {
		return err
	}
	if value.PID != endpoint.PID || value.Version != Version || value.BuildID != BuildID || endpoint.BuildID != BuildID {
		return errors.New("daemon_build_mismatch")
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
		return err
	}
	for {
		if health(ctx, endpoint) != nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.New("daemon_stop_timeout")
		}
		time.Sleep(50 * time.Millisecond)
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			stopErr := requestStop(ctx, previous)
			cancel()
			if stopErr != nil {
				return Endpoint{}, errors.New("daemon_upgrade_requires_stop")
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := health(ctx, previous)
				cancel()
				if err != nil && err.Error() != "daemon_health_refused" {
					break
				}
				time.Sleep(50 * time.Millisecond)
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
	logPath := filepath.Join(dir, "daemon.log")
	if info, err := os.Lstat(logPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return Endpoint{}, errors.New("unsafe_daemon_log")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Endpoint{}, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Endpoint{}, err
	}
	defer log.Close()
	opened, openErr := log.Stat()
	current, statErr := os.Lstat(logPath)
	if openErr != nil || statErr != nil || !current.Mode().IsRegular() || current.Mode().Perm()&0077 != 0 || !os.SameFile(opened, current) {
		return Endpoint{}, errors.New("unsafe_daemon_log")
	}
	cmd := exec.Command(absolute, "--state-dir", dir, "--port", "")
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

var errBuildMismatch = errors.New("daemon_build_mismatch")
