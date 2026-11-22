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
				return m, subagentAlias(m), nil
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
		if rawRoles, exists := in["roles"]; exists && rawRoles != nil {
			roleMap := Object(rawRoles)
			if roleMap == nil {
				return nil, errors.New("invalid_subagent_roles")
			}
			roles := Document{}
			for roleKey, rawID := range roleMap {
				if roleKey != "plan" && roleKey != "coder" && roleKey != "explore" {
					return nil, errors.New("invalid_subagent_roles")
				}
				if rawID == nil {
					continue
				}
				roleModelID, ok := rawID.(string)
				if !ok {
					return nil, errors.New("invalid_subagent_roles")
				}
				roleModelID = strings.TrimSpace(roleModelID)
				if roleModelID == "" {
					continue
				}
				if _, _, err := resolve(roleModelID); err != nil {
					return nil, err
				}
				if !seen[roleModelID] {
					return nil, errors.New("invalid_subagent_roles")
				}
				roles[roleKey] = roleModelID
			}
			if len(roles) > 0 {
				out["roles"] = roles
			}
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

// subagentAlias reproduces the official import rule: only a model whose last
// identifier segment starts with "kimi-" joins the kimi family. Every other model,
// including a chat-protocol one, is imported as a responses model, and a block
// naming the wrong family is dropped because the alias is not declared.
func subagentAlias(model map[string]any) string {
	id := String(model["id"])
	if model["protocol"] == "responses" {
		return "byok-kth-resp/" + id
	}
	last := id
	if at := strings.LastIndex(id, "/"); at >= 0 {
		last = id[at+1:]
	}
	if strings.HasPrefix(strings.ToLower(last), "kimi-") {
		return "byok-kth-kimi/" + id
	}
	return "byok-kth-resp/" + id
}
