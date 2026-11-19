package kimi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// RunContractWorker is the child-process entry point. It reads one Job as JSON
// from stdin and writes one Outcome as JSON to stdout.
//
// The caller (cmd/gateway-daemon) is expected to detect ContractWorkerFlag
// before doing anything else and call this function, then exit with the
// returned code. The parent in RunJob uses os.Executable, so the worker is the
// same binary as the daemon.
func RunContractWorker(stdin io.Reader, stdout io.Writer) int {
	payload, err := io.ReadAll(io.LimitReader(stdin, maxWorkerInput))
	if err != nil {
		writeOutcome(stdout, Outcome{Error: "contract_job_unreadable"})
		return 2
	}
	var job Job
	if json.Unmarshal(payload, &job) != nil {
		writeOutcome(stdout, Outcome{Error: "contract_job_invalid"})
		return 2
	}
	budget := time.Duration(job.BudgetMS) * time.Millisecond
	if budget <= 0 {
		budget = 20 * time.Second
	}
	var outcome Outcome
	switch job.Kind {
	case "contract":
		outcome = runContractPipeline(job, budget)
	case "launch":
		outcome = runLaunchPipeline(job, budget)
	case "self-check":
		outcome = runSelfCheck(job, budget)
	default:
		outcome = Outcome{Error: "contract_kind_unknown"}
	}
	writeOutcome(stdout, outcome)
	return 0
}

func writeOutcome(out io.Writer, outcome Outcome) {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(outcome)
}

// ---------------------------------------------------------------------------
// sandbox

// newSandbox builds the global object the bundle fragments run against.
//
// The original probe supplied a small set of ECMAScript built-ins plus inert
// console shims, and no host API at all. This mirrors that list. `eval` and the
// `Function` constructor are removed after the context exists: the bundle never
// needs them, and removing them means a fragment cannot build new code from
// strings.
func newSandbox(vm *goja.Runtime) error {
	logger := vm.NewObject()
	for _, name := range []string{"log", "debug", "info", "warn", "error", "trace"} {
		if err := logger.Set(name, func(goja.FunctionCall) goja.Value { return goja.Undefined() }); err != nil {
			return err
		}
	}
	for _, name := range []string{"console", "_0xa44319"} {
		if err := vm.Set(name, logger); err != nil {
			return err
		}
	}
	if err := vm.Set("_0x1d175e", "compat-probe"); err != nil {
		return err
	}
	global := vm.GlobalObject()
	for _, name := range []string{"eval", "Function"} {
		if err := global.Delete(name); err != nil {
			return err
		}
	}
	return nil
}

// guard runs one script with a hard budget. goja's only interruption mechanism
// is time-based, which is why the process-level RSS watchdog also exists.
func guard(vm *goja.Runtime, budget time.Duration, run func() error) error {
	timer := time.AfterFunc(budget, func() { vm.Interrupt("contract_budget") })
	defer timer.Stop()
	err := run()
	vm.ClearInterrupt()
	return err
}

// ---------------------------------------------------------------------------
// contract pipeline

type contractReport struct {
	Table     string   `json:"table,omitempty"`
	Decoder   string   `json:"decoder,omitempty"`
	Parser    string   `json:"parser,omitempty"`
	Functions int      `json:"functions,omitempty"`
	Pending   []string `json:"pending,omitempty"`
	Models    []any    `json:"models"`
	Skipped   []any    `json:"skippedModels"`
}

func runContractPipeline(job Job, budget time.Duration) Outcome {
	text := job.Source
	if text == "" {
		return Outcome{Error: "contract_source_empty"}
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	if err := newSandbox(vm); err != nil {
		return Outcome{Error: "contract_sandbox_failed"}
	}

	table, footer, at, ok := tableFunction(text)
	if !ok {
		return Outcome{Error: "table_not_found"}
	}
	decoder, _, ok := decoderFunction(text)
	if !ok {
		return Outcome{Error: "decoder_not_found"}
	}
	tableBody := tableBodyOf(text, table)
	decoderBody := functionBodyOf(text, decoder)
	if tableBody == "" || decoderBody == "" {
		return Outcome{Error: "bootstrap_unreadable"}
	}
	bootstrapStart := strings.LastIndex(text[:at], "(function(")
	if bootstrapStart < 0 {
		return Outcome{Error: "bootstrap_not_found"}
	}
	bootstrap := text[bootstrapStart:at+len(footer)] + ";"

	if err := guard(vm, budget, func() error {
		if _, err := vm.RunString(tableBody + "\n" + decoderBody); err != nil {
			return err
		}
		_, err := vm.RunString(bootstrap)
		return err
	}); err != nil {
		return Outcome{Error: "bootstrap_failed:" + trimError(err)}
	}

	decodeFunc, callable := goja.AssertFunction(vm.Get(decoder))
	if !callable {
		return Outcome{Error: "decoder_not_callable"}
	}
	memo := map[int]string{}
	lookup := func(index int) (string, bool) {
		if value, ok := memo[index]; ok {
			return value, true
		}
		value, err := decodeFunc(goja.Undefined(), vm.ToValue(index))
		if err != nil || goja.IsUndefined(value) || goja.IsNull(value) {
			return "", false
		}
		memo[index] = value.String()
		return memo[index], true
	}
	aliasSet := aliases(text, decoder)
	decode := func(source string) string { return decodeSource(source, aliasSet, lookup) }

	index := indexFunctions(text)
	parserName, routeCatalog := findRouteCatalogParser(text, decode, index)
	if !routeCatalog {
		var found bool
		parserName, _, found = findParser(text, decode, index)
		if !found {
			return Outcome{Error: "parser_not_found"}
		}
	}

	closure := map[string]bool{parserName: true}
	queue := []string{parserName}
	for len(queue) > 0 && len(closure) <= maxClosure*8 {
		name := queue[0]
		queue = queue[1:]
		source, ok := functionAt(index, text, name)
		if !ok {
			continue
		}
		for _, match := range regexpCallNames.FindAllStringSubmatch(decode(source), -1) {
			if _, exists := index[match[1]]; exists && !closure[match[1]] {
				closure[match[1]] = true
				queue = append(queue, match[1])
			}
		}
	}
	sources := make([]string, 0, len(closure))
	for name := range closure {
		if source, ok := functionAt(index, text, name); ok {
			sources = append(sources, decode(source))
		}
	}

	catalogJSON := []byte(job.Catalog)
	if len(catalogJSON) == 0 {
		catalogJSON = []byte("{}")
	}
	if !json.Valid(catalogJSON) {
		return Outcome{Error: "contract_catalog_invalid"}
	}

	pending := map[string]bool{}
	declared := map[string]bool{}
	assembled := strings.Join(sources, "\n")
	declaredAliases := aliasDeclarations(aliasSet)

	var result goja.Value
	for attempt := 0; attempt < maxRetries; attempt++ {
		run := goja.New()
		run.SetMaxCallStackSize(1024)
		if err := newSandbox(run); err != nil {
			return Outcome{Error: "contract_sandbox_failed"}
		}
		if routeCatalog {
			if err := installCatalogURLTypes(run); err != nil {
				return Outcome{Error: "contract_sandbox_failed"}
			}
		}
		failure := ""
		if err := guard(run, budget, func() error {
			if _, err := run.RunString(declaredAliases); err != nil {
				return err
			}
			if _, err := run.RunString(assembled); err != nil {
				return err
			}
			if err := run.Set("__catalog", string(catalogJSON)); err != nil {
				return err
			}
			value, err := run.RunString("JSON.stringify(" + parserName + "(JSON.parse(__catalog)))")
			if err != nil {
				return err
			}
			result = value
			return nil
		}); err != nil {
			failure = err.Error()
			if isBudget(err) {
				return Outcome{Error: "contract_budget_exceeded", TimedOut: true}
			}
		} else {
			report := contractReport{Table: table, Decoder: decoder, Parser: parserName,
				Functions: len(closure), Pending: SortedIDs(keys(pending))}
			if result == nil || !readCatalogReport([]byte(result.String()), routeCatalog, &report) {
				return Outcome{Error: "parser_result_unrecognized"}
			}
			if report.Models == nil {
				report.Models = []any{}
			}
			if report.Skipped == nil {
				report.Skipped = []any{}
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				return Outcome{Error: "parser_result_unrecognized"}
			}
			var asMap map[string]any
			_ = json.Unmarshal(encoded, &asMap)
			return Outcome{Result: asMap}
		}

		missing := missingSymbol(failure)
		if missing == "" || pending[missing] {
			return Outcome{Error: "closure_failed:" + trimText(failure)}
		}
		pending[missing] = true
		groups := collectConstants(text, []string{missing}, decode)
		var fresh [][]declaration
		for _, group := range groups {
			for _, item := range group {
				if !declared[item.name] {
					fresh = append(fresh, group)
					break
				}
			}
		}
		for _, group := range fresh {
			for _, item := range group {
				declared[item.name] = true
			}
		}
		if rendered := renderConstants(fresh); rendered != "" {
			assembled += "\n" + rendered + "\n"
		} else {
			// An unresolvable symbol keeps the failure visible: the probe must
			// never invent behaviour for something it could not read.
			assembled += "\nconst " + missing + ` = (() => { throw Error("compat_unknown_symbol:` + missing + `"); })();`
			declared[missing] = true
		}
	}
	return Outcome{Error: "closure_unstable"}
}

func installCatalogURLTypes(vm *goja.Runtime) error {
	if err := vm.Set("URL", func(call goja.ConstructorCall) *goja.Object {
		value, err := url.Parse(call.Argument(0).String())
		if err != nil || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") {
			panic(vm.NewTypeError("invalid URL"))
		}
		host := value.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		pass, _ := value.User.Password()
		fields := map[string]string{
			"protocol": value.Scheme + ":", "hostname": host, "origin": value.Scheme + "://" + value.Host,
			"pathname": value.EscapedPath(), "username": value.User.Username(), "password": pass,
		}
		for name, field := range fields {
			if err := call.This.Set(name, field); err != nil {
				panic(vm.NewTypeError("invalid URL property"))
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return vm.Set("isIPv4", func(value string) bool {
		ip, err := netip.ParseAddr(value)
		return err == nil && ip.Is4()
	})
}

func readCatalogReport(raw []byte, routeCatalog bool, report *contractReport) bool {
	if !routeCatalog {
		var parsed struct {
			Models  []any `json:"models"`
			Skipped []any `json:"skippedModels"`
		}
		if json.Unmarshal(raw, &parsed) != nil {
			return false
		}
		report.Models, report.Skipped = parsed.Models, parsed.Skipped
		return true
	}
	var parsed struct {
		Routes []struct {
			Protocol string `json:"protocol"`
			Models   []struct {
				ID string `json:"model"`
			} `json:"models"`
		} `json:"routes"`
		Blocked []struct {
			ID string `json:"modelId"`
		} `json:"blocked"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Routes == nil || parsed.Blocked == nil {
		return false
	}
	for _, route := range parsed.Routes {
		if route.Protocol == "" || len(route.Models) == 0 {
			return false
		}
		for _, model := range route.Models {
			report.Models = append(report.Models, map[string]string{"model": model.ID, "family": route.Protocol})
		}
	}
	for _, blocked := range parsed.Blocked {
		if blocked.ID == "" {
			return false
		}
		report.Skipped = append(report.Skipped, blocked.ID)
	}
	return true
}

func aliasDeclarations(set map[string]bool) string {
	var out strings.Builder
	for _, name := range aliasNames(set) {
		out.WriteString("const ")
		out.WriteString(name)
		out.WriteString(" = value => value;\n")
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// launch pipeline

// runLaunchPipeline answers one question: does the packaged build still read
// DAIMON_BUNDLE_DIR? A sentinel value is injected as the environment, and the
// reader must return it with isPackaged false and true.
const launchSentinel = "/synthetic-kimi-runtime-probe"

func runLaunchPipeline(job Job, budget time.Duration) Outcome {
	text := job.Source
	if text == "" {
		return Outcome{Error: "launch_source_empty"}
	}
	functions := launchFunctions(text)
	table, _, _, hasTable := tableFunction(text)
	decoder, _, hasDecoder := decoderFunction(text)
	decode := func(source string) string { return source }
	if hasTable && hasDecoder {
		vm := goja.New()
		vm.SetMaxCallStackSize(1024)
		if err := newSandbox(vm); err != nil {
			return Outcome{Error: "launch_sandbox_failed"}
		}
		tableBody := tableBodyOf(text, table)
		decoderBody := functionBodyOf(text, decoder)
		if tableBody != "" && decoderBody != "" {
			_, at, footer, ok := tablePosition(text)
			if ok {
				bootstrap := text[strings.LastIndex(text[:at], "(function("):at+len(footer)] + ";"
				if err := guard(vm, budget, func() error {
					if _, err := vm.RunString(tableBody + "\n" + decoderBody); err != nil {
						return err
					}
					_, err := vm.RunString(bootstrap)
					return err
				}); err == nil {
					if callable, ok := goja.AssertFunction(vm.Get(decoder)); ok {
						memo := map[int]string{}
						decode = func(source string) string {
							return decodeSource(source, aliases(text, decoder), func(index int) (string, bool) {
								if value, ok := memo[index]; ok {
									return value, true
								}
								value, err := callable(goja.Undefined(), vm.ToValue(index))
								if err != nil || goja.IsUndefined(value) || goja.IsNull(value) {
									return "", false
								}
								memo[index] = value.String()
								return memo[index], true
							})
						}
					}
				}
			}
		}
	}

	decoded := decode(text)
	functions = launchFunctions(decoded)
	readers := launchReaders(decoded, functions)
	if len(readers) != 1 {
		return Outcome{Result: map[string]any{"state": "unverified", "reason": "bundle_override_reader_unconfirmed"}}
	}
	reader := readers[0]
	closure := map[string]bool{reader: true}
	queue := []string{reader}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		source, ok := functions.source(name)
		if !ok {
			continue
		}
		for _, match := range regexpCallNames.FindAllStringSubmatch(source, -1) {
			if functions.has(match[1]) && !closure[match[1]] {
				closure[match[1]] = true
				queue = append(queue, match[1])
			}
		}
		if len(closure) > maxClosure {
			return Outcome{Result: map[string]any{"state": "unverified", "reason": "bundle_override_reader_unconfirmed"}}
		}
	}
	ordered := make([]string, 0, len(closure))
	for name := range closure {
		if source, ok := functions.source(name); ok {
			ordered = append(ordered, source)
		}
	}
	script := strings.Join(ordered, "\n") + "\n" + reader + `("DAIMON_BUNDLE_DIR")`

	run := func(packaged bool) (string, error) {
		vm := goja.New()
		vm.SetMaxCallStackSize(1024)
		if err := newSandbox(vm); err != nil {
			return "", err
		}
		process := vm.NewObject()
		env := vm.NewObject()
		if err := env.Set("DAIMON_BUNDLE_DIR", launchSentinel); err != nil {
			return "", err
		}
		if err := process.Set("env", env); err != nil {
			return "", err
		}
		app := vm.NewObject()
		if err := app.Set("isPackaged", packaged); err != nil {
			return "", err
		}
		if err := vm.Set("process", process); err != nil {
			return "", err
		}
		if err := vm.Set("app", app); err != nil {
			return "", err
		}
		var value goja.Value
		if err := guard(vm, budget, func() error {
			produced, err := vm.RunString(script)
			value = produced
			return err
		}); err != nil {
			return "", err
		}
		if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
			return "", nil
		}
		return value.String(), nil
	}

	development, err := run(false)
	if err != nil || development != launchSentinel {
		return Outcome{Result: map[string]any{"state": "unverified", "reason": "bundle_override_reader_unconfirmed"}}
	}
	production, err := run(true)
	if err != nil || production != launchSentinel {
		return Outcome{Result: map[string]any{"state": "unsupported", "reason": "packaged_bundle_override_disabled"}}
	}
	return Outcome{Result: map[string]any{"state": "supported", "reason": "packaged_bundle_override_accepted"}}
}

// launchFunctions indexes `function NAME(...) {` declarations and finds each
// body's end with a brace walk that understands quotes and comments, because a
// minified bundle has no reliable `\n}` boundary.
func launchFunctions(text string) launchTable {
	table := launchTable{bodies: map[string]string{}}
	for _, match := range regexpLaunchDecl.FindAllStringSubmatchIndex(text, -1) {
		name := text[match[2]:match[3]]
		brace := strings.LastIndex(text[match[0]:match[1]], "{")
		if brace < 0 {
			continue
		}
		end := blockEnd(text, match[0]+brace)
		if end < 0 {
			continue
		}
		table.bodies[name] = text[match[0]:end]
	}
	return table
}

type launchTable struct{ bodies map[string]string }

func (t launchTable) has(name string) bool {
	_, ok := t.bodies[name]
	return ok
}

func (t launchTable) source(name string) (string, bool) {
	source, ok := t.bodies[name]
	return source, ok
}

func blockEnd(text string, start int) int {
	depth := 0
	var quote byte
	var comment int // 0 none, 1 line, 2 block
	for at := start; at < len(text); at++ {
		character := text[at]
		var next byte
		if at+1 < len(text) {
			next = text[at+1]
		}
		switch comment {
		case 1:
			if character == '\n' {
				comment = 0
			}
			continue
		case 2:
			if character == '*' && next == '/' {
				comment = 0
				at++
			}
			continue
		}
		if quote != 0 {
			if character == '\\' {
				at++
			} else if character == quote {
				quote = 0
			}
			continue
		}
		switch {
		case character == '/' && next == '/':
			comment = 1
			at++
		case character == '/' && next == '*':
			comment = 2
			at++
		case character == '"' || character == '\'' || character == '`':
			quote = character
		case character == '{':
			depth++
		case character == '}':
			depth--
			if depth == 0 {
				return at + 1
			}
		}
	}
	return -1
}

// launchReaders finds functions that call NAME("DAIMON_BUNDLE_DIR") where NAME
// is itself a declared function. Exactly one is required: two readers means the
// bundle has two paths and the probe cannot say which one packaging uses.
func launchReaders(text string, functions launchTable) []string {
	var out []string
	seen := map[string]bool{}
	for _, match := range regexpLaunchReader.FindAllStringSubmatch(text, -1) {
		name := match[1]
		if seen[name] || !functions.has(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers

func isBudget(err error) bool {
	if err == nil {
		return false
	}
	if interrupted, ok := err.(*goja.InterruptedError); ok {
		return fmt.Sprint(interrupted.Value()) == "contract_budget"
	}
	return strings.Contains(err.Error(), "contract_budget")
}

func missingSymbol(message string) string {
	match := regexpMissingSymbol.FindStringSubmatch(message)
	if match == nil {
		return ""
	}
	return match[1]
}

func trimError(err error) string {
	if err == nil {
		return ""
	}
	return trimText(err.Error())
}

func trimText(value string) string {
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

// tablePosition is tableFunction with the extra fields the launch pipeline
// needs; it exists so both pipelines agree on which bootstrap they read.
func tablePosition(text string) (name string, at int, footer string, ok bool) {
	name, footer, at, ok = tableFunction(text)
	return
}

// ReadWorkerJob is a test hook: it exposes the job decoder without exporting
// the whole pipeline.
func ReadWorkerJob(stdin io.Reader) (Job, error) {
	payload, err := io.ReadAll(io.LimitReader(stdin, maxWorkerInput))
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(payload, &job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// WorkerMain is the one-call entry for cmd/gateway-daemon:
//
//	if len(os.Args) > 1 && os.Args[1] == kimi.ContractWorkerFlag {
//	    os.Exit(kimi.RunContractWorker(os.Stdin, os.Stdout))
//	}
func WorkerMain() int { return RunContractWorker(os.Stdin, os.Stdout) }

// runInProcess executes a job without a child process. It exists so tests can
// compare the pipeline against the original node:vm implementation directly;
// production callers always use RunJob.
func runInProcess(job Job, config ExecutorConfig) (Outcome, error) {
	budget := config.timeout()
	if budget > 20*time.Second {
		budget = 20 * time.Second
	}
	switch job.Kind {
	case "contract":
		return runContractPipeline(job, budget), nil
	case "launch":
		return runLaunchPipeline(job, budget), nil
	case "self-check":
		return runSelfCheck(job, budget), nil
	}
	return Outcome{Error: "contract_kind_unknown"}, nil
}

// ---------------------------------------------------------------------------
// self-checks

// The self-check names. They are exported so a test can select one without
// being able to supply source text.
const (
	SelfCheckLoop     = "loop"
	SelfCheckAllocate = "allocate"
	SelfCheckSandbox  = "sandbox"
)

// selfCheckScripts are compiled-in programs that exercise the sandbox bounds.
// They are constants rather than caller-supplied text so the worker never gains
// a general evaluation entry point.
var selfCheckScripts = map[string]string{
	SelfCheckLoop:     `while (true) { }`,
	SelfCheckAllocate: `const sink = []; for (;;) { sink.push(new Array(50000).fill('z')); }`,
	SelfCheckSandbox: `JSON.stringify([typeof require, typeof process, typeof Buffer, typeof fetch,
		typeof setTimeout, typeof TextDecoder, typeof eval, typeof Function])`,
}

func runSelfCheck(job Job, budget time.Duration) Outcome {
	script, ok := selfCheckScripts[job.Check]
	if !ok {
		return Outcome{Error: "self_check_unknown"}
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	if err := newSandbox(vm); err != nil {
		return Outcome{Error: "contract_sandbox_failed"}
	}
	var value goja.Value
	if err := guard(vm, budget, func() error {
		produced, err := vm.RunString(script)
		value = produced
		return err
	}); err != nil {
		if isBudget(err) {
			return Outcome{Error: "contract_budget_exceeded", TimedOut: true}
		}
		return Outcome{Error: "self_check_failed:" + trimText(err.Error())}
	}
	observed := ""
	if value != nil && !goja.IsUndefined(value) {
		observed = value.String()
	}
	return Outcome{Result: map[string]any{"observed": observed}}
}
