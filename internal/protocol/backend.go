package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// RequestOptions is everything one forwarded request needs beyond its body.
type RequestOptions struct {
	// Headers are the whitelisted session headers from the client.
	Headers map[string]string
	// ConnectionID selects the endpoint; nil means the legacy single upstream.
	ConnectionID *string
	// Model is the catalog row this attempt is served as. A row naming a Messages
	// upstream is served from that conversion path; every other row keeps the
	// OpenAI wire it always had. A failover attempt passes a copy whose
	// `UpstreamProtocol` is the candidate's own, so one model can span two wires.
	Model *Model
}

// UpstreamResponse is a reply this process has to forward byte for byte.
type UpstreamResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Endpoint is one upstream base URL with one credential. An empty ID is the
// legacy `upstream` block, which existing callers keep addressing as nil.
type Endpoint struct {
	ID        string
	BaseURL   string
	APIKey    string
	Protocols []string
}

// ValidateBaseURL enforces that a base URL is configuration, not input:
// credentials in the URL, query strings and fragments are rejected rather than
// quietly stripped.
func ValidateBaseURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", ErrInvalidBaseURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrInvalidBaseURL
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidBaseURL
	}
	if parsed.Host == "" {
		return "", ErrInvalidBaseURL
	}
	if parsed.Scheme == "http" {
		switch parsed.Hostname() {
		case "127.0.0.1", "::1", "localhost":
		default:
			return "", ErrInvalidBaseURL
		}
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// HTTPBackend holds every configured connection at once and dispatches each
// request by connection id.
type HTTPBackend struct {
	endpoints map[string]*Endpoint
	models    []Model
	client    *http.Client
	// now is injectable so a test can pin `created_at`.
	now func() int64
}

// NewBackend builds a backend from a validated catalog and endpoint table.
// `key` is the map key for the legacy block: the empty string.
func NewBackend(endpoints []Endpoint, models []Model) (*HTTPBackend, error) {
	table := make(map[string]*Endpoint, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.ID != "" {
			if !connectionIDPattern(endpoint.ID) {
				return nil, ErrInvalidConnection
			}
		}
		if endpoint.APIKey == "" {
			return nil, ErrInvalidAPIKey
		}
		base, err := ValidateBaseURL(endpoint.BaseURL)
		if err != nil {
			return nil, err
		}
		copied := endpoint
		copied.BaseURL = base
		if copied.ID != "" {
			if _, dup := table[copied.ID]; dup {
				return nil, ErrInvalidConnection
			}
		} else if _, dup := table[""]; dup {
			return nil, ErrInvalidConnection
		}
		table[copied.ID] = &copied
	}
	return &HTTPBackend{endpoints: table, models: models,
		client: &http.Client{
			// A redirect would replay the Authorization header at a host that
			// answered 302, so it is refused instead of followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: func() int64 { return time.Now().Unix() }}, nil
}

// Models returns the catalog this backend serves.
func (b *HTTPBackend) Models() []Model { return b.models }

// Has reports whether this live process can reach a connection.
func (b *HTTPBackend) Has(connectionID *string) bool {
	_, ok := b.endpoints[keyOfConnection(connectionID)]
	return ok
}

// ConnectionIDs lists every named connection, without its credential.
func (b *HTTPBackend) ConnectionIDs() []string {
	out := make([]string, 0, len(b.endpoints))
	for id := range b.endpoints {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func keyOfConnection(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// Connection resolves one endpoint, refusing an unknown id rather than falling
// back to the legacy block.
func (b *HTTPBackend) Connection(connectionID *string) (*Endpoint, error) {
	endpoint, ok := b.endpoints[keyOfConnection(connectionID)]
	if !ok {
		if connectionID == nil {
			return nil, ErrConnectionUnavailable
		}
		return nil, ErrInvalidConnectionID
	}
	return endpoint, nil
}

// Request forwards one Kimi request. `payload` is the already-normalised body
// (model rewritten to the upstream id, instructions applied).
func (b *HTTPBackend) Request(ctx context.Context, route string, payload []byte, options RequestOptions) (*UpstreamResponse, error) {
	if route != ProtocolChat && route != ProtocolResponses {
		return nil, ErrInvalidProtocol
	}
	endpoint, err := b.Connection(options.ConnectionID)
	if err != nil {
		return nil, err
	}
	if len(endpoint.Protocols) > 0 && !contains(endpoint.Protocols, route) {
		return nil, ErrInvalidProtocol
	}
	if options.Model != nil && options.Model.IsAnthropic() {
		return b.anthropic(ctx, route, payload, options, endpoint)
	}
	path := "/chat/completions"
	if route == ProtocolResponses {
		path = "/responses"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	for name, value := range options.Headers {
		request.Header.Set(name, value)
	}
	reply, err := b.do(request)
	if err != nil {
		return nil, err
	}
	return &UpstreamResponse{Status: reply.StatusCode, Header: reply.Header, Body: reply.Body}, nil
}

type preSendError struct {
	err error
}

func (e *preSendError) Error() string { return e.err.Error() }
func (e *preSendError) Unwrap() error { return e.err }

func (b *HTTPBackend) do(request *http.Request) (*http.Response, error) {
	var gotConn atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			gotConn.Store(true)
		},
	}
	traced := request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	reply, err := b.client.Do(traced)
	if err != nil {
		var urlErr *url.Error
		if !gotConn.Load() && errors.As(err, &urlErr) {
			return nil, &preSendError{err: err}
		}
		return nil, err
	}
	return reply, nil
}

// anthropic converts both directions for one Messages upstream.
func (b *HTTPBackend) anthropic(ctx context.Context, route string, payload []byte, options RequestOptions, endpoint *Endpoint) (*UpstreamResponse, error) {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, ErrInvalidRequest
	}
	body, err := ToAnthropicRequest(route, decoded)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.BaseURL+"/messages", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	// Anthropic authenticates with x-api-key and ignores a Bearer header, so
	// sending only the OpenAI shape would come back as a 401 that reads like a
	// bad key.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", endpoint.APIKey)
	request.Header.Set("anthropic-version", AnthropicVersion)
	for name, value := range options.Headers {
		request.Header.Set(name, value)
	}
	reply, err := b.do(request)
	if err != nil {
		return nil, err
	}
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		// A failed request keeps the vendor's own status and error envelope,
		// rewritten into the shape Kimi already parses.
		raw, _ := io.ReadAll(io.LimitReader(reply.Body, UsageScanLimit))
		_ = reply.Body.Close()
		body := AnthropicErrorBody(raw, reply.StatusCode)
		header := reply.Header.Clone()
		header.Set("Content-Type", "application/json")
		return &UpstreamResponse{Status: reply.StatusCode, Header: header,
			Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
	streamed, _ := body["stream"].(bool)
	if !streamed {
		raw, err := io.ReadAll(io.LimitReader(reply.Body, 64*1024*1024))
		_ = reply.Body.Close()
		if err != nil {
			return nil, err
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil || message == nil {
			// A reply this cannot parse is an upstream contract violation, not
			// an empty answer.
			return nil, ErrInvalidUpstreamResponse
		}
		var shaped map[string]any
		if route == ProtocolChat {
			shaped, err = FromAnthropicChat(message, b.now())
		} else {
			shaped, err = FromAnthropicResponses(message, b.now())
		}
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(shaped)
		header := http.Header{}
		header.Set("Content-Type", "application/json")
		return &UpstreamResponse{Status: reply.StatusCode, Header: header,
			Body: io.NopCloser(bytes.NewReader(encoded))}, nil
	}
	translator, err := NewStreamTranslator(route, 1024*1024)
	if err != nil {
		_ = reply.Body.Close()
		return nil, err
	}
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	return &UpstreamResponse{Status: reply.StatusCode, Header: header,
		Body: &translatedBody{upstream: reply.Body, translator: translator}}, nil
}

// translatedBody pulls the upstream lazily, so a client that goes away cancels
// the upstream body through the gateway's own cancellation instead of draining
// a reply nobody will read.
type translatedBody struct {
	upstream   io.ReadCloser
	translator *StreamTranslator
	buf        []byte
	pending    []byte
	finished   bool
}

func (t *translatedBody) Read(p []byte) (int, error) {
	if len(t.buf) == 0 {
		t.buf = make([]byte, 32*1024)
	}
	for len(t.pending) == 0 {
		if t.finished {
			return 0, io.EOF
		}
		n, err := t.upstream.Read(t.buf)
		if n > 0 {
			t.pending = append(t.pending, t.translator.Push(t.buf[:n])...)
			continue
		}
		if err == io.EOF {
			t.finished = true
			t.pending = append(t.pending, t.translator.End()...)
			if len(t.pending) == 0 {
				return 0, io.EOF
			}
			continue
		}
		if err != nil {
			t.finished = true
			return 0, err
		}
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	if len(t.pending) == 0 {
		t.pending = nil
	}
	return n, nil
}

func (t *translatedBody) Close() error {
	t.finished = true
	return t.upstream.Close()
}
