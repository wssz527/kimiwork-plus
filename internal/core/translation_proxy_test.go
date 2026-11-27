package core

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseWindowsProxyConfig(t *testing.T) {
	const single = "127.0.0.1:7890"
	const proxied = "http://127.0.0.1:7890"
	const schemeMap = "http=127.0.0.1:7890; https=127.0.0.1:7892; socks=127.0.0.1:7891"
	for _, tc := range []struct {
		name, server, override, request, want string
	}{
		{"single", single, "", "https://public.example", proxied},
		{"explicit HTTP", "http://proxy.example:80", "", "https://public.example", "http://proxy.example:80"},
		{"explicit HTTPS", "https://proxy.example:443", "", "https://public.example", "https://proxy.example:443"},
		{"explicit SOCKS5", "socks5://proxy.example:1080", "", "https://public.example", "socks5://proxy.example:1080"},
		{"explicit SOCKS normalized", "socks://proxy.example:1080", "", "https://public.example", "socks5://proxy.example:1080"},
		{"HTTP mapping", schemeMap, "", "http://public.example", proxied},
		{"HTTPS mapping", schemeMap, "", "https://public.example", "http://127.0.0.1:7892"},
		{"case-insensitive scheme mapping", " HTTPS = https://proxy.example:443 ", "", "https://public.example", "https://proxy.example:443"},
		{"HTTP before SOCKS fallback", "http=127.0.0.1:7890;socks=127.0.0.1:7891", "", "https://public.example", proxied},
		{"SOCKS fallback", "socks=127.0.0.1:7891", "", "https://public.example", "socks5://127.0.0.1:7891"},
		{"empty server", "", "", "https://public.example", ""},
		{"no mapping for scheme", "https=127.0.0.1:7892", "", "http://public.example", ""},
		{"localhost bypass", single, "<local>", "https://localhost", ""},
		{"IPv4 loopback bypass", single, "<local>", "https://127.0.0.1", ""},
		{"IPv6 loopback bypass", single, "<LOCAL>", "https://[::1]", ""},
		{"localhost without overrides", single, "", "http://localhost:8080/v1", ""},
		{"IPv4 loopback without overrides", single, "", "http://127.0.0.1:8080/v1", ""},
		{"IPv6 loopback without overrides", single, "", "http://[::1]:8080/v1", ""},
		{"intranet bypass", single, "<local>", "https://office", ""},
		{"qualified host with local bypass", single, "<local>", "https://office.example", proxied},
		{"exact host", single, "skip.example ;  office.example ", "https://OFFICE.Example:8080", ""},
		{"exact port", single, "office.example:8080", "https://office.example:8080", ""},
		{"default port", single, "office.example:443", "https://office.example", ""},
		{"port mismatch", single, "office.example:8080", "https://office.example", proxied},
		{"IPv6 port", single, "[2001:db8::1]:8080", "https://[2001:db8::1]:8080", ""},
		{"suffix wildcard", single, "*.example.com", "https://nested.office.example.com", ""},
		{"suffix wildcard excludes parent", single, "*.example.com", "https://example.com", proxied},
		{"ends in wildcard pattern", single, "*.office", "https://corp.office", ""},
		{"starts with wildcard pattern", single, "office*", "https://office.example", ""},
		{"wildcard with port", single, "*.example.com:8080", "https://office.example.com:8080", ""},
		{"host wildcard excludes port", single, "*443", "https://office.example", proxied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqURL, err := url.Parse(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := parseWindowsProxyConfig(tc.server, tc.override, reqURL)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if proxy != nil {
				got = proxy.String()
			}
			if got != tc.want {
				t.Fatalf("proxy = %q, want %q", got, tc.want)
			}
		})
	}
	if proxy, err := parseWindowsProxyConfig("proxy.example:bad", "", &url.URL{Scheme: "https", Host: "public.example"}); err == nil || proxy != nil {
		t.Fatalf("malformed proxy: %v %v", proxy, err)
	}
}

func TestTranslationProxyPrefersEnvironment(t *testing.T) {
	const subprocess = "KWP_TEST_TRANSLATION_PROXY_ENV"
	if os.Getenv(subprocess) == "1" {
		const want = "http://env-proxy.example:8123"
		req, err := http.NewRequest(http.MethodGet, "https://translation.example", nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := defaultTranslationTransport.Proxy(req)
		if err != nil || proxy == nil || proxy.String() != want {
			t.Fatalf("environment proxy = %v, %v, want %s", proxy, err, want)
		}
		return
	}

	// net/http caches proxy environment on first use in a process.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestTranslationProxyPrefersEnvironment$")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", subprocess:
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, subprocess+"=1", "HTTPS_PROXY=http://env-proxy.example:8123")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated proxy environment: %v\n%s", err, output)
	}
}

func TestTranslationUsesCallerHTTPClient(t *testing.T) {
	redirects, requests := 0, 0
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			redirects++
			return nil
		},
		Transport: translationRoundTrip(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.URL.Path == "/first" {
				return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"/translated"}}, Body: http.NoBody}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"translated":true}`))}, nil
		}),
	}
	result, err := (Translator{Client: client}).request(context.Background(), "https://translation.example/first", "application/json", "", []byte(`{}`), true)
	if err != nil || Object(result)["translated"] != true || redirects != 1 || requests != 2 {
		t.Fatalf("custom client: result=%v err=%v redirects=%d requests=%d", result, err, redirects, requests)
	}
}
