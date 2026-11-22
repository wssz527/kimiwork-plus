package core

import "testing"

// The alias must follow the official import rule, not the protocol. A chat model
// whose id does not start with "kimi-" is imported by the official application as
// a responses model, so a block naming the kimi family is dropped as undeclared
// and the policy silently never applies.
func TestSubagentAliasFollowsTheOfficialImportRule(t *testing.T) {
	for _, tc := range []struct {
		id, protocol, alias string
	}{
		{"kimi-alpha", "chat", "byok-kth-kimi/kimi-alpha"},
		{"moonshot/kimi-k2", "chat", "byok-kth-kimi/moonshot/kimi-k2"},
		{"group/glm-5-3-flash", "chat", "byok-kth-resp/group/glm-5-3-flash"},
		{"group/deepseek-v4-1-flash", "chat", "byok-kth-resp/group/deepseek-v4-1-flash"},
		{"kimi-resp", "responses", "byok-kth-resp/kimi-resp"},
		{"group/KIMI-K2", "chat", "byok-kth-kimi/group/KIMI-K2"},
		{"kimi", "chat", "byok-kth-resp/kimi"},
	} {
		t.Run(tc.id+"/"+tc.protocol, func(t *testing.T) {
			models := []any{Document{"id": tc.id, "protocol": tc.protocol, "tools": true}}
			policy, err := NormalizeSubagents(Document{"mode": "force", "model": tc.id}, models)
			if err != nil {
				t.Fatal(err)
			}
			if String(policy["modelAlias"]) != tc.alias {
				t.Fatalf("alias for %s is %q, want %q", tc.id, String(policy["modelAlias"]), tc.alias)
			}
		})
	}
}

// A pool model carries the same alias as a single default, so a pool row and a
// force default for one model cannot disagree about which family imported it.
func TestSubagentPoolRowsUseTheSameAlias(t *testing.T) {
	models := []any{
		Document{"id": "group/glm-5-3-flash", "protocol": "chat", "tools": true},
		Document{"id": "moonshot/kimi-k2", "protocol": "chat", "tools": true},
	}
	policy, err := NormalizeSubagents(Document{"mode": "pool", "model": "moonshot/kimi-k2", "models": []any{
		Document{"id": "group/glm-5-3-flash", "description": "general"},
		Document{"id": "moonshot/kimi-k2", "description": "long context"},
	}}, models)
	if err != nil {
		t.Fatal(err)
	}
	aliases := map[string]string{}
	for _, row := range Rows(policy["models"]) {
		entry := Object(row)
		aliases[String(entry["id"])] = String(entry["modelAlias"])
	}
	if aliases["group/glm-5-3-flash"] != "byok-kth-resp/group/glm-5-3-flash" {
		t.Fatalf("pool alias for glm: %q", aliases["group/glm-5-3-flash"])
	}
	if aliases["moonshot/kimi-k2"] != "byok-kth-kimi/moonshot/kimi-k2" {
		t.Fatalf("pool alias for kimi: %q", aliases["moonshot/kimi-k2"])
	}
	if String(policy["modelAlias"]) != "byok-kth-kimi/moonshot/kimi-k2" {
		t.Fatalf("default alias: %q", String(policy["modelAlias"]))
	}
}

func TestSubagentRolesValidationAndPersistence(t *testing.T) {
	models := []any{
		Document{"id": "magpie/deepseek", "protocol": "chat", "tools": true},
		Document{"id": "magpie/glm", "protocol": "chat", "tools": true},
		Document{"id": "magpie/no-tools", "protocol": "chat", "tools": false},
	}
	policy, err := NormalizeSubagents(Document{
		"mode":  "pool",
		"model": "magpie/deepseek",
		"models": []any{
			Document{"id": "magpie/deepseek", "description": "默认子代理；用于 coder 代码编写"},
			Document{"id": "magpie/glm", "description": "用于 plan 规划与审查"},
		},
		"roles": Document{
			"plan":    "magpie/glm",
			"coder":   "magpie/deepseek",
			"explore": "",
		},
	}, models)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	roles := Object(policy["roles"])
	if String(roles["plan"]) != "magpie/glm" || String(roles["coder"]) != "magpie/deepseek" {
		t.Fatalf("unexpected roles: %#v", roles)
	}
	if _, exists := roles["explore"]; exists {
		t.Fatalf("empty explore role should be omitted: %#v", roles)
	}

	// Unknown role key must be rejected.
	if _, err := NormalizeSubagents(Document{
		"mode":   "pool",
		"model":  "magpie/deepseek",
		"models": []any{Document{"id": "magpie/deepseek", "description": "default"}},
		"roles":  Document{"reviewer": "magpie/deepseek"},
	}, models); err == nil || err.Error() != "invalid_subagent_roles" {
		t.Fatalf("expected invalid_subagent_roles for unknown key, got %v", err)
	}

	// Role model missing from pool must be rejected.
	if _, err := NormalizeSubagents(Document{
		"mode":   "pool",
		"model":  "magpie/deepseek",
		"models": []any{Document{"id": "magpie/deepseek", "description": "default"}},
		"roles":  Document{"plan": "magpie/glm"},
	}, models); err == nil || err.Error() != "invalid_subagent_roles" {
		t.Fatalf("expected invalid_subagent_roles for model not in pool, got %v", err)
	}
}
