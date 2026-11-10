package protocol_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"kimiworkplus.local/migration/internal/protocol"
)

func replyOK(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// forwardedMessages is what the upstream really received for one chat request.
func forwardedMessages(t *testing.T, body string) []map[string]any {
	t.Helper()
	var payload struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode forwarded body %q: %v", body, err)
	}
	return payload.Messages
}

func managedCount(messages []map[string]any) int {
	count := 0
	for _, message := range messages {
		if content, _ := message["content"].(string); strings.Contains(content, protocol.InstructionsBegin) {
			count++
		}
	}
	return count
}

func TestInstructionsProviderRunsOncePerRequest(t *testing.T) {
	fake := newUpstream(t, replyOK)
	var calls int32
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) {
			atomic.AddInt32(&calls, 1)
			return "stay in scope", nil
		}
	})
	for i := 0; i < 3; i++ {
		reply, body := post(t, url, "/chat/completions",
			map[string]any{"model": "kimi-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
		if reply.StatusCode != 200 {
			t.Fatalf("request %d: status %d body %s", i, reply.StatusCode, body)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("provider called %d times, want 3", got)
	}
	requests := fake.captured()
	if len(requests) != 3 {
		t.Fatalf("upstream saw %d requests", len(requests))
	}
	messages := forwardedMessages(t, requests[2].Body)
	if len(messages) == 0 {
		t.Fatal("no messages forwarded")
	}
	if role, _ := messages[0]["role"].(string); role != "system" {
		t.Fatalf("first message role %q", role)
	}
	content, _ := messages[0]["content"].(string)
	want := protocol.InstructionsBegin + "\nstay in scope\n" + protocol.InstructionsEnd
	if content != want {
		t.Fatalf("managed content %q want %q", content, want)
	}
}

// The provider is the hot-reload path: what it answers on the second request is
// what the upstream must see, with no gateway rebuild in between.
func TestInstructionsProviderAnswerIsUsedPerRequest(t *testing.T) {
	fake := newUpstream(t, replyOK)
	var calls int32
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return "first rules", nil
			}
			return "second rules", nil
		}
	})
	payload := map[string]any{"model": "kimi-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for i := 0; i < 2; i++ {
		if reply, body := post(t, url, "/chat/completions", payload, nil); reply.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i, reply.StatusCode, body)
		}
	}
	requests := fake.captured()
	first := forwardedMessages(t, requests[0].Body)
	second := forwardedMessages(t, requests[1].Body)
	if content, _ := first[0]["content"].(string); !strings.Contains(content, "first rules") {
		t.Fatalf("first request content %q", content)
	}
	if content, _ := second[0]["content"].(string); !strings.Contains(content, "second rules") {
		t.Fatalf("second request content %q", content)
	}
}

func TestInstructionsProviderErrorRefusesWithItsCode(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) {
			return "", errors.New("instructions_unavailable")
		}
	})
	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-probe", "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "instructions_unavailable" {
		t.Fatalf("status %d code %s", reply.StatusCode, errorCode(t, body))
	}
	if len(fake.captured()) != 0 {
		t.Fatalf("upstream saw %d requests, want 0", len(fake.captured()))
	}
}

// An empty answer still runs the transformation, because a history written while
// a rules file existed keeps its managed message until something strips it.
func TestInstructionsProviderEmptyStripsManagedMessage(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) { return "", nil }
	})
	managed := protocol.InstructionsBegin + "\nold rules\n" + protocol.InstructionsEnd
	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model": "kimi-probe",
		"messages": []any{
			map[string]any{"role": "system", "content": managed},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	messages := forwardedMessages(t, fake.captured()[0].Body)
	if count := managedCount(messages); count != 0 {
		t.Fatalf("forwarded %d managed messages, want 0", count)
	}
	if len(messages) != 1 {
		t.Fatalf("forwarded %d messages, want 1", len(messages))
	}
}

func TestInstructionsProviderEmptyStripsLegacyTag(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) { return "", nil }
	})
	legacy := "<magpie-kimi-shared-instructions>\nold rules\n</magpie-kimi-shared-instructions>"
	post(t, url, "/chat/completions", map[string]any{
		"model": "kimi-probe",
		"messages": []any{
			map[string]any{"role": "system", "content": legacy},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, nil)
	messages := forwardedMessages(t, fake.captured()[0].Body)
	for _, message := range messages {
		if content, _ := message["content"].(string); strings.Contains(content, "magpie-kimi-shared-instructions") {
			t.Fatalf("legacy managed message survived: %q", content)
		}
	}
}

func TestInstructionsProviderCoversResponsesRoute(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{responsesModel("kimi-resp")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) { return "stay in scope", nil }
	})
	reply, body := post(t, url, "/responses",
		map[string]any{"model": "kimi-resp", "input": "hi"}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(fake.captured()[0].Body), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("forwarded %d input rows, want 2", len(payload.Input))
	}
	parts, _ := payload.Input[0]["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("managed content parts %#v", payload.Input[0]["content"])
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	if !strings.Contains(text, "stay in scope") {
		t.Fatalf("managed text %q", text)
	}
}

// The static field stays for a caller that already read the file, and a provider
// takes precedence when both are set.
func TestStaticInstructionsStillApply(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.Instructions = "static rules"
	})
	post(t, url, "/chat/completions", map[string]any{
		"model": "kimi-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	messages := forwardedMessages(t, fake.captured()[0].Body)
	content, _ := messages[0]["content"].(string)
	if !strings.Contains(content, "static rules") {
		t.Fatalf("managed content %q", content)
	}
}

func TestInstructionsProviderWinsOverStaticText(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.Instructions = "static rules"
		cfg.InstructionsProvider = func() (string, error) { return "live rules", nil }
	})
	post(t, url, "/chat/completions", map[string]any{
		"model": "kimi-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	content, _ := forwardedMessages(t, fake.captured()[0].Body)[0]["content"].(string)
	if !strings.Contains(content, "live rules") || strings.Contains(content, "static rules") {
		t.Fatalf("managed content %q", content)
	}
}

// A provider answer that is only whitespace is empty text, so the transformation
// still runs and the upstream sees no managed message.
func TestInstructionsProviderWhitespaceOnlyIsEmpty(t *testing.T) {
	fake := newUpstream(t, replyOK)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-probe")}, func(cfg *protocol.Config) {
		cfg.InstructionsProvider = func() (string, error) { return "  \n\t ", nil }
	})
	reply, body := post(t, url, "/chat/completions", map[string]any{
		"model": "kimi-probe", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if reply.StatusCode != 200 {
		t.Fatalf("status %d body %s", reply.StatusCode, body)
	}
	if count := managedCount(forwardedMessages(t, fake.captured()[0].Body)); count != 0 {
		t.Fatalf("forwarded %d managed messages, want 0", count)
	}
}
