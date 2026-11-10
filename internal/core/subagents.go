package core

import (
	"errors"
	"strings"
	"unicode"
)

func NormalizeSubagents(value any, models []any) (Document, error) {
	if value == nil {
		return Document{"mode": "inherit"}, nil
	}
	in := Object(value)
	if in == nil {
		return nil, errors.New("invalid_subagent_policy")
	}
	mode := String(in["mode"])
	if mode == "" {
		mode = "inherit"
	}
	if mode != "inherit" && mode != "force" && mode != "pool" {
		return nil, errors.New("invalid_subagent_policy")
	}
	if v, exists := in["transport"]; exists && v != "native-config" {
		return nil, errors.New("invalid_subagent_transport")
	}
	out := Document{"mode": mode, "transport": "native-config"}
	if mode == "inherit" {
		return out, nil
	}
	resolve := func(id any) (map[string]any, string, error) {
		for _, row := range models {
			m := Object(row)
			if m["id"] == id {
				if m["tools"] != true {
					return nil, "", errors.New("subagent_model_tools_required")
				}
				prefix := "byok-kth-kimi/"
				if m["protocol"] == "responses" {
					prefix = "byok-kth-resp/"
				}
				return m, prefix + String(id), nil
			}
		}
		return nil, "", errors.New("subagent_model_unavailable")
	}
	model, alias, err := resolve(in["model"])
	if err != nil {
		return nil, err
	}
	out["model"] = model["id"]
	out["modelAlias"] = alias
	if mode == "pool" {
		rows := Rows(in["models"])
		if len(rows) == 0 || len(rows) > 32 {
			return nil, errors.New("invalid_subagent_pool")
		}
		seen := map[string]bool{}
		pool := []any{}
		for _, row := range rows {
			r := Object(row)
			id := String(r["id"])
			if seen[id] {
				return nil, errors.New("invalid_subagent_pool")
			}
			seen[id] = true
			_, alias, err := resolve(id)
			if err != nil {
				return nil, err
			}
			description := String(r["description"])
			if strings.TrimSpace(description) == "" || utfLen(description) > 1000 || strings.IndexFunc(description, unicode.IsControl) >= 0 {
				return nil, errors.New("invalid_subagent_description")
			}
			pool = append(pool, Document{"id": id, "modelAlias": alias, "description": strings.TrimSpace(description)})
		}
		if !seen[String(model["id"])] {
			return nil, errors.New("subagent_pool_default_required")
		}
		if _, exists := in["effort"]; exists {
			return nil, errors.New("invalid_subagent_effort")
		}
		out["models"] = pool
		return out, nil
	}
	effort, exists := in["effort"]
	if !exists || effort == nil {
		effort, exists = model["defaultEffort"]
	}
	if exists {
		valid := effort == "off"
		if model["reasoning"] == true {
			for _, v := range Rows(model["reasoningEfforts"]) {
				valid = valid || v == effort
			}
		}
		if !effortPattern.MatchString(String(effort)) || !valid {
			return nil, errors.New("invalid_subagent_effort")
		}
		out["effort"] = effort
	}
	return out, nil
}
