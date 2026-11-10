package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DaemonTransport is how a lifecycle operation talks to the daemon it is about
// to replace. It is an interface so the executor can be tested without a
// listening socket, and so the one place that speaks HTTP stays small.
//
// The authenticated stop endpoint is the only graceful way to stop the preview
// daemon. The helper never assumes that terminating a job is equivalent: a
// platform termination is reported as forced.
type DaemonTransport interface {
	// Endpoint reads the private endpoint record for a state directory.
	Endpoint(stateDir string) (DaemonEndpoint, error)
	// Stop asks the daemon to drain and exit. It succeeds only when the daemon
	// answered the authenticated request with its documented status.
	Stop(ctx context.Context, endpoint DaemonEndpoint) error
	// Health reports whether the recorded process still answers.
	Health(ctx context.Context, endpoint DaemonEndpoint) HealthResult
}

// DaemonEndpoint mirrors the private desktop-endpoint.json record. It is
// duplicated here rather than imported from the bridge because platform must not
// depend on the HTTP layer that depends on it.
type DaemonEndpoint struct {
	URL     string `json:"url"`
	Key     string `json:"key"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	BuildID string `json:"buildId"`
}

// HealthResult is one probe of the recorded daemon.
type HealthResult struct {
	// Alive is true when a process answered the authenticated health request.
	Alive bool
	// PID and BuildID are what answered. They differ from the record when a
	// different daemon took the address, which is not a successful drain.
	PID     int
	BuildID string
	// Reason is a short code when the probe failed for a reason other than
	// "nothing is listening".
	Reason string
}

var (
	errEndpointUnavailable = errors.New("daemon_endpoint_unavailable")
	errEndpointInvalid     = errors.New("daemon_endpoint_invalid")
	errStopRefused         = errors.New("daemon_stop_refused")
)

// HTTPTransport is the real machine's transport.
type HTTPTransport struct {
	// Timeout bounds one request. Zero means two seconds.
	Timeout time.Duration
}

func (t HTTPTransport) timeout() time.Duration {
	if t.Timeout <= 0 {
		return 2 * time.Second
	}
	return t.Timeout
}

// Endpoint reads desktop-endpoint.json. The record must be a private file whose
// URL is loopback-only and whose token is the expected length; anything else is
// refused rather than guessed at.
func (t HTTPTransport) Endpoint(stateDir string) (DaemonEndpoint, error) {
	content, err := readPrivateFile(filepath.Join(stateDir, "desktop-endpoint.json"), 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DaemonEndpoint{}, errEndpointUnavailable
		}
		return DaemonEndpoint{}, err
	}
	var endpoint DaemonEndpoint
	if err := json.Unmarshal(content, &endpoint); err != nil {
		return DaemonEndpoint{}, errEndpointInvalid
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Path != "" || parsed.Port() == "" ||
		len(endpoint.Key) != 64 || endpoint.PID <= 0 {
		return DaemonEndpoint{}, errEndpointInvalid
	}
	return endpoint, nil
}

func (t HTTPTransport) client() *http.Client {
	return &http.Client{
		Timeout: t.timeout(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Stop posts the authenticated stop request. A 202 means the daemon accepted and
// is draining; anything else is a refusal, including a redirect.
func (t HTTPTransport) Stop(ctx context.Context, endpoint DaemonEndpoint) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint.URL+"/api/daemon/stop", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+endpoint.Key)
	request.Header.Set("Origin", endpoint.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err := t.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return errStopRefused
	}
	return nil
}

// Health probes the recorded daemon. A refused connection is the normal answer
// once it has exited, so it is reported as not alive without a reason.
func (t HTTPTransport) Health(ctx context.Context, endpoint DaemonEndpoint) HealthResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL+"/api/health", nil)
	if err != nil {
		return HealthResult{Reason: reasonOf(err)}
	}
	request.Header.Set("Authorization", "Bearer "+endpoint.Key)
	response, err := t.client().Do(request)
	if err != nil {
		if isConnectionRefused(err) {
			return HealthResult{}
		}
		return HealthResult{Reason: "daemon_health_unreachable"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return HealthResult{Reason: "daemon_health_refused"}
	}
	var value struct {
		PID     int    `json:"pid"`
		BuildID string `json:"buildId"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&value); err != nil {
		return HealthResult{Reason: "daemon_health_invalid"}
	}
	return HealthResult{Alive: true, PID: value.PID, BuildID: value.BuildID}
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "connection refused") || strings.Contains(text, "connect: connection refused") ||
		strings.Contains(text, "actively refused") || strings.Contains(text, "No connection could be made")
}

// DrainResult is the outcome of the graceful stop.
type DrainResult struct {
	// Drained is true when the daemon accepted the stop and then stopped
	// answering.
	Drained bool
	// Replaced is true when a different process answered instead, which means
	// the change did not end the old daemon cleanly.
	Replaced bool
	// Reason is a short code when Drained is false.
	Reason string
	Detail string
}

// drain asks the recorded daemon to stop and waits for it to go away. The wait
// is bounded: an operation that cannot drain has to say so rather than hang.
func drain(ctx context.Context, transport DaemonTransport, stateDir string, timeout time.Duration,
	now func() time.Time, sleep func(time.Duration)) DrainResult {
	endpoint, err := transport.Endpoint(stateDir)
	if err != nil {
		if errors.Is(err, errEndpointUnavailable) {
			// No daemon record means nothing to drain. That is not a failure:
			// the caller may already be stopped.
			return DrainResult{Drained: true}
		}
		return DrainResult{Reason: reasonOf(err), Detail: "The private daemon record could not be read."}
	}
	health := transport.Health(ctx, endpoint)
	if !health.Alive {
		return DrainResult{Drained: true}
	}
	if health.PID != endpoint.PID || health.BuildID != endpoint.BuildID {
		return DrainResult{Replaced: true, Reason: "daemon_identity_mismatch",
			Detail: "The address is answered by a different process than the recorded one."}
	}
	if err := transport.Stop(ctx, endpoint); err != nil {
		return DrainResult{Reason: reasonOf(err), Detail: "The daemon refused the authenticated stop request."}
	}
	deadline := now().Add(timeout)
	for now().Before(deadline) {
		select {
		case <-ctx.Done():
			return DrainResult{Reason: "daemon_drain_cancelled", Detail: "The operation was cancelled while draining."}
		default:
		}
		probe := transport.Health(ctx, endpoint)
		if !probe.Alive {
			return DrainResult{Drained: true}
		}
		if probe.PID != endpoint.PID {
			return DrainResult{Replaced: true, Reason: "daemon_replaced_during_drain",
				Detail: "A different process answered while the recorded daemon was draining."}
		}
		sleep(50 * time.Millisecond)
	}
	return DrainResult{Reason: "daemon_drain_timeout", Detail: "The daemon accepted the stop but was still answering when the drain deadline passed."}
}

// readPrivateFile is the local copy of the private-file rule: regular file, no
// group or other access on macOS, and a size bound.
func readPrivateFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private_file_permissions")
	}
	if !isWindows() && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private_file_permissions")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("private_file_changed")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("private_file_too_large")
	}
	return content, nil
}
