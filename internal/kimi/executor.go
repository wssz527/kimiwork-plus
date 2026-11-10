package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The contract probe runs arbitrary-looking JavaScript taken out of the
// installed Kimi bundle. That code is not ours, and goja offers no memory cap:
// a 200 ms busy loop can allocate hundreds of megabytes before a time budget
// interrupts it. So every probe runs in a separate process of the same binary
// (`--contract-worker`), which is killed on timeout or when its resident set
// passes the budget. Nothing in this file hands the script a file, network or
// process API; the worker only receives JSON in and returns JSON out.

const (
	// ContractWorkerFlag is the argument the parent passes to re-enter this
	// package as a child process. The parent uses os.Executable() so the
	// gateway binary and the worker are the same file.
	ContractWorkerFlag = "--contract-worker"

	defaultProbeTimeout = 30 * time.Second
	defaultProbeRSS     = 512 << 20
	maxWorkerOutput     = 8 << 20
	// maxWorkerInput bounds the job the parent sends. A real 3.2.x bundle is
	// about 10 MB, so the cap is generous but still finite.
	maxWorkerInput = 64 << 20
)

// ExecutorConfig bounds one probe. Zero values take the defaults.
type ExecutorConfig struct {
	// Timeout is the wall-clock budget for the whole child process.
	Timeout time.Duration
	// MaxRSSBytes kills the child when its resident set exceeds this.
	MaxRSSBytes int64
	// Executable overrides os.Executable, for tests and for hosts that ship a
	// dedicated worker binary.
	Executable string
}

func (c ExecutorConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultProbeTimeout
}

func (c ExecutorConfig) maxRSS() int64 {
	if c.MaxRSSBytes > 0 {
		return c.MaxRSSBytes
	}
	return defaultProbeRSS
}

// Job is one unit of work for the worker.
type Job struct {
	// Kind selects the pipeline: "contract" (catalog parser) or "launch"
	// (bundle-override reader).
	Kind string `json:"kind"`
	// Source is the bundle text. It never leaves this machine.
	Source string `json:"source"`
	// Catalog is the api.json body the parser receives, already rendered as
	// JSON text. It is carried as raw bytes so member order survives the trip:
	// the parser builds its list with Object.values, so order is behaviour.
	Catalog json.RawMessage `json:"catalog,omitempty"`
	// BudgetMS is the per-script budget inside the worker. The parent still
	// enforces the process timeout; this only makes a slow script fail earlier.
	BudgetMS int `json:"budgetMs,omitempty"`
	// Check selects a compiled-in self-check for Kind "self-check". The script
	// text is fixed in this package: the caller chooses a name, never source.
	Check string `json:"check,omitempty"`
}

// Outcome is the worker's answer. An error string means the pipeline failed;
// the caller maps that to `unverified` and never to success.
type Outcome struct {
	Result   map[string]any `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
	TimedOut bool           `json:"timedOut,omitempty"`
}

// ErrProbeTimeout reports that the child exceeded its budget.
var ErrProbeTimeout = errors.New("contract_probe_timeout")

// ErrProbeResource reports that the child was killed for exceeding MaxRSSBytes.
var ErrProbeResource = errors.New("contract_probe_resource_exceeded")

// RunJob executes one job in a bounded child process.
func RunJob(ctx context.Context, job Job, config ExecutorConfig) (Outcome, error) {
	executable := config.Executable
	if executable == "" {
		resolved, err := os.Executable()
		if err != nil {
			return Outcome{}, fmt.Errorf("contract_executable_unavailable: %w", err)
		}
		executable = resolved
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return Outcome{}, fmt.Errorf("contract_job_invalid: %w", err)
	}

	childCtx, cancel := context.WithTimeout(ctx, config.timeout())
	defer cancel()

	command := exec.CommandContext(childCtx, executable, ContractWorkerFlag)
	command.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Env = workerEnvironment()
	configureChild(command)

	if err := command.Start(); err != nil {
		return Outcome{}, fmt.Errorf("contract_worker_start_failed: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()

	stopWatch := watchResidentSet(command.Process.Pid, config.maxRSS(), cancel)
	waitErr := <-done
	killedForMemory := stopWatch()

	switch {
	case killedForMemory:
		return Outcome{}, ErrProbeResource
	case childCtx.Err() == context.DeadlineExceeded:
		return Outcome{}, ErrProbeTimeout
	case ctx.Err() != nil:
		return Outcome{}, ctx.Err()
	case waitErr != nil:
		// A worker that fails to start, crashes or exits non-zero still gets a
		// chance to have written a structured error before it died.
		if outcome, ok := decodeOutcome(stdout.Bytes()); ok {
			return outcome, nil
		}
		return Outcome{}, fmt.Errorf("contract_worker_failed: %w%s", waitErr, tail(stderr.String()))
	}
	outcome, ok := decodeOutcome(stdout.Bytes())
	if !ok {
		return Outcome{}, errors.New("contract_worker_output_invalid")
	}
	return outcome, nil
}

// workerEnvironment strips inherited state the script must never see. The
// worker is not a general-purpose Node: it needs no PATH, no HOME and no keys.
func workerEnvironment() []string {
	keep := []string{"TMPDIR", "TEMP", "TMP", "SystemRoot"}
	var out []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		for _, allowed := range keep {
			if strings.EqualFold(name, allowed) {
				out = append(out, entry)
				break
			}
		}
	}
	return out
}

func decodeOutcome(data []byte) (Outcome, bool) {
	if len(data) == 0 || len(data) > maxWorkerOutput {
		return Outcome{}, false
	}
	var outcome Outcome
	if json.Unmarshal(data, &outcome) != nil {
		return Outcome{}, false
	}
	return outcome, true
}

func tail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 400 {
		return ": " + value[len(value)-400:]
	}
	if value == "" {
		return ""
	}
	return ": " + value
}

// watchResidentSet polls the child's resident set and cancels the context when
// it passes the budget. It returns a function that stops the watcher and
// reports whether the kill was for memory rather than time.
func watchResidentSet(pid int, limit int64, cancel context.CancelFunc) func() bool {
	if limit <= 0 || pid <= 0 {
		return func() bool { return false }
	}
	var once sync.Once
	killed := false
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				rss, ok := residentBytes(pid)
				if ok && rss > limit {
					once.Do(func() { killed = true })
					cancel()
					return
				}
			}
		}
	}()
	return func() bool {
		close(stop)
		<-finished
		return killed
	}
}

// residentBytes reads the resident set size. An unreadable value is not a
// reason to kill anything: it only means this sample cannot prove a limit.
func residentBytes(pid int) (int64, bool) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, false
		}
		kilobytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0, false
		}
		return kilobytes * 1024, true
	case "linux":
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "statm"))
		if err != nil {
			return 0, false
		}
		fields := strings.Fields(string(data))
		if len(fields) < 2 {
			return 0, false
		}
		pages, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return pages * int64(os.Getpagesize()), true
	}
	return residentBytesPlatform(pid)
}
