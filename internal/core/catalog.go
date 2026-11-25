package core

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

//go:embed providers.json
var providersJSON []byte

var modelCatalogHintsJSON, _ = json.Marshal(modelCatalogHints)

func providerPresets() Document {
	var d Document
	_ = json.Unmarshal(providersJSON, &d)
	return d
}

func Providers() Document {
	d := providerPresets()
	var hints []any
	_ = json.Unmarshal(modelCatalogHintsJSON, &hints)
	d["modelCatalogHints"] = hints
	return d
}
func Preset(id string) map[string]any {
	for _, v := range Rows(providerPresets()["presets"]) {
		p := Object(v)
		if p["id"] == id {
			return p
		}
	}
	return nil
}

var effortPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func textLimit(v any, max int) string {
	s := strings.TrimSpace(String(v))
	if len(utf16.Encode([]rune(s))) > max {
		return ""
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return ""
		}
	}
	return s
}
func catalogProtocols(row, preset map[string]any) []any {
	status := map[string]string{"chat": "absent", "responses": "absent"}
	stated := false
	declared := 0
	if ps, ok := row["protocols"].([]any); ok {
		stated = true
		for _, v := range ps[:min(len(ps), 8)] {
			p := String(v)
			if p == "chat" || p == "responses" {
				declared++
				status[p] = "yes"
			}
		}
	}
	if caps := Object(row["api_capabilities"]); caps != nil {
		stated = true
		for name, v := range caps {
			p := ""
			switch name {
			case "responses":
				p = "responses"
			case "chat", "chat_completions", "completions":
				p = "chat"
			}
			if p == "" {
				continue
			}
			declared++
			verdict := "unreadable"
			if v == nil || v == false {
				verdict = "no"
			} else if v == true {
				verdict = "yes"
			} else if obj := Object(v); obj != nil {
				if supported, ok := obj["supported"]; !ok || supported == true {
					verdict = "yes"
				} else if supported == nil || supported == false {
					verdict = "no"
				}
			}
			if verdict == "no" {
				status[p] = "no"
			} else if verdict == "yes" && status[p] != "no" {
				status[p] = "yes"
			} else if status[p] == "absent" {
				status[p] = verdict
			}
		}
	}
	out := []any{}
	if stated && declared == 0 {
		return out
	}
	affirmative := status["chat"] == "yes" || status["responses"] == "yes"
	allowed := Rows(preset["protocols"])
	for _, p := range []string{"chat", "responses"} {
		inPreset := false
		for _, v := range allowed {
			if v == p {
				inPreset = true
			}
		}
		if affirmative {
			if status[p] == "yes" && (len(allowed) == 0 || inPreset) {
				out = append(out, p)
			}
		} else if inPreset && status[p] == "absent" {
			out = append(out, p)
		}
	}
	return out
}
func ParseDiscovered(rows []any, presetID, sourceURL, checkedAt string) []any {
	p := Preset(presetID)
	out := []any{}
	seen := map[string]bool{}
	source := Document{"checkedAt": checkedAt[:min(len(checkedAt), 10)]}
	if u, e := url.Parse(sourceURL); e == nil && u.Scheme == "https" && u.User == nil {
		source["url"] = u.String()
	}
	for _, v := range rows[:min(len(rows), 2000)] {
		r := Object(v)
		id := textLimit(r["id"], 200)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m := Document{"id": id, "protocols": catalogProtocols(r, p), "source": source}
		name := textLimit(r["name"], 200)
		if name == "" {
			name = textLimit(r["display_name"], 200)
		}
		if name != "" && name != id {
			m["name"] = name
		}
		for target, fields := range map[string][]string{"contextWindow": {"context_window", "contextWindow", "context_length"}, "maxOutputTokens": {"max_output_tokens", "maxOutputTokens"}, "tools": {"tools", "supports_tools", "supports_tool_call", "tool_call"}, "reasoning": {"reasoning", "supports_reasoning"}} {
			for _, key := range fields {
				value, ok := r[key]
				if !ok || value == nil {
					continue
				}
				if target == "tools" || target == "reasoning" {
					if b, ok := value.(bool); ok {
						m[target] = b
						break
					}
					continue
				}
				max := float64(100000000)
				if target == "maxOutputTokens" {
					max = 10000000
				}
				if n, ok := value.(float64); ok && n > 0 && n <= max && n == float64(int64(n)) {
					m[target] = n
				}
				break
			}
		}
		if c, ok := m["contextWindow"].(float64); ok {
			if n, ok := m["maxOutputTokens"].(float64); ok && n > c {
				delete(m, "maxOutputTokens")
			}
		}
		modalities := Rows(r["input_modalities"])
		if modalities == nil {
			modalities = Rows(r["inputModalities"])
		}
		if modalities == nil {
			modalities = Rows(Object(r["modalities"])["input"])
		}
		mods := uniqueStrings(modalities, 8, 2, func(s string) bool { return s == "text" || s == "image" })
		if len(mods) > 0 {
			m["inputModalities"] = mods
		}
		efforts := Rows(Object(r["effort"])["supported_levels"])
		if efforts == nil {
			efforts = Rows(r["reasoning_efforts"])
		}
		if efforts == nil {
			efforts = Rows(r["reasoningEfforts"])
		}
		if efforts == nil {
			efforts = Rows(r["reasoning_levels"])
		}
		if efforts == nil {
			for _, row := range Rows(r["supported_reasoning_levels"]) {
				if effort := String(Object(row)["effort"]); effort != "" {
					efforts = append(efforts, effort)
				}
			}
		}
		es := uniqueStrings(efforts, 32, 16, effortPattern.MatchString)
		if len(es) > 0 {
			m["reasoningEfforts"] = es
		}
		aliases := uniqueStrings(Rows(r["aliases"]), 16, 8, func(s string) bool { return s != "" && s != id && textLimit(s, 200) == s })
		if len(aliases) > 0 {
			m["aliases"] = aliases
		}
		if desc := textLimit(r["description"], 1000); desc != "" {
			m["description"] = desc
		}
		out = append(out, m)
		if len(out) >= 500 {
			break
		}
	}
	return out
}
func uniqueStrings(values []any, scan, limit int, valid func(string) bool) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, v := range values[:min(len(values), scan)] {
		s, ok := v.(string)
		if ok && !seen[s] && valid(s) {
			seen[s] = true
			out = append(out, s)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}
func Discover(ctx context.Context, base, key, preset string) (Document, error) {
	base, e := BaseURL(base)
	if e != nil {
		return nil, e
	}
	if key == "" {
		return nil, errors.New("invalid_api_key")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	if e != nil {
		return nil, e
	}
	if preset == "anthropic" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", protocol.AnthropicVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	client := http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := client.Do(req)
	if e != nil {
		reason := "discovery_unavailable"
		if ctx.Err() != nil {
			reason = "discovery_timeout"
		}
		return Document{"state": "failed", "reason": reason, "models": []any{}, "modelCount": 0}, nil
	}
	defer res.Body.Close()
	failed := func(reason string) Document {
		return Document{"state": "failed", "reason": reason, "httpStatus": res.StatusCode, "models": []any{}, "modelCount": 0}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return failed("discovery_http_error"), nil
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	if e != nil {
		return failed("discovery_unavailable"), nil
	}
	if len(b) > 2*1024*1024 {
		return failed("discovery_too_large"), nil
	}
	var payload any
	if json.Unmarshal(b, &payload) != nil {
		return failed("discovery_invalid_catalog"), nil
	}
	rows := Rows(payload)
	if rows == nil {
		obj := Object(payload)
		rows = Rows(obj["data"])
		if rows == nil {
			rows = Rows(obj["models"])
		}
	}
	if rows == nil {
		return failed("discovery_invalid_catalog"), nil
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	models := ParseDiscovered(rows, preset, base+"/models", checkedAt)
	return Document{"state": "passed", "models": models, "modelCount": len(models), "httpStatus": res.StatusCode, "source": "discovery", "sourceURL": base + "/models", "presetId": preset, "checkedAt": checkedAt}, nil
}
