package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/assets/injection"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

// The translation preview can only be offered from a connection that really
// answers Chat. A connection naming Responses must never be reported as
// Chat-capable, and a connection saved before declarations existed keeps the
// model-route fallback it always had.
func TestTranslationFromConnectionFollowsTheDeclaredProtocol(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{
		"KIMI_WORK_CHATONLY_KEY": "synthetic-chat",
		"KIMI_WORK_RESPONLY_KEY": "synthetic-responses",
		"KIMI_WORK_LEGACY_KEY":   "synthetic-legacy",
	})
	service := &Service{Store: store, Keys: keys, StateDir: dir}
	_, revision, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(revision, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{"id": "chatonly", "name": "Chat only", "baseURL": "https://chat.example/v1", "protocols": []any{"chat"}},
			core.Document{"id": "responly", "name": "Responses only", "baseURL": "https://responses.example/v1", "protocols": []any{"responses"}},
			core.Document{"id": "legacy", "name": "Legacy", "baseURL": "https://legacy.example/v1"},
		}
		d["models"] = []any{
			core.Document{"id": "kimi-chat", "protocol": "chat", "contextWindow": float64(32000), "connectionId": "chatonly"},
			core.Document{"id": "vendor/gpt", "protocol": "responses", "contextWindow": float64(32000), "connectionId": "responly"},
			core.Document{"id": "legacy/model", "protocol": "responses", "contextWindow": float64(32000), "connectionId": "legacy"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	apply := func(modelID string) (core.Document, error) {
		result, err := service.translationAction(context.Background(), "/api/translation/from-connection",
			core.Document{"modelId": modelID})
		if err != nil {
			return nil, err
		}
		return core.Object(result), nil
	}
	result, err := apply("kimi-chat")
	if err != nil {
		t.Fatalf("a Chat connection was refused: %v", err)
	}
	settings := result["settings"].(publicTranslationConfig)
	if result["hasApiKey"] != true || !settings.HasAPIKey || settings.APIKeySource != "connection" || settings.APIKeyRef != "" {
		t.Fatalf("copied settings did not report a redacted connection key: %+v", result)
	}
	source := core.Object(result["source"])
	if core.String(source["chatSupport"]) != "declared" || core.String(source["baseURL"]) != "https://chat.example/v1" {
		t.Fatalf("source = %+v", source)
	}
	if _, err := apply("vendor/gpt"); err == nil || err.Error() != "translation_connection_responses_only" {
		t.Fatalf("a Responses connection claimed Chat support: %v", err)
	}
	if _, err := apply("legacy/model"); err == nil || err.Error() != "translation_connection_chat_undeclared" {
		t.Fatalf("an undeclared connection did not fall back to the model route: %v", err)
	}
}

// translationPage answers the CDP calls a managed Work page would, and records
// every evaluation so a test can read back what the page was told.
type translationPage struct {
	mu          sync.Mutex
	evaluations []string
	write       func(string)
}

func (p *translationPage) send(message map[string]any) {
	encoded, _ := json.Marshal(message)
	p.write(string(encoded))
}

func (p *translationPage) answer(request map[string]any) {
	method, _ := request["method"].(string)
	sessionID, _ := request["sessionId"].(string)
	id, _ := request["id"].(float64)
	params, _ := request["params"].(map[string]any)
	result := map[string]any{}
	switch method {
	case "Target.getTargets":
		result["targetInfos"] = []any{map[string]any{"targetId": "work-1", "type": "page",
			"url": "app://localhost/kimi-agent.html"}}
	case "Target.attachToTarget":
		result["sessionId"] = "session-work-1"
	case "Page.getFrameTree":
		result["frameTree"] = map[string]any{"frame": map[string]any{"id": "main-frame"}}
	case "Runtime.enable":
		p.send(map[string]any{"method": "Runtime.executionContextCreated", "sessionId": sessionID,
			"params": map[string]any{"context": map[string]any{"id": 7,
				"auxData": map[string]any{"isDefault": true, "frameId": "main-frame"}}}})
	case "Page.addScriptToEvaluateOnNewDocument":
		result["identifier"] = "script-1"
	case "Runtime.evaluate":
		expression, _ := params["expression"].(string)
		p.mu.Lock()
		p.evaluations = append(p.evaluations, expression)
		p.mu.Unlock()
		result["result"] = map[string]any{"type": "object", "value": map[string]any{
			"version": 1, "state": "active", "surface": "work", "top": true,
			"structure": "matched", "blocks": 3, "seenBlocks": 3}}
	}
	p.send(map[string]any{"id": id, "result": result, "sessionId": sessionID})
}

func (p *translationPage) snapshotEvaluations() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.evaluations...)
}

// newTranslationPipe wires a host to a fake Work page over the same
// NUL-terminated JSON transport the managed child speaks.
func newTranslationPipe(t *testing.T) (*kimi.Pipe, *translationPage) {
	t.Helper()
	toHostReader, toHostWriter := io.Pipe()
	toPageReader, toPageWriter := io.Pipe()
	page := &translationPage{write: func(frame string) { _, _ = toHostWriter.Write(append([]byte(frame), 0)) }}
	pipe := kimi.NewPipe(toHostReader, toPageWriter)
	go func() {
		reader := bufio.NewReaderSize(toPageReader, 64<<10)
		for {
			frame, err := reader.ReadString(0)
			if strings.HasSuffix(frame, "\x00") {
				var request map[string]any
				if json.Unmarshal([]byte(strings.TrimSuffix(frame, "\x00")), &request) == nil {
					page.answer(request)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pipe.Dispose() })
	return pipe, page
}

func waitForEvaluation(page *translationPage, condition func(string) bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, expression := range page.snapshotEvaluations() {
			if condition(expression) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestTranslationRoundTripFromTheLiveBindingPayload closes the loop the real
// renderer needs: the flat binding payload it sends must reach the configured
// provider and come back as a translation. The provider is a local Chat
// Completions server, so a regression to `translation_input_invalid` fails here
// rather than only on a live page.
func TestTranslationRoundTripFromTheLiveBindingPayload(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for _, message := range body.Messages {
			if message.Role == "user" {
				prompts = append(prompts, message.Content)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"检查这段代码"}}]}`))
	}))
	defer upstream.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := core.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(revision, func(d core.Document) error {
		d["translation"] = core.Document{"enabled": true, "provider": "custom", "targetLanguage": "zh",
			"view": "translated", "customEndpoint": upstream.URL, "customModel": "fixture-model"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, Keys: CacheKeys(testKeys{}), StateDir: stateDir}
	controller, err := kimi.NewController(kimi.ControllerSettings{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	native := &Native{service: service, controller: controller, jobs: make(chan struct{}, 2)}
	controller.SetTranslationHandler(native.translate)

	payload, _, err := injection.TranslationRenderer()
	if err != nil {
		t.Fatal(err)
	}
	pipe, page := newTranslationPipe(t)
	var host *kimi.Host
	reply := func(ctx context.Context, session *kimi.SurfaceSession, generation int, id string, value any) {
		kimi.ServeTranslationReply(ctx, host, native.translate, session, generation, id, value)
	}
	host = kimi.NewHost(pipe, kimi.InjectionOptions{
		Payload:      string(payload),
		Revision:     "rev-1",
		Settings:     map[string]any{"enabled": true, "provider": "custom", "targetLanguage": "zh", "view": "translated"},
		ReplyTimeout: 5 * time.Second,
		OnReply:      reply,
	})
	t.Cleanup(func() { host.DetachAll(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sessions, err := host.Sync(ctx)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("sync: %v", err)
	}
	session := sessions[0]

	// Exactly what translation-renderer.v1.js writes to the binding: one flat
	// object, no `value` wrapper.
	page.send(map[string]any{"method": "Runtime.bindingCalled", "sessionId": session.SessionID,
		"params": map[string]any{"name": kimi.BindingName, "executionContextId": session.ContextID(),
			"payload": `{"type":"translate","id":"kwg-1","generation":` + strconv.Itoa(session.Generation()) +
				`,"text":"Check the code","protectedTexts":[]}`}})

	if !waitForEvaluation(page, func(expression string) bool {
		return strings.Contains(expression, "__kwgTranslationReceive?.(")
	}) {
		t.Fatal("the renderer was never answered")
	}
	answers := []string{}
	for _, expression := range page.snapshotEvaluations() {
		if strings.Contains(expression, "__kwgTranslationReceive?.(") {
			answers = append(answers, expression)
		}
	}
	for _, answer := range answers {
		if strings.Contains(answer, "translation_input_invalid") {
			t.Fatalf("the host rejected the live payload: %s", answer)
		}
	}
	if !strings.Contains(strings.Join(answers, "\n"), "检查这段代码") {
		t.Fatalf("answers = %v", answers)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, prompt := range prompts {
		if strings.Contains(prompt, "Check the code") {
			return
		}
	}
	t.Fatalf("the provider never saw the text from the live payload: %q", prompts)
}
