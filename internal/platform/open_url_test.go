package platform

import "testing"

func TestValidateExternalHTTPURL(t *testing.T) {
	valid := []string{
		"https://cnb.cool/wssz527/kimiwork-plus/-/releases/tag/v1.1.4",
		"https://github.com/wssz527/kimiwork-plus/releases/tag/v1.1.4",
		"http://127.0.0.1:8080/release",
	}
	for _, raw := range valid {
		got, err := ValidateExternalHTTPURL(raw)
		if err != nil || got != raw {
			t.Fatalf("ValidateExternalHTTPURL(%q) = (%q, %v), want valid", raw, got, err)
		}
	}
	invalid := []string{
		"",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,hi",
		"https://user:pass@example.com/release",
		"https://example.com/a\nb",
		"https://example.com/a b",
		"-a Calculator",
		"/relative/path",
	}
	for _, raw := range invalid {
		if got, err := ValidateExternalHTTPURL(raw); err == nil {
			t.Fatalf("ValidateExternalHTTPURL(%q) unexpectedly accepted %q", raw, got)
		}
	}
}
