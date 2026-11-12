package kimi

import (
	"errors"
	"testing"
)

func TestDesktopPageAllowsOfficialDisplayParameters(t *testing.T) {
	for _, raw := range []string{
		"app://localhost/kimi-agent.html?theme=dark&locale=zh-CN&platform=darwin",
		"app://localhost/work-settings.html?theme=dark",
	} {
		if _, ok := desktopPath(raw); !ok {
			t.Fatalf("official page refused: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://localhost/kimi-agent.html?theme=dark",
		"app://localhost.evil/kimi-agent.html?theme=dark",
		"app://user@localhost/kimi-agent.html",
		"app://localhost:123/kimi-agent.html",
		"app://localhost/other.html?theme=dark",
		"app://localhost/kimi-agent.html#other",
	} {
		if _, ok := desktopPath(raw); ok {
			t.Fatalf("foreign page accepted: %s", raw)
		}
	}
}

func TestEvaluateUnwrapsCDPRemoteObject(t *testing.T) {
	for _, value := range []any{true, "ready", map[string]any{"ok": true}, nil} {
		r := Result{Result: map[string]any{"result": map[string]any{"type": "object", "value": value}}}
		got, err := r.evaluatedValue()
		if err != nil {
			t.Fatal(err)
		}
		if value == nil && got != nil {
			t.Fatal(got)
		}
		if object, ok := value.(map[string]any); ok {
			if got.(map[string]any)["ok"] != object["ok"] {
				t.Fatal(got)
			}
		} else if got != value {
			t.Fatalf("got %v, want %v", got, value)
		}
	}
	for _, r := range []Result{
		{Result: map[string]any{"result": map[string]any{"type": "object"}, "exceptionDetails": map[string]any{"text": "Uncaught"}}},
		{Result: map[string]any{"result": true}},
	} {
		if _, err := r.evaluatedValue(); !errors.Is(err, ErrRendererUnconfirmed) {
			t.Fatalf("unconfirmed evaluation: %v", err)
		}
	}
}
