package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wssz527/kimi-work-plus/internal/bridge"
)

type windowDiagnostics struct {
	mu     sync.Mutex
	status string
}

func (d *windowDiagnostics) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.status == "" {
		return "window_created"
	}
	return d.status
}

func (d *windowDiagnostics) Failed() {
	d.mu.Lock()
	d.status = "web_content_process_terminated"
	d.mu.Unlock()
}

func (d *windowDiagnostics) Export(dir string) (string, error) {
	path := filepath.Join(dir, "diagnostics")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	content, err := json.MarshalIndent(map[string]string{
		"buildId": bridge.BuildID,
		"status":  d.Status(),
		"at":      time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path = filepath.Join(path, "window-load-diagnostic.json")
	return path, os.WriteFile(path, append(content, '\n'), 0o600)
}
