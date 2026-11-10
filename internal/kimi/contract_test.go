package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureCatalog and fixtureExpected are the golden artifacts produced by the
// original node:vm implementation (see testdata/README.md). They are copied
// from the 0.12.1 test fixtures, not regenerated from this package.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "fixtures", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}

// fixtureCatalog returns the golden api.json body as raw JSON. It is passed to
// the worker as text so member order is preserved exactly.
func fixtureCatalog(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(loadFixture(t, "catalog.json"))
}

// renderedCatalog is the package's own rendering as raw JSON.
func renderedCatalog(t *testing.T) json.RawMessage {
	t.Helper()
	models, err := ValidateModels(SampleModels())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(Catalog(models))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCatalogRendersTheSameShapeAsTheGateway(t *testing.T) {
	got := renderedCatalog(t)
	want := fixtureCatalog(t)
	if string(got) != string(want) {
		t.Fatalf("catalog mismatch\n got=%s\nwant=%s", got, want)
	}
}

func TestValidateModelsRejectsProtocolConflict(t *testing.T) {
	_, err := ValidateModels([]Model{{ID: "kimi-x", Protocol: "responses", ContextWindow: 1024}})
	if err == nil || err.Error() != "protocol_conflict" {
		t.Fatalf("want protocol_conflict, got %v", err)
	}
	if _, err := ValidateModels([]Model{{ID: "gpt-x", Protocol: "responses", ContextWindow: 0}}); err == nil {
		t.Fatal("zero context window must be refused")
	}
	if _, err := ValidateModels([]Model{{ID: "a", Protocol: "responses", ContextWindow: 1},
		{ID: "a", Protocol: "responses", ContextWindow: 1}}); err == nil {
		t.Fatal("duplicate ids must be refused")
	}
}

// TestFixtureSeedsMatchNodeVM is the equivalence gate: twenty synthetic bundles
// with different symbol names must produce exactly what node:vm produced.
func TestFixtureSeedsMatchNodeVM(t *testing.T) {
	var expected map[string]string
	if err := json.Unmarshal(loadFixture(t, "expected.json"), &expected); err != nil {
		t.Fatal(err)
	}
	catalog := fixtureCatalog(t)
	for seed := 0; seed < 20; seed++ {
		source, err := os.ReadFile(filepath.Join("testdata", "fixtures", fmt.Sprintf("bundle-%d.js", seed)))
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		outcome, err := runInProcess(Job{Kind: "contract", Source: string(source), Catalog: catalog}, ExecutorConfig{})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if outcome.Error != "" {
			t.Fatalf("seed %d: %s", seed, outcome.Error)
		}
		got, err := json.Marshal(struct {
			Models  []any `json:"models"`
			Skipped []any `json:"skippedModels"`
		}{Models: anySlice(outcome.Result["models"]), Skipped: anySlice(outcome.Result["skippedModels"])})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !jsonEqual(t, got, []byte(expected[fmt.Sprint(seed)])) {
			t.Fatalf("seed %d mismatch\n got=%s\nwant=%s", seed, got, expected[fmt.Sprint(seed)])
		}
	}
}

// jsonEqual compares two documents semantically. Go renders map keys sorted and
// node preserves insertion order, so byte equality is not the equivalence
// property that matters here.
func jsonEqual(t *testing.T, left, right []byte) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		t.Fatalf("left: %v", err)
	}
	if err := json.Unmarshal(right, &b); err != nil {
		t.Fatalf("right: %v", err)
	}
	canonicalA, _ := json.Marshal(canonical(a))
	canonicalB, _ := json.Marshal(canonical(b))
	return string(canonicalA) == string(canonicalB)
}

func canonical(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = canonical(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = canonical(item)
		}
		return out
	}
	return value
}

func anySlice(value any) []any {
	if rows, ok := value.([]any); ok {
		return rows
	}
	return []any{}
}

// TestParserShapeChangeIsUnverified mirrors the old test that a changed parser
// result shape must never be certified as an empty success.
func TestParserShapeChangeIsUnverified(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "fixtures", "bundle-0.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{
		"return undefined;",
		"return { models: [null], skippedModels };",
		"return { models, skippedModels: [null] };",
		"return { models: [{}], skippedModels };",
	} {
		changed := replaceOnce(string(source), "return { models, skippedModels };", replacement)
		outcome, _ := runInProcess(Job{Kind: "contract", Source: changed, Catalog: fixtureCatalog(t)}, ExecutorConfig{})
		result := verdict(ContractResult{}, outcome.Result, mustModels(t))
		if result.State != StateUnverified || result.Reason != "parser_result_unrecognized" {
			t.Fatalf("%s -> %s/%s", replacement, result.State, result.Reason)
		}
	}
}

func mustModels(t *testing.T) []Model {
	t.Helper()
	models, err := ValidateModels(SampleModels())
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func replaceOnce(text, old, replacement string) string {
	for at := 0; at+len(old) <= len(text); at++ {
		if text[at:at+len(old)] == old {
			return text[:at] + replacement + text[at+len(old):]
		}
	}
	return text
}

// TestBraceInsideLiteralStillLocatesTable covers the real 3.2.22 regression: a
// string table entry containing a raw newline and `}` must not end the slice.
func TestBraceInsideLiteralStillLocatesTable(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "fixtures", "bundle-0.js"))
	if err != nil {
		t.Fatal(err)
	}
	marker := "\n}\nfunction _0xfeed() {\n  return 1;\n}"
	changed := replaceOnce(string(source), "const _0x1002 = ['fixture'];",
		"const _0x1002 = ['fixture', `"+marker+"`];")
	outcome, _ := runInProcess(Job{Kind: "contract", Source: changed, Catalog: fixtureCatalog(t)}, ExecutorConfig{})
	if outcome.Error != "" {
		t.Fatalf("unexpected error: %s", outcome.Error)
	}
	result := verdict(ContractResult{}, outcome.Result, mustModels(t))
	if result.State != StateVerified {
		t.Fatalf("state=%s reason=%s", result.State, result.Reason)
	}
}

func TestLaunchProbeReportsSupportedAndDisabled(t *testing.T) {
	supported := `
function runtimeAllowed(packaged = app.isPackaged) { return true; }
function runtimeEnv(name, env = process.env, packaged = app.isPackaged) {
  return runtimeAllowed(packaged) ? env[name] : undefined;
}
function runtimeReference() { return runtimeEnv('DAIMON_BUNDLE_DIR'); }
`
	outcome, err := runInProcess(Job{Kind: "launch", Source: supported}, ExecutorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Error != "" || outcome.Result["state"] != "supported" {
		t.Fatalf("supported case: %+v", outcome)
	}

	disabled := replaceOnce(supported, "return true;", "return !packaged;")
	outcome, _ = runInProcess(Job{Kind: "launch", Source: disabled}, ExecutorConfig{})
	if outcome.Result["state"] != "unsupported" || outcome.Result["reason"] != "packaged_bundle_override_disabled" {
		t.Fatalf("disabled case: %+v", outcome.Result)
	}

	for _, ambiguous := range []string{
		"function changed() { return 1; }",
		supported + strings.ReplaceAll(supported, "runtime", "another"),
		replaceOnce(supported, "env[name]", "unknownCapability(name)"),
	} {
		outcome, _ = runInProcess(Job{Kind: "launch", Source: ambiguous}, ExecutorConfig{})
		if outcome.Result["state"] != "unverified" {
			t.Fatalf("ambiguous case %q: %+v", ambiguous, outcome.Result)
		}
	}
}

// TestWorkerChildProcessIsBounded proves the production path: the job runs in a
// real child process of the test binary.
func TestWorkerChildProcessIsBounded(t *testing.T) {
	if os.Getenv("KIMI_SKIP_CHILD") == "1" {
		t.Skip("child process test disabled")
	}
	source, err := os.ReadFile(filepath.Join("testdata", "fixtures", "bundle-0.js"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outcome, err := RunJob(ctx, Job{Kind: "contract", Source: string(source), Catalog: fixtureCatalog(t)},
		ExecutorConfig{Executable: workerTestBinary(t)})
	if err != nil {
		t.Fatalf("child run: %v", err)
	}
	if outcome.Error != "" {
		t.Fatalf("child outcome: %s", outcome.Error)
	}
	if rows, _ := outcome.Result["models"].([]any); len(rows) != 5 {
		t.Fatalf("child models=%v", outcome.Result["models"])
	}
}

// workerTestBinary is the test binary itself: TestMain re-enters this package
// as the worker when ContractWorkerFlag is present, so RunJob's os.Executable
// contract is exercised for real.
func workerTestBinary(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}
