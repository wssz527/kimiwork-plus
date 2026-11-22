package platform

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
)

func ValidateExternalHTTPURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 2048 {
		return "", errors.New("invalid_external_url")
	}
	if strings.IndexFunc(trimmed, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return "", errors.New("invalid_external_url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("invalid_external_url")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if (scheme != "https" && scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("invalid_external_url")
	}
	return parsed.String(), nil
}

func OpenExternalURL(raw string) error {
	safeURL, err := ValidateExternalHTTPURL(raw)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", safeURL)
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", safeURL)
	default:
		cmd = exec.Command("xdg-open", safeURL)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
