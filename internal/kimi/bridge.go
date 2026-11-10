package kimi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The native bridge is the private HTTP endpoint the managed Kimi host exposes
// inside its own process. It is loopback-only, bearer-authenticated, and its
// route set is fixed by the host that created it.
//
// Two sides live here:
//
//	Client  reads the descriptor file and calls the endpoint. It is what the
//	        controller uses against a host this process started.
//	Host    serves the same routes over the private CDP pipe, for the managed
//	        child this package launched.
//
// Nothing here opens a debug port, and the descriptor never leaves the Kimi home
// directory it was written to.

// BridgeContractGeneration is the capability number the host advertises. A
// caller compares it instead of probing routes by failure: a host built before a
// route existed answers 404 for a route it simply does not have.
const BridgeContractGeneration = 5

// BridgeDescriptorName is the descriptor file inside the Kimi home.
const BridgeDescriptorName = ".kimi-work-desktop-bridge.json"

// Bridge operations. The set is fixed: an unknown operation is refused before
// any request is made.
const (
	OperationStatus            = "status"
	OperationImportModels      = "import-models"
	OperationCloseWorkSettings = "close-work-settings"
	OperationWorkSurface       = "work-surface"
	OperationActivateWork      = "activate-work"
	OperationOpenWorkSettings  = "open-work-settings"
)

var bridgeOperations = map[string]bool{
	OperationStatus: true, OperationImportModels: true, OperationCloseWorkSettings: true,
	OperationWorkSurface: true, OperationActivateWork: true, OperationOpenWorkSettings: true,
}

var (
	ErrBridgeUnavailable  = errors.New("native_bridge_unavailable")
	ErrBridgeUnauthorized = errors.New("native_bridge_unauthorized")
	ErrBridgeBusy         = errors.New("native_bridge_busy")
)

// BridgeDescriptor is the loopback endpoint a managed host published.
type BridgeDescriptor struct {
	SchemaVersion  int    `json:"schemaVersion"`
	ID             string `json:"id"`
	PID            int    `json:"pid"`
	ApplicationPID int    `json:"applicationPid"`
	URL            string `json:"url"`
	Token          string `json:"token"`
}

// BridgeStatus is the answer to OperationStatus.
type BridgeStatus struct {
	OK             bool   `json:"ok"`
	State          string `json:"state"`
	ApplicationPID int    `json:"applicationPid"`
	Contract       int    `json:"contract"`
	Reason         string `json:"reason,omitempty"`
}

// WorkSurface reports what is actually on screen. `Work` and `Rendered` describe
// the same page: only a target that is both visible and painted sets both.
type WorkSurface struct {
	OK       bool             `json:"ok"`
	Work     bool             `json:"work"`
	Settings bool             `json:"settings"`
	Rendered bool             `json:"rendered"`
	Pages    int              `json:"pages"`
	Shown    []map[string]any `json:"shown,omitempty"`
}

// ImportRequest is what the caller sends to OperationImportModels. The token is
// the gateway's own local token, never a provider key.
type ImportRequest struct {
	URL                string `json:"url"`
	Token              string `json:"token"`
	ExpectedAccountKey string `json:"expectedAccountKey,omitempty"`
	OpenSettings       bool   `json:"openSettings,omitempty"`
}

// ImportResult is the host's answer. A confirmed import that still owes a
// restart is not an applied import, and says so.
type ImportResult struct {
	OK              bool   `json:"ok"`
	Reason          string `json:"reason,omitempty"`
	Models          int    `json:"models,omitempty"`
	RequiresRestart bool   `json:"requiresRestart,omitempty"`
	RestartDeferred bool   `json:"restartDeferred,omitempty"`
	Opened          bool   `json:"opened,omitempty"`
	Closed          *bool  `json:"closed,omitempty"`
}

// ActionResult is the answer to the page actions.
type ActionResult struct {
	OK     bool     `json:"ok"`
	Reason string   `json:"reason,omitempty"`
	Via    []string `json:"via,omitempty"`
}

// ReadBridgeDescriptor reads and validates the descriptor. Anything unexpected
// answers ErrBridgeUnavailable rather than a guessed endpoint: a descriptor that
// points somewhere else must never be dialled.
func ReadBridgeDescriptor(kimiDir string) (BridgeDescriptor, error) {
	file := filepath.Join(kimiDir, BridgeDescriptorName)
	info, err := os.Lstat(file)
	if err != nil {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16384 {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	if info.Mode().Perm()&0o077 != 0 {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	var descriptor BridgeDescriptor
	if json.Unmarshal(data, &descriptor) != nil || descriptor.SchemaVersion != 1 {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	parsed, err := url.Parse(descriptor.URL)
	// A URL with no path parses as an empty Path, and the client appends the
	// operation to the base. Both "" and "/" are accepted so a descriptor written
	// either way is usable; anything else is a different endpoint.
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	if len(descriptor.Token) != 64 || !isLowerHexString(descriptor.Token) {
		return BridgeDescriptor{}, ErrBridgeUnavailable
	}
	return descriptor, nil
}

func isLowerHexString(value string) bool {
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return value != ""
}

// BridgeClient calls one managed host.
type BridgeClient struct {
	descriptor BridgeDescriptor
	http       *http.Client
}

// NewBridgeClient builds a client from a descriptor. The transport never
// follows a redirect and never honours a proxy: the endpoint is loopback.
func NewBridgeClient(descriptor BridgeDescriptor) *BridgeClient {
	transport := &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext:        (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
	return &BridgeClient{descriptor: descriptor, http: &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Status asks the host what it is doing.
func (c *BridgeClient) Status(ctx context.Context) (BridgeStatus, error) {
	var status BridgeStatus
	err := c.call(ctx, OperationStatus, nil, &status)
	return status, err
}

// WorkSurface asks what is on screen.
func (c *BridgeClient) WorkSurface(ctx context.Context) (WorkSurface, error) {
	var surface WorkSurface
	err := c.call(ctx, OperationWorkSurface, nil, &surface)
	return surface, err
}

// ImportModels asks the host to run the official model import.
func (c *BridgeClient) ImportModels(ctx context.Context, request ImportRequest) (ImportResult, error) {
	var result ImportResult
	err := c.call(ctx, OperationImportModels, request, &result)
	return result, err
}

// CloseWorkSettings posts the official overlay close.
func (c *BridgeClient) CloseWorkSettings(ctx context.Context) (ActionResult, error) {
	var result ActionResult
	err := c.call(ctx, OperationCloseWorkSettings, struct{}{}, &result)
	return result, err
}

// ActivateWork asks the desktop's own "show Work" action.
func (c *BridgeClient) ActivateWork(ctx context.Context) (ActionResult, error) {
	var result ActionResult
	err := c.call(ctx, OperationActivateWork, struct{}{}, &result)
	return result, err
}

// OpenWorkSettings opens the Work view's own settings page.
func (c *BridgeClient) OpenWorkSettings(ctx context.Context) (ActionResult, error) {
	var result ActionResult
	err := c.call(ctx, OperationOpenWorkSettings, struct{}{}, &result)
	return result, err
}

func (c *BridgeClient) call(ctx context.Context, operation string, input any, out any) error {
	if !bridgeOperations[operation] {
		return fmt.Errorf("native_bridge_invalid_request")
	}
	method := http.MethodPost
	var body []byte
	if operation == OperationStatus || operation == OperationWorkSurface {
		method = http.MethodGet
	} else {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("native_bridge_invalid_request")
		}
		body = encoded
	}
	// The descriptor URL may or may not carry a trailing slash; the operation is
	// always one path segment.
	endpoint := strings.TrimRight(c.descriptor.URL, "/") + "/" + operation
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrBridgeUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+c.descriptor.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return ErrBridgeUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrBridgeUnauthorized
	case http.StatusConflict:
		return ErrBridgeBusy
	default:
		return fmt.Errorf("native_bridge_failed:%d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("native_bridge_invalid_response")
	}
	return nil
}

// ---------------------------------------------------------------------------
// host side

// BridgeHost serves the same routes over the private pipe. It is the managed
// child's counterpart: the page actions run inside Kimi, and this type only
// carries the requests and their answers.
type BridgeHost struct {
	descriptor BridgeDescriptor
	server     *http.Server
	listener   net.Listener
	kimiDir    string
	actions    PageActions
}

// PageActions is the page-level work the host delegates. The controller supplies
// it; a test supplies a fake.
type PageActions interface {
	// ImportModels performs the official import and reports whether a restart
	// is still owed.
	ImportModels(ctx context.Context, request ImportRequest) (ImportResult, error)
	// WorkSurface reports what is on screen.
	WorkSurface(ctx context.Context) (WorkSurface, error)
	// CloseWorkSettings posts the official overlay close.
	CloseWorkSettings(ctx context.Context) (ActionResult, error)
	// ActivateWork asks the desktop's own show-Work action.
	ActivateWork(ctx context.Context) (ActionResult, error)
	// OpenWorkSettings opens the Work view's own settings page.
	OpenWorkSettings(ctx context.Context) (ActionResult, error)
	// Connected reports whether the pipe is still usable.
	Connected() bool
}

// StartBridgeHost publishes a loopback endpoint and writes its descriptor into
// the Kimi home. The listener is bound to 127.0.0.1 with port 0, so no fixed
// port is ever claimed.
func StartBridgeHost(kimiDir string, applicationPID int, actions PageActions) (*BridgeHost, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("native_bridge_listen_failed")
	}
	id, err := randomUUID()
	if err != nil {
		listener.Close()
		return nil, err
	}
	host := &BridgeHost{
		descriptor: BridgeDescriptor{SchemaVersion: 1, ID: id, PID: os.Getpid(),
			ApplicationPID: applicationPID, URL: "http://" + listener.Addr().String(), Token: token},
		listener: listener,
		kimiDir:  kimiDir,
		actions:  actions,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", host.handle)
	host.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := host.publish(); err != nil {
		listener.Close()
		return nil, err
	}
	go func() { _ = host.server.Serve(listener) }()
	return host, nil
}

// Descriptor is the published endpoint, for the controller's own client.
func (h *BridgeHost) Descriptor() BridgeDescriptor { return h.descriptor }

// Client is a client for this host, so the controller can exercise exactly the
// path a remote caller would.
func (h *BridgeHost) Client() *BridgeClient { return NewBridgeClient(h.descriptor) }

func (h *BridgeHost) publish() error {
	if err := os.MkdirAll(h.kimiDir, 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(h.descriptor)
	if err != nil {
		return err
	}
	file := filepath.Join(h.kimiDir, BridgeDescriptorName)
	temporary := file + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}

func (h *BridgeHost) handle(writer http.ResponseWriter, request *http.Request) {
	reply := func(status int, payload any) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(payload)
	}
	// A request that is not loopback, carries an Origin, or names a different
	// Host is refused before the token is even compared.
	if request.Header.Get("Origin") != "" || request.Header.Get("Sec-Fetch-Site") != "" ||
		request.Host != h.descriptor.URL[len("http://"):] {
		reply(http.StatusUnauthorized, map[string]any{"ok": false, "reason": "native_bridge_unauthorized"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("Authorization")),
		[]byte("Bearer "+h.descriptor.Token)) != 1 {
		reply(http.StatusUnauthorized, map[string]any{"ok": false, "reason": "native_bridge_unauthorized"})
		return
	}
	operation := strings.TrimPrefix(request.URL.Path, "/")
	if !bridgeOperations[operation] {
		reply(http.StatusNotFound, map[string]any{"ok": false, "reason": "native_bridge_not_found"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()

	switch operation {
	case OperationStatus:
		state := "ready"
		if !h.actions.Connected() {
			state = "disconnected"
		}
		reply(http.StatusOK, BridgeStatus{OK: true, State: state,
			ApplicationPID: h.descriptor.ApplicationPID, Contract: BridgeContractGeneration})
	case OperationWorkSurface:
		surface, err := h.actions.WorkSurface(ctx)
		if err != nil {
			reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": safeReason(err)})
			return
		}
		reply(http.StatusOK, surface)
	default:
		if request.Method != http.MethodPost {
			reply(http.StatusNotFound, map[string]any{"ok": false, "reason": "native_bridge_not_found"})
			return
		}
		if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
			reply(http.StatusUnsupportedMediaType, map[string]any{"ok": false, "reason": "native_bridge_json_required"})
			return
		}
		body := http.MaxBytesReader(writer, request.Body, 16384)
		var input json.RawMessage
		if json.NewDecoder(body).Decode(&input) != nil {
			reply(http.StatusBadRequest, map[string]any{"ok": false, "reason": "native_bridge_invalid_request"})
			return
		}
		switch operation {
		case OperationImportModels:
			var importRequest ImportRequest
			if json.Unmarshal(input, &importRequest) != nil {
				reply(http.StatusBadRequest, map[string]any{"ok": false, "reason": "native_bridge_invalid_request"})
				return
			}
			result, err := h.actions.ImportModels(ctx, importRequest)
			if err != nil {
				reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": safeReason(err)})
				return
			}
			reply(http.StatusOK, result)
		case OperationCloseWorkSettings:
			result, err := h.actions.CloseWorkSettings(ctx)
			if err != nil {
				reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": safeReason(err)})
				return
			}
			reply(http.StatusOK, result)
		case OperationActivateWork:
			result, err := h.actions.ActivateWork(ctx)
			if err != nil {
				reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": safeReason(err)})
				return
			}
			reply(http.StatusOK, result)
		case OperationOpenWorkSettings:
			result, err := h.actions.OpenWorkSettings(ctx)
			if err != nil {
				reply(http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": safeReason(err)})
				return
			}
			reply(http.StatusOK, result)
		}
	}
}

// Close stops the endpoint and removes the descriptor it published.
func (h *BridgeHost) Close() error {
	if h.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(ctx)
	}
	if h.listener != nil {
		_ = h.listener.Close()
	}
	file := filepath.Join(h.kimiDir, BridgeDescriptorName)
	if descriptor, err := ReadBridgeDescriptor(h.kimiDir); err == nil && descriptor.ID == h.descriptor.ID {
		_ = os.Remove(file)
	}
	return nil
}

func safeReason(err error) string {
	if err == nil {
		return "native_bridge_failed"
	}
	message := err.Error()
	if len(message) > 80 {
		message = message[:80]
	}
	if message == "" {
		return "native_bridge_failed"
	}
	return message
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func randomUUID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	buffer[6] = buffer[6]&0x0f | 0x40
	buffer[8] = buffer[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16]), nil
}
