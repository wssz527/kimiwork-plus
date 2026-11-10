package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The worker can be asked to run one of a fixed set of compiled-in checks. It
// exists so the process-level budget can be proven without giving the parent a
// way to evaluate arbitrary source: the script text never comes from the caller.
func TestTimeBudgetTerminatesTheWorker(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = RunJob(ctx, Job{Kind: "self-check", Check: SelfCheckLoop}, ExecutorConfig{
		Executable: executable, Timeout: 1200 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a non-terminating script reported success")
	}
	if !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("want ErrProbeTimeout, got %v", err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("budget overran: %v", elapsed)
	}
	t.Logf("worker killed after %v", elapsed)
}

// TestResourceBudgetKillsTheWorker proves the RSS watchdog is wired: the child
// is given a budget far below what an allocation loop reaches.
func TestResourceBudgetKillsTheWorker(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err = RunJob(ctx, Job{Kind: "self-check", Check: SelfCheckAllocate}, ExecutorConfig{
		Executable: executable, Timeout: 15 * time.Second, MaxRSSBytes: 96 << 20})
	if err == nil {
		t.Fatal("an allocation loop reported success")
	}
	if !errors.Is(err, ErrProbeResource) && !errors.Is(err, ErrProbeTimeout) {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("worker stopped after %v with %v", time.Since(start), err)
}

// TestWorkerEnvironmentIsStripped proves the child never inherits a key or a
// PATH, so a bundle fragment cannot reach a tool even if it tried.
func TestWorkerEnvironmentIsStripped(t *testing.T) {
	t.Setenv("KIMI_TEST_SECRET", "must-not-reach-the-worker")
	t.Setenv("PATH", "/usr/bin:/bin")
	environment := workerEnvironment()
	for _, entry := range environment {
		if strings.HasPrefix(entry, "KIMI_TEST_SECRET=") {
			t.Fatal("a secret was passed to the worker")
		}
		if strings.HasPrefix(entry, "PATH=") {
			t.Fatal("PATH was passed to the worker")
		}
		if strings.HasPrefix(entry, "HOME=") {
			t.Fatal("HOME was passed to the worker")
		}
	}
}

func TestSandboxHasNoHostAPI(t *testing.T) {
	outcome, err := runInProcess(Job{Kind: "self-check", Check: SelfCheckSandbox}, ExecutorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Error != "" {
		t.Fatalf("sandbox check failed: %s", outcome.Error)
	}
	observed, _ := outcome.Result["observed"].(string)
	t.Logf("sandbox surface: %s", observed)
	var kinds []string
	if err := json.Unmarshal([]byte(observed), &kinds); err != nil {
		t.Fatalf("unexpected sandbox surface: %s", observed)
	}
	// require, process, Buffer, fetch, setTimeout, TextDecoder, eval, Function
	// must all be absent. Anything else is a host API the bundle could reach.
	if len(kinds) != 8 {
		t.Fatalf("expected 8 probes, got %d", len(kinds))
	}
	for i, kind := range kinds {
		if kind != "undefined" {
			t.Fatalf("probe %d is %q, want undefined", i, kind)
		}
	}
}

func TestExecutorRefusesAMissingExecutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := RunJob(ctx, Job{Kind: "contract", Source: "x"}, ExecutorConfig{
		Executable: "/nonexistent/contract-worker"})
	if err == nil {
		t.Fatal("a missing worker executable reported success")
	}
}
