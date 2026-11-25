package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

type connectionProbeRequest struct {
	ConnectionID string `json:"connectionId"`
	PresetID     string `json:"presetId"`
	BaseURL      string `json:"baseUrl"`
	APIKey       string `json:"apiKey"`
	Model        string `json:"model"`
	Protocol     string `json:"protocol"`
	TimeoutMs    int64  `json:"timeoutMs"`
}

func (s *Service) probeConnection(ctx context.Context, in core.Document) (protocol.ProbeResult, error) {
	var request connectionProbeRequest
	raw, _ := json.Marshal(in)
	if json.Unmarshal(raw, &request) != nil || request.TimeoutMs < 0 || len(request.Model) > 2048 || strings.ContainsAny(request.Model, "\r\n\x00") {
		return protocol.ProbeResult{}, errors.New("invalid_probe_request")
	}
	request.BaseURL = strings.TrimSpace(request.BaseURL)
	request.Model = strings.TrimSpace(request.Model)
	var saved map[string]any
	if request.ConnectionID != "" {
		d, _, err := s.Store.Read()
		if err != nil {
			return protocol.ProbeResult{}, err
		}
		for _, row := range core.Rows(d["connections"]) {
			if connection := core.Object(row); connection["id"] == request.ConnectionID {
				saved = connection
				break
			}
		}
		if saved == nil {
			return protocol.ProbeResult{}, errors.New("connection_unavailable")
		}
		if request.BaseURL == "" {
			request.BaseURL = core.String(saved["baseURL"])
		}
		if request.PresetID == "" {
			request.PresetID = core.String(saved["providerPresetId"])
		}
		if _, provided := in["protocol"]; !provided {
			request.Protocol = core.ConnectionProtocol(saved)
			if request.Protocol == "chat" && (request.PresetID == "anthropic" || strings.Contains(strings.ToLower(request.BaseURL), "anthropic")) {
				request.Protocol = "anthropic"
			}
		}
	}
	base, err := core.ValidateBaseURL(request.BaseURL)
	if err != nil {
		return protocol.ProbeResult{}, err
	}
	switch request.Protocol {
	case "", "auto", "chat", "responses", "anthropic":
	default:
		return protocol.ProbeResult{}, errors.New("invalid_probe_protocol")
	}
	if request.APIKey == "" && saved != nil {
		savedBase, _ := core.ValidateBaseURL(core.String(saved["baseURL"]))
		if base == savedBase {
			request.APIKey, err = credentialFor(saved, s.Keys)
			if err != nil {
				return protocol.ProbeResult{}, errors.New("credentials_unavailable")
			}
		}
	}
	if strings.ContainsAny(request.APIKey, "\r\n\x00") {
		return protocol.ProbeResult{}, errors.New("invalid_api_key")
	}
	timeout := request.TimeoutMs
	if timeout == 0 {
		timeout = 15000
	}
	deadline, cancel := context.WithTimeout(ctx, time.Duration(min(timeout, 30000))*time.Millisecond)
	defer cancel()
	result := protocol.Probe(deadline, protocol.ProbeRequest{
		BaseURL: base, APIKey: strings.TrimSpace(request.APIKey), Model: request.Model, PresetID: request.PresetID, Protocol: request.Protocol,
	})
	if result.OK {
		label := map[string]string{"chat": "Chat Completions", "responses": "Responses", "anthropic": "Anthropic Messages"}
		result.Message = fmt.Sprintf("连接测试通过（%s · %d ms）", label[result.DetectedProtocol], result.LatencyMs)
	} else {
		result.Message = message(result.Reason)
	}
	return result, nil
}
