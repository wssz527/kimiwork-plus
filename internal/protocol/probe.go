package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ProbeRequest struct {
	BaseURL  string
	APIKey   string
	PresetID string
	Model    string
	Protocol string
}

type ProbeModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ProbeCatalog struct {
	Supported  bool         `json:"supported"`
	ModelCount int          `json:"modelCount"`
	ModelFound bool         `json:"modelFound"`
	Models     []ProbeModel `json:"models"`
}

type ProbeResult struct {
	OK                 bool         `json:"ok"`
	State              string       `json:"state"`
	Reason             string       `json:"reason"`
	HTTPStatus         int          `json:"httpStatus"`
	LatencyMs          int64        `json:"latencyMs"`
	DetectedProtocol   string       `json:"detectedProtocol"`
	ConnectionProtocol string       `json:"connectionProtocol"`
	UpstreamProtocol   string       `json:"upstreamProtocol"`
	TriedProtocols     []string     `json:"triedProtocols"`
	TestedModel        string       `json:"testedModel"`
	Catalog            ProbeCatalog `json:"catalog"`
	Message            string       `json:"message"`
	Detail             string       `json:"detail"`
}

func probeProtocols(in ProbeRequest) []string {
	if in.Protocol != "" && in.Protocol != "auto" {
		return []string{in.Protocol}
	}
	model := strings.ToLower(in.Model)
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	if strings.Contains(strings.ToLower(in.BaseURL), "anthropic") || in.PresetID == ProtocolAnthropic {
		return []string{ProtocolAnthropic, ProtocolChat, ProtocolResponses}
	}
	preferResponses := in.PresetID == "openai" || strings.HasSuffix(in.PresetID, "-responses")
	if (model == "" && preferResponses) || strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.Contains(model, "-codex") {
		return []string{ProtocolResponses, ProtocolChat, ProtocolAnthropic}
	}
	return []string{ProtocolChat, ProtocolResponses, ProtocolAnthropic}
}

func Probe(ctx context.Context, in ProbeRequest) (result ProbeResult) {
	started := time.Now()
	defer func() { result.LatencyMs = time.Since(started).Milliseconds() }()
	result.State = "failed"
	result.TriedProtocols = []string{}
	result.Catalog.Models = []ProbeModel{}
	if in.APIKey == "" {
		result.Reason = "probe_missing_key"
		return result
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	protocols := probeProtocols(in)
	catalogCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	resp, err := probeHTTP(catalogCtx, client, in, protocols[0], "/models", nil)
	model := in.Model
	if err == nil {
		result.HTTPStatus = resp.StatusCode
		if result.HTTPStatus == http.StatusOK {
			var catalog struct {
				Data []struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					DisplayName string `json:"display_name"`
				} `json:"data"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&catalog) == nil && catalog.Data != nil {
				result.Catalog.Supported = true
				seen := map[string]bool{}
				for _, row := range catalog.Data {
					if row.ID == "" || seen[row.ID] {
						continue
					}
					seen[row.ID] = true
					if model == "" {
						model = row.ID
					}
					result.Catalog.ModelCount++
					if len(result.Catalog.Models) < 50 {
						name := row.Name
						if name == "" {
							name = row.DisplayName
						}
						if name == "" {
							name = row.ID
						}
						result.Catalog.Models = append(result.Catalog.Models, ProbeModel{probeRedact(row.ID, in.APIKey), probeRedact(name, in.APIKey)})
					}
				}
				result.Catalog.ModelFound = seen[model]
			}
		} else {
			result.Detail, _ = probeDetail(resp.Body, in.APIKey)
		}
		resp.Body.Close()
	}
	cancel()
	if probeUnauthorized(result.HTTPStatus) {
		result.Reason = "probe_unauthorized"
		return result
	}
	if model == "" {
		switch {
		case result.Catalog.Supported:
			result.pass(protocols[0])
		case ctx.Err() != nil:
			result.Reason = "probe_timeout"
		case result.HTTPStatus == 0:
			result.Reason = "probe_unreachable"
		default:
			result.Reason = "probe_model_required"
		}
		return result
	}
	result.TestedModel = probeRedact(model, in.APIKey)
	in.Model = model
	for _, wire := range probeProtocols(in) {
		if ctx.Err() != nil {
			break
		}
		result.TriedProtocols = append(result.TriedProtocols, wire)
		status, detail, ok := probeInference(ctx, client, in, wire)
		result.HTTPStatus, result.Detail = status, detail
		if ok {
			result.pass(wire)
			return result
		}
		if probeUnauthorized(status) {
			result.Reason = "probe_unauthorized"
			return result
		}
	}
	switch {
	case ctx.Err() != nil:
		result.Reason = "probe_timeout"
	case result.HTTPStatus == 0:
		result.Reason = "probe_unreachable"
	default:
		result.Reason = "probe_inference_failed"
	}
	return result
}

func (r *ProbeResult) pass(wire string) {
	r.State, r.OK, r.Detail = "passed", true, ""
	r.DetectedProtocol, r.ConnectionProtocol = wire, wire
	if wire == ProtocolAnthropic {
		r.ConnectionProtocol, r.UpstreamProtocol = ProtocolChat, ProtocolAnthropic
	}
}

func probeUnauthorized(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func probeHTTP(ctx context.Context, client *http.Client, in ProbeRequest, wire, path string, payload []byte) (*http.Response, error) {
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, in.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if wire == ProtocolAnthropic {
		req.Header.Set("x-api-key", in.APIKey)
		req.Header.Set("anthropic-version", AnthropicVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+in.APIKey)
	}
	return client.Do(req)
}

func probeInference(ctx context.Context, client *http.Client, in ProbeRequest, wire string) (int, string, bool) {
	payload := map[string]any{"model": in.Model, "stream": false}
	path := "/chat/completions"
	if wire == ProtocolResponses {
		path = "/responses"
		payload["input"], payload["max_output_tokens"] = "ping", 16
	} else {
		if wire == ProtocolAnthropic {
			path = "/messages"
		}
		payload["messages"] = []map[string]string{{"role": "user", "content": "ping"}}
		payload["max_tokens"] = 16
	}
	status, detail, retry, ok := probeAttempt(ctx, client, in, wire, path, payload, false)
	if retry {
		payload["stream"] = true
		status, detail, _, ok = probeAttempt(ctx, client, in, wire, path, payload, true)
	}
	return status, detail, ok
}

func probeAttempt(ctx context.Context, client *http.Client, in ProbeRequest, wire, path string, payload map[string]any, stream bool) (int, string, bool, bool) {
	body, _ := json.Marshal(payload)
	resp, err := probeHTTP(ctx, client, in, wire, path, body)
	if err != nil {
		return 0, "", false, false
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		if !stream || probeFrame(resp.Body) {
			return status, "", false, true
		}
		return status, "", false, false
	}
	detail, requiresStream := probeDetail(resp.Body, in.APIKey)
	return status, detail, !stream && !probeUnauthorized(status) && requiresStream, false
}

func probeFrame(body io.Reader) bool {
	scan := bufio.NewScanner(io.LimitReader(body, 256<<10))
	scan.Buffer(make([]byte, 4096), 256<<10)
	for scan.Scan() {
		data, ok := strings.CutPrefix(scan.Text(), "data:")
		if !ok {
			continue
		}
		var frame map[string]json.RawMessage
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &frame) == nil && len(frame) > 0 {
			switch textOf(frame["type"]) {
			case "ping":
				continue
			case "error", "response.failed":
				return false
			}
			return frame["error"] == nil
		}
	}
	return false
}

func probeDetail(body io.Reader, key string) (string, bool) {
	const maxDetailBody = 16 << 10
	raw, _ := io.ReadAll(io.LimitReader(body, maxDetailBody+1))
	if len(raw) > maxDetailBody {
		return "", false
	}
	detail := string(raw)
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		var nested struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(envelope.Error, &nested)
		for _, candidate := range []string{nested.Message, textOf(envelope.Error), envelope.Message} {
			if candidate != "" {
				detail = candidate
				break
			}
		}
	}
	lower := strings.ToLower(detail)
	requiresStream := strings.Contains(lower, "stream") && (strings.Contains(lower, "must") || strings.Contains(lower, "required") || strings.Contains(lower, "only") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "not support"))
	detail = strings.Join(strings.Fields(probeRedact(detail, key)), " ")
	if chars := []rune(detail); len(chars) > 200 {
		detail = string(chars[:200])
	}
	return detail, requiresStream
}

func probeRedact(value, key string) string {
	encoded, _ := json.Marshal(key)
	for _, secret := range []string{key, url.QueryEscape(key), url.PathEscape(key), string(encoded[1 : len(encoded)-1])} {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}
