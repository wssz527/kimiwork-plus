package bridge

import (
	"context"
	"errors"
	"kimiworkplus.local/migration/internal/core"
	"strings"
)

func discoveryKey(c map[string]any) string {
	return core.String(c["id"]) + "\n" + core.String(c["baseURL"])
}
func (s *Service) discover(ctx context.Context, in core.Document) (any, error) {
	d, rev, e := s.Store.Read()
	if e != nil {
		return nil, e
	}
	if v, ok := in["revision"]; ok && v != rev {
		return nil, core.ErrConflict
	}
	base, key, preset, id := core.String(in["baseURL"]), core.String(in["apiKey"]), core.String(in["presetId"]), core.String(in["connectionId"])
	if base != "" {
		base, e = core.BaseURL(base)
		if e != nil {
			return nil, e
		}
	}
	if preset != "" && core.Preset(preset) == nil {
		return nil, errors.New("invalid_discovery")
	}
	var saved map[string]any
	if id != "" {
		for _, v := range core.Rows(d["connections"]) {
			c := core.Object(v)
			if c["id"] == id {
				saved = c
				break
			}
		}
		if saved == nil {
			return nil, errors.New("connection_unavailable")
		}
		if base == "" || base == saved["baseURL"] {
			base = core.String(saved["baseURL"])
			if own := core.String(saved["providerPresetId"]); own != "" {
				preset = own
			}
			if key == "" {
				key, e = credentialFor(saved, s.Keys)
				if e != nil {
					return nil, errors.New("credentials_unavailable")
				}
			}
		} else if key == "" {
			return nil, errors.New("invalid_api_key")
		}
	}
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("invalid_api_key")
	}
	result, e := core.Discover(ctx, base, key, preset)
	if e != nil {
		return nil, e
	}
	result["connectionId"] = nil
	if id != "" {
		result["connectionId"] = id
	}
	if result["state"] == "passed" && saved != nil && base == saved["baseURL"] {
		s.mu.Lock()
		if s.discoveries == nil {
			s.discoveries = map[string]core.Document{}
		}
		s.discoveries[discoveryKey(saved)] = result
		s.mu.Unlock()
	}
	return result, nil
}
func (s *Service) search(in core.Document) (any, error) {
	query, ok := in["query"].(string)
	if !ok || len([]rune(query)) > 200 {
		return nil, errors.New("invalid_model_search")
	}
	presetID := core.String(in["presetId"])
	preset := core.Preset(presetID)
	if presetID != "" && preset == nil {
		return nil, errors.New("invalid_model_search")
	}
	var discovery core.Document
	if id := core.String(in["connectionId"]); id != "" {
		d, _, e := s.Store.Read()
		if e != nil {
			return nil, e
		}
		var c map[string]any
		for _, v := range core.Rows(d["connections"]) {
			if core.Object(v)["id"] == id {
				c = core.Object(v)
				break
			}
		}
		if c == nil {
			return nil, errors.New("connection_unavailable")
		}
		if own := core.String(c["providerPresetId"]); own != "" {
			if presetID != "" && own != presetID {
				return nil, errors.New("preset_connection_mismatch")
			}
			preset = core.Preset(own)
		}
		s.mu.RLock()
		discovery = s.discoveries[discoveryKey(c)]
		s.mu.RUnlock()
	}
	return core.SearchCatalog(preset, discovery, query), nil
}
