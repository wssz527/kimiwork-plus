package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTranslationPreservesProtectedText(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var d Document
		_ = json.NewDecoder(r.Body).Decode(&d)
		messages := Rows(d["messages"])
		content := String(Object(messages[1])["content"])
		var values []string
		if json.Unmarshal([]byte(content), &values) == nil {
			for i := range values {
				values[i] = "译:" + values[i]
			}
			b, _ := json.Marshal(values)
			content = string(b)
		} else {
			content = "译:" + content
		}
		_ = json.NewEncoder(w).Encode(Document{"choices": []any{Document{"message": Document{"content": content}}}})
	}))
	defer server.Close()
	c, e := TranslationSettings(Document{"provider": "custom", "customEndpoint": server.URL})
	if e != nil {
		t.Fatal(e)
	}
	source := "Open `src/app.js`, read https://example.test/docs and update config.json before the next step.\n\n```go\nx := 1\n```\nTail"
	out, e := (Translator{}).Translate(context.Background(), source, c, "synthetic", []string{"config.json"})
	if e != nil {
		t.Fatal(e)
	}
	for _, part := range []string{"`src/app.js`", "https://example.test/docs", "config.json", "\n\n```go\nx := 1\n```\n"} {
		if !strings.Contains(out, part) {
			t.Fatalf("lost %q in %s", part, out)
		}
	}
	if calls.Load() == 0 || out == source {
		t.Fatal("provider unused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = (Translator{}).Translate(ctx, source, c, "", nil); e == nil {
		t.Fatal("cancel ignored")
	}
}

type translationRoundTrip func(*http.Request) (*http.Response, error)

func (f translationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTranslationBuiltInProvidersAndBounds(t *testing.T) {
	for _, provider := range []string{"bing", "google"} {
		t.Run(provider, func(t *testing.T) {
			client := &http.Client{Transport: translationRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.Header.Get("Authorization") != "" {
					t.Fatal("invalid provider request")
				}
				var response string
				if provider == "bing" {
					response = `[{"translations":[{"text":"你好"}]}]`
				} else {
					response = `[[["你好","hello"]]]`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			c, _ := TranslationSettings(Document{"provider": provider})
			out, e := (Translator{Client: client}).Translate(context.Background(), "hello", c, "", nil)
			if e != nil || out != "你好" {
				t.Fatal(out, e)
			}
		})
	}
	c, _ := TranslationSettings(nil)
	if _, e := (Translator{}).Translate(context.Background(), strings.Repeat("x", 120001), c, "", nil); e == nil {
		t.Fatal("oversize accepted")
	}
	for _, input := range []Document{{"apiKeyEnv": "PATH"}, {"provider": "custom"}, {"customEndpoint": "http://example.invalid"}, {"provider": "other"}} {
		if _, e := TranslationSettings(input); e == nil {
			t.Fatal(input)
		}
	}
}
