package protocol

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayForwardsKimiHistoryWithoutForeignThinking(t *testing.T) {
	const assistantContent = `"reasoning_content":"kimi-private","reasoning_details":[{"text":"kimi-private"}],"reasoning":"kimi-private", "content":[{"type":"thinking","thinking":"kimi-private"},%s]`
	chat := `{"model":"switch-to","reasoning_effort":"high","messages":[{"role":"user","content":"original request"},{"role":"assistant",` + strings.Replace(assistantContent, "%s", `{"type":"text","text":"visible text"}`, 1) + `,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"n\":1}"}}]},{"role":"tool","tool_call_id":"call_a","content":"tool answer"},{"role":"user","content":"follow up"},{"role":"assistant","reasoning_content":"active thought","reasoning_details":[{"text":"active details"}],"reasoning":"active reasoning","content":[{"type":"thinking","thinking":"kimi-private"}],"tool_calls":[{"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_b","content":"active result"}]}`
	responses := `{"model":"switch-to","reasoning":{"effort":"high"},"input":[{"role":"user","content":"original request"},{"type":"message","role":"assistant",` + strings.Replace(assistantContent, "%s", `{"type":"output_text","text":"visible text"}`, 1) + `},{"type":"thinking","thinking":"kimi-private"},{"summary":[{"type":"summary_text","text":"kimi-private"}],"type":"reasoning"},{"type":"reasoning","id":"","encrypted_content":false,"summary":[{"type":"summary_text","text":"kimi-private"}]},{"type":"reasoning","id":"msg_1_item_1","summary":[{"type":"summary_text","text":"kimi-private"}]},{"type":"reasoning","id":"anthropic_item_0","summary":[{"type":"summary_text","text":"kimi-private"}]},{"type":"reasoning","id":"rs_native","summary":[]},{"type":"reasoning","summary":[],"encrypted_content":"native-encrypted"},{"type":"reasoning","id":"msg_1_item_2","summary":[],"encrypted_content":"native-encrypted-synthetic-id"},{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"n\":1}"},{"type":"function_call_output","call_id":"call_a","output":"tool answer"},{"role":"user","content":"follow up"}]}`
	for _, tc := range []struct {
		name, route, path, upstreamPath, payload string
		anthropic                                bool
	}{
		{"chat", ProtocolChat, "/v1/chat/completions", "/chat/completions", chat, false},
		{"responses", ProtocolResponses, "/v1/responses", "/responses", responses, false},
		{"anthropic from chat", ProtocolChat, "/v1/chat/completions", "/messages", chat, true},
		{"anthropic from responses", ProtocolResponses, "/v1/responses", "/messages", responses, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.upstreamPath {
					t.Errorf("upstream path = %s, want %s", r.URL.Path, tc.upstreamPath)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"ok","content":[{"type":"text","text":"done"}]}`)
			}))
			defer server.Close()
			model := Model{ID: "switch-to", Protocol: tc.route}
			if tc.anthropic {
				wire := ProtocolAnthropic
				model.UpstreamProtocol = &wire
			}
			backend, err := NewBackend([]Endpoint{{BaseURL: server.URL, APIKey: "test"}}, []Model{model})
			if err != nil {
				t.Fatal(err)
			}
			gateway, err := NewGateway(Config{Key: "local", Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.payload))
			r.Header.Set("Authorization", "Bearer local")
			w := httptest.NewRecorder()
			gateway.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var body string
			select {
			case body = <-requests:
			default:
				t.Fatal("no upstream request")
			}
			if !json.Valid([]byte(body)) || strings.Contains(body, "kimi-private") {
				t.Fatalf("unexpected forwarded history: %s", body)
			}
			for _, value := range []string{"original request", "visible text", "follow up", "call_a", "lookup", "tool answer", "high"} {
				if !strings.Contains(body, value) {
					t.Errorf("missing %q in %s", value, body)
				}
			}
			if tc.anthropic {
				for _, typ := range []string{`"type":"tool_use"`, `"type":"tool_result"`} {
					if !strings.Contains(body, typ) {
						t.Errorf("missing %s in %s", typ, body)
					}
				}
			} else if tc.route == ProtocolResponses {
				for _, item := range []string{
					`{"type":"reasoning","id":"rs_native","summary":[]}`,
					`{"type":"reasoning","summary":[],"encrypted_content":"native-encrypted"}`,
					`{"type":"reasoning","id":"msg_1_item_2","summary":[],"encrypted_content":"native-encrypted-synthetic-id"}`,
				} {
					if !strings.Contains(body, item) {
						t.Errorf("native Responses item %s was changed: %s", item, body)
					}
				}
				for _, forbidden := range []string{`"msg_1_item_1"`, `"anthropic_item_0"`} {
					if strings.Contains(body, forbidden) {
						t.Errorf("synthetic reasoning item %s leaked: %s", forbidden, body)
					}
				}
			} else {
				for _, value := range []string{`"reasoning_content":"active thought"`, `"reasoning_details":[{"text":"active details"}]`, `"reasoning":"active reasoning"`, `"tool_call_id":"call_b"`, `"content":"active result"`} {
					if !strings.Contains(body, value) {
						t.Errorf("current Chat tool loop lost %s: %s", value, body)
					}
				}
			}
		})
	}
}

func TestCleanOpenAIHistoryKeepsToolOnlyTurn(t *testing.T) {
	payload := `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"remove"}],"tool_calls":[{"id":"a"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"remove"}],"tool_calls": [ ]}]}`
	want := `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a"}]}]}`
	if got := string(cleanOpenAIHistory([]byte(payload), ProtocolChat)); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
