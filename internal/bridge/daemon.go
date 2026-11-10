package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"kimiworkplus.local/migration/internal/core"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func DefaultStateDir() (string, error) {
	d, e := os.UserConfigDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(d, "kimi-work-plus-preview"), nil
}
func ReadEndpoint(dir string) (Endpoint, error) {
	var ep Endpoint
	b, e := core.ReadPrivateFile(filepath.Join(dir, "desktop-endpoint.json"), 4096)
	if e != nil {
		return ep, e
	}
	if e = json.Unmarshal(b, &ep); e != nil {
		return ep, e
	}
	u, e := url.Parse(ep.URL)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Path != "" || len(ep.Key) != 64 {
		return Endpoint{}, errors.New("invalid_endpoint")
	}
	return ep, nil
}
func Healthy(ep Endpoint) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", ep.URL+"/api/health", nil)
	if e != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+ep.Key)
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := client.Do(req)
	if e != nil {
		return false
	}
	defer res.Body.Close()
	var v struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
	}
	return res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(&v) == nil && v.PID == ep.PID && v.Version == Version
}
func Ensure(dir, binary string) (Endpoint, error) {
	if ep, e := ReadEndpoint(dir); e == nil && Healthy(ep) {
		return ep, nil
	}
	if e := core.PrivateDirectory(dir); e != nil {
		return Endpoint{}, e
	}
	logPath := filepath.Join(dir, "daemon.log")
	if info, e := os.Lstat(logPath); e == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return Endpoint{}, errors.New("unsafe_daemon_log")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return Endpoint{}, e
	}
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return Endpoint{}, e
	}
	defer log.Close()
	opened, openErr := log.Stat()
	current, statErr := os.Lstat(logPath)
	if openErr != nil || statErr != nil || !current.Mode().IsRegular() || current.Mode().Perm()&0077 != 0 || !os.SameFile(opened, current) {
		return Endpoint{}, errors.New("unsafe_daemon_log")
	}
	cmd := exec.Command(binary, "--state-dir", dir, "--port", "0")
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		return Endpoint{}, e
	}
	go cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ep, e := ReadEndpoint(dir); e == nil && Healthy(ep) {
			return ep, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return Endpoint{}, errors.New("daemon_start_timeout")
}
