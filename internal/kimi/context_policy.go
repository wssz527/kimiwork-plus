package kimi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The context policy edits the kernel's own `[loop_control]` table:
//
//	compaction_trigger_ratio   the fraction of the window that triggers
//	                           compaction; the schema takes 0.50 … 0.99
//	reserved_context_size      the tokens held back from the window, >= 0
//
// Both keys belong to the kernel's `LoopControlSchema`, registered for the
// `loopControl` config section, which is `[loop_control]` in `config.toml`. A
// table under any other name is not read by the kernel at all.
//
// Two layouts are supported and preserved, because the installed kernels differ:
// a marker-delimited block (the same shape the subagent writer uses) and a plain
// table whose values are tracked by an ownership receipt. The block is preferred;
// the table layout is what an earlier build wrote.
const (
	ContextRatioMin = 0.5
	ContextRatioMax = 0.99

	contextModeInherit = "inherit"
	contextModeCustom  = "custom"

	// contextOriginalAbsent marks a key the file did not have before this module
	// wrote it, so a release deletes the line instead of restoring it. It cannot
	// collide with a real line: no TOML line starts with a NUL byte.
	contextOriginalAbsent = "\x00absent"
)

var (
	contextFieldNames = []string{"triggerRatio", "reservedTokens"}
	contextKeyOf      = map[string]string{"triggerRatio": "compaction_trigger_ratio",
		"reservedTokens": "reserved_context_size"}
	contextTable = "loop_control"
)

// ContextPolicy is the requested configuration. `TriggerRatio` is always the
// decimal the kernel's schema takes; percent is a presentation concern.
type ContextPolicy struct {
	Mode           string   `json:"mode"`
	TriggerRatio   *float64 `json:"triggerRatio,omitempty"`
	ReservedTokens *int64   `json:"reservedTokens,omitempty"`
}

// ContextStatus is the result of an update or read.
type ContextStatus struct {
	State    string         `json:"state"`
	Mode     string         `json:"mode,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Policy   *ContextPolicy `json:"policy,omitempty"`
	Layout   string         `json:"layout,omitempty"`
	Observed map[string]any `json:"observed,omitempty"`
}

// ContextPolicyConfig edits one explicit Kimi home.
type ContextPolicyConfig struct {
	stateDir string
	paths    configPaths
	receipt  string
	ownerID  string
}

// NewContextPolicyConfig prepares a writer for an explicit Kimi home.
func NewContextPolicyConfig(stateDir, kimiHome string) (*ContextPolicyConfig, error) {
	if stateDir == "" || !isAbsPath(stateDir) {
		return nil, errors.New("state_dir_required")
	}
	paths, err := newConfigPaths(kimiHome)
	if err != nil {
		return nil, err
	}
	owner, err := randomUUID()
	if err != nil {
		return nil, err
	}
	return &ContextPolicyConfig{stateDir: stateDir, paths: paths,
		receipt: filepath.Join(stateDir, contextReceipt), ownerID: owner}, nil
}

// HomeDir is the Kimi home this writer targets.
func (c *ContextPolicyConfig) HomeDir() string { return c.paths.homeDir }

// validRatio accepts the kernel's decimal range only.
func validRatio(value float64) bool {
	return value >= ContextRatioMin && value <= ContextRatioMax
}

// validReserved accepts zero. The released module's own rule is
// `Number.isSafeInteger(value) && value >= 0`, and the panel lets the user type
// 0, so refusing it here would make a value the UI accepts unsaveable.
func validReserved(value int64) bool { return value >= 0 && value <= 1<<30 }

// normalizeContextPolicy refuses a shape the kernel cannot take.
func normalizeContextPolicy(policy ContextPolicy) (*ContextPolicy, bool) {
	if policy.Mode == contextModeInherit {
		if policy.TriggerRatio != nil || policy.ReservedTokens != nil {
			return nil, false
		}
		return &ContextPolicy{Mode: contextModeInherit}, true
	}
	if policy.Mode != contextModeCustom {
		return nil, false
	}
	out := &ContextPolicy{Mode: contextModeCustom}
	chosen := 0
	if policy.TriggerRatio != nil {
		if !validRatio(*policy.TriggerRatio) {
			return nil, false
		}
		value := *policy.TriggerRatio
		out.TriggerRatio = &value
		chosen++
	}
	if policy.ReservedTokens != nil {
		if !validReserved(*policy.ReservedTokens) {
			return nil, false
		}
		value := *policy.ReservedTokens
		out.ReservedTokens = &value
		chosen++
	}
	if chosen == 0 {
		return nil, false
	}
	return out, true
}

// renderContextRatio renders the decimal the schema takes. A ratio is written
// with two decimals so 0.5 becomes 0.50, which is the form the kernel expects.
func renderContextRatio(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// ---------------------------------------------------------------------------
// receipt

type contextReceiptFile struct {
	SchemaVersion    int               `json:"schemaVersion"`
	OwnerID          string            `json:"ownerID"`
	KimiDir          string            `json:"kimiDir"`
	ConfigPath       string            `json:"configPath"`
	Layout           string            `json:"layout"`
	Block            *contextBlockRef  `json:"block,omitempty"`
	Values           map[string]string `json:"values,omitempty"`
	Written          map[string]string `json:"written,omitempty"`
	Original         map[string]string `json:"original,omitempty"`
	HeaderEOLPatched bool              `json:"headerEolPatched,omitempty"`
}

type contextBlockRef struct {
	OwnerID  string `json:"ownerID"`
	BodyHash string `json:"bodyHash"`
}

type contextReceiptState struct {
	kind  string
	raw   []byte
	value *contextReceiptFile
}

func (c *ContextPolicyConfig) readReceipt() contextReceiptState {
	info, err := lstatPath(c.receipt)
	if err != nil {
		return contextReceiptState{kind: "absent"}
	}
	if info.isSymlink || !info.isFile || info.size > 1<<20 {
		return contextReceiptState{kind: "unsafe"}
	}
	raw, err := os.ReadFile(c.receipt)
	if err != nil {
		return contextReceiptState{kind: "unsafe"}
	}
	var value contextReceiptFile
	if json.Unmarshal(raw, &value) != nil || value.SchemaVersion != 1 ||
		!uuidPattern.MatchString(value.OwnerID) || value.KimiDir == "" || value.ConfigPath == "" {
		return contextReceiptState{kind: "corrupt"}
	}
	return contextReceiptState{kind: "valid", raw: raw, value: &value}
}

func (c *ContextPolicyConfig) writeReceipt(value *contextReceiptFile) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "receipt_unavailable"
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return "receipt_unavailable"
	}
	if info, err := lstatPath(c.stateDir); err != nil || info.isSymlink || !info.isDir {
		return "receipt_unavailable"
	}
	if err := writePrivateFile(c.receipt, append(encoded, '\n'), 0o600); err != nil {
		return "receipt_unavailable"
	}
	return ""
}

func (c *ContextPolicyConfig) removeReceipt() { removeFile(c.receipt) }

// ---------------------------------------------------------------------------
// scan

type contextEntry struct {
	line  int
	value string
}

type contextScan struct {
	raw      string
	lines    []tomlLine
	block    *subagentBlock
	entries  map[string]contextEntry
	headerAt int
	issue    string
}

// scanContext reads the compaction keys and the managed block, if any.
func scanContext(raw string) contextScan {
	lines := splitTOML(raw)
	scan := contextScan{raw: raw, lines: lines, entries: map[string]contextEntry{}, headerAt: -1}
	var begins, ends []int
	for i, line := range lines {
		switch strings.TrimSpace(line.text) {
		case contextBlockBegin:
			begins = append(begins, i)
		case contextBlockEnd:
			ends = append(ends, i)
		}
	}
	if len(begins) > 1 || len(ends) > 1 {
		scan.issue = "duplicate_block"
		return scan
	}
	if len(begins) != len(ends) || (len(begins) == 1 && ends[0] < begins[0]) {
		scan.issue = "unterminated_block"
		return scan
	}
	var start, stop = -1, -1
	if len(begins) == 1 {
		start, stop = begins[0], ends[0]
	}
	for i, line := range lines {
		if start >= 0 && i >= start && i <= stop {
			continue
		}
		if strings.Contains(line.text, "KIMI WORK GATEWAY CONTEXT POLICY") {
			scan.issue = "suspicious_marker"
			return scan
		}
	}
	if start >= 0 {
		if start+1 >= len(lines) {
			scan.issue = "block_modified"
			return scan
		}
		owner := subagentOwnerLine.FindStringSubmatch(lines[start+1].text)
		if owner == nil {
			scan.issue = "block_modified"
			return scan
		}
		body := make([]string, 0, stop-start)
		for _, line := range lines[start+2 : stop] {
			body = append(body, line.text)
		}
		if tomlDigest(body) != owner[2] {
			scan.issue = "block_modified"
			return scan
		}
		scan.block = &subagentBlock{begin: start, end: stop, ownerID: owner[1], digest: owner[2],
			addedEOL: owner[3] == "1"}
	}
	table := ""
	seenHeader := 0
	for i, line := range lines {
		if scan.block != nil && i >= scan.block.begin && i <= scan.block.end {
			continue
		}
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		code, issue := tomlCode(line.text)
		if issue != "" {
			scan.issue = issue
			return scan
		}
		lead := strings.IndexFunc(line.text, func(r rune) bool { return r != ' ' && r != '\t' })
		if lead < 0 {
			continue
		}
		if line.text[lead] == '[' {
			// An array of tables or a sub-table of the same name defines the
			// table in a shape whose merge rules differ. Appending a plain
			// `[loop_control]` beside either produces a file the kernel cannot
			// parse, so the layout is refused rather than guessed.
			array := strings.HasPrefix(line.text[lead:], "[[")
			close := strings.LastIndexByte(line.text, ']')
			if close < 0 {
				table = ""
				continue
			}
			inner := lead + 1
			if array {
				inner++
				close--
			}
			segments := tomlKeySegments(line.text, inner, close)
			if len(segments) > 0 && segments[0] == contextTable {
				if array || len(segments) > 1 {
					scan.issue = "unsupported_layout"
					return scan
				}
				seenHeader++
				if seenHeader > 1 {
					scan.issue = "duplicate_table"
					return scan
				}
				table = contextTable
				if scan.headerAt < 0 {
					scan.headerAt = i
				}
				continue
			}
			table = ""
			continue
		}
		eq := strings.IndexByte(code, '=')
		if eq < 0 {
			continue
		}
		key := tomlKeySegments(line.text, lead, eq)
		if table == "" && len(key) > 0 && key[0] == contextTable {
			// A root `loop_control.x = …` or `loop_control = { … }` defines the
			// same table outside a header, so it is not ours to extend.
			scan.issue = "unsupported_layout"
			return scan
		}
		if table != contextTable || len(key) != 1 {
			continue
		}
		rawValue := strings.TrimSpace(line.text[eq+1:])
		for field, tomlKey := range contextKeyOf {
			if key[0] != tomlKey {
				continue
			}
			if field == "triggerRatio" {
				if _, err := strconv.ParseFloat(rawValue, 64); err != nil {
					scan.issue = "unreadable_value"
					return scan
				}
			} else if _, err := strconv.ParseInt(rawValue, 10, 64); err != nil {
				scan.issue = "unreadable_value"
				return scan
			}
			if _, duplicate := scan.entries[field]; duplicate {
				scan.issue = "duplicate_key"
				return scan
			}
			scan.entries[field] = contextEntry{line: i, value: rawValue}
		}
	}
	return scan
}

// savedPolicy reads the policy the receipt records.
func (c *ContextPolicyConfig) savedPolicy(receipt contextReceiptState) *ContextPolicy {
	if receipt.kind != "valid" {
		return nil
	}
	policy := &ContextPolicy{Mode: contextModeCustom}
	chosen := 0
	if raw, ok := receipt.value.Values["triggerRatio"]; ok {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && validRatio(value) {
			policy.TriggerRatio = &value
			chosen++
		}
	}
	if raw, ok := receipt.value.Values["reservedTokens"]; ok {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && validReserved(value) {
			policy.ReservedTokens = &value
			chosen++
		}
	}
	if chosen == 0 {
		return nil
	}
	return policy
}

// State reports what is on disk without writing.
func (c *ContextPolicyConfig) State() ContextStatus {
	snapshot, issue := c.snapshot()
	receipt := c.readReceipt()
	if issue != "" {
		if issue == "config_missing" {
			return ContextStatus{State: NativeStateWaiting, Reason: issue}
		}
		return ContextStatus{State: NativeStateConflict, Reason: issue}
	}
	if receipt.kind == "corrupt" || receipt.kind == "unsafe" {
		return ContextStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
	}
	observed := map[string]any{}
	for field, entry := range snapshot.entries {
		observed[field] = entry.value
	}
	if snapshot.block != nil {
		policy := c.savedPolicy(receipt)
		if policy == nil {
			return ContextStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
		}
		return ContextStatus{State: NativeStateFileApplied, Mode: contextModeCustom, Policy: policy,
			Layout: "block", Observed: observed}
	}
	if policy := c.savedPolicy(receipt); policy != nil {
		return ContextStatus{State: NativeStateFileApplied, Mode: contextModeCustom, Policy: policy,
			Layout: "table", Observed: observed}
	}
	if len(snapshot.entries) > 0 {
		// A table we did not write: report it, never adopt it.
		return ContextStatus{State: NativeStateConflict, Reason: "foreign_value", Observed: observed}
	}
	return ContextStatus{State: NativeStateInherit, Mode: contextModeInherit}
}

// Update applies a policy.
func (c *ContextPolicyConfig) Update(policy ContextPolicy) ContextStatus {
	normalized, ok := normalizeContextPolicy(policy)
	if !ok {
		return ContextStatus{State: NativeStateConflict, Reason: "invalid_policy"}
	}
	snapshot, issue := c.snapshot()
	if issue != "" {
		if issue == "config_missing" && normalized.Mode == contextModeInherit {
			c.releaseOwnedReceipt()
			return ContextStatus{State: NativeStateInherit, Mode: contextModeInherit}
		}
		if issue == "config_missing" {
			return ContextStatus{State: NativeStateWaiting, Reason: issue}
		}
		return ContextStatus{State: NativeStateConflict, Reason: issue}
	}
	if normalized.Mode == contextModeInherit {
		return c.release(snapshot)
	}
	// A value we did not write is never overwritten.
	receipt := c.readReceipt()
	if receipt.kind == "corrupt" || receipt.kind == "unsafe" {
		return ContextStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
	}
	for field := range snapshot.entries {
		if !c.ownsValue(receipt, snapshot, field) {
			return ContextStatus{State: NativeStateConflict, Reason: "foreign_value"}
		}
	}
	if snapshot.block == nil {
		return c.applyTable(snapshot, normalized, receipt)
	}
	return c.applyBlock(snapshot, normalized, receipt)
}

// ownsValue reports whether the receipt can attribute this table entry.
//
// The test is the value, not the owner id. A restart creates a new owner, and the
// released module deliberately compares what it wrote rather than who wrote it:
// requiring the same owner would turn this module's own configuration into a
// foreign value after every restart, so a user could never change or clear a
// setting they had saved in an earlier session.
func (c *ContextPolicyConfig) ownsValue(receipt contextReceiptState, snapshot contextScan, field string) bool {
	if receipt.kind != "valid" {
		return false
	}
	entry := snapshot.entries[field]
	written := receipt.value.Written[field]
	return written == entry.value || receipt.value.Values[field] == entry.value
}

// applyBlock rewrites the managed block, which is the preferred layout.
func (c *ContextPolicyConfig) applyBlock(snapshot contextScan, policy *ContextPolicy, receipt contextReceiptState) ContextStatus {
	// A block whose recorded hash matches what is on disk is this module's own,
	// whoever wrote it: a restart creates a new owner but not a new policy.
	if receipt.kind == "valid" && receipt.value.Block != nil &&
		receipt.value.Block.BodyHash != snapshot.block.digest {
		return ContextStatus{State: NativeStateConflict, Reason: "owner_conflict"}
	}
	body := renderContextBlock(policy)
	rendered, _ := withManagedBlock(contextAsSubagentScan(snapshot), body, contextMarkers, c.ownerID, tomlEOL(snapshot.raw))
	next := &contextReceiptFile{SchemaVersion: 1, OwnerID: c.ownerID, KimiDir: c.paths.homeDir,
		ConfigPath: c.paths.configPath, Layout: "block",
		Block:  &contextBlockRef{OwnerID: c.ownerID, BodyHash: tomlDigest(body)},
		Values: map[string]string{}}
	if policy.TriggerRatio != nil {
		next.Values["triggerRatio"] = renderContextRatio(*policy.TriggerRatio)
	}
	if policy.ReservedTokens != nil {
		next.Values["reservedTokens"] = strconv.FormatInt(*policy.ReservedTokens, 10)
	}
	previous := c.readReceipt()
	if failure := c.writeReceipt(next); failure != "" {
		return ContextStatus{State: NativeStateConflict, Reason: failure}
	}
	if rendered != snapshot.raw {
		if failure := c.commit(snapshot, rendered); failure != "" {
			if previous.kind == "valid" {
				_ = c.writeReceipt(previous.value)
			} else {
				c.removeReceipt()
			}
			return ContextStatus{State: NativeStateConflict, Reason: failure}
		}
	}
	return ContextStatus{State: NativeStateFileApplied, Mode: contextModeCustom, Policy: policy, Layout: "block"}
}

// renderContextField renders one policy field as the token the kernel takes.
func renderContextField(policy *ContextPolicy, field string) (string, bool) {
	if field == "triggerRatio" {
		if policy.TriggerRatio == nil {
			return "", false
		}
		return renderContextRatio(*policy.TriggerRatio), true
	}
	if policy.ReservedTokens == nil {
		return "", false
	}
	return strconv.FormatInt(*policy.ReservedTokens, 10), true
}

// applyTable edits the plain `[loop_control]` table, preserving the original value
// of every key it touches so a later release can restore it exactly.
//
// Every edit is planned first and the file is rebuilt once, because the edits
// interact: inserting a key moves the lines a later removal or replacement
// names, and a table this call appends must be extended rather than appended a
// second time.
func (c *ContextPolicyConfig) applyTable(snapshot contextScan, policy *ContextPolicy, receipt contextReceiptState) ContextStatus {
	lines := append([]tomlLine(nil), snapshot.lines...)
	eol := tomlEOL(snapshot.raw)
	original := map[string]string{}
	written := map[string]string{}
	values := map[string]string{}
	if receipt.kind == "valid" {
		// The recorded originals describe the file's earlier content, so they are
		// carried across a restart; dropping them would make a later release
		// unable to restore what was there before this module ever wrote.
		for key, value := range receipt.value.Original {
			original[key] = value
		}
	}
	covered := map[string]bool{}
	replace := map[int]tomlLine{}
	remove := map[int]bool{}
	var inserts []string
	for _, field := range contextFieldNames {
		raw, chosen := renderContextField(policy, field)
		if !chosen {
			continue
		}
		covered[field] = true
		tomlKey := contextKeyOf[field]
		values[field] = raw
		written[field] = raw
		if entry, present := snapshot.entries[field]; present {
			if _, kept := original[field]; !kept {
				original[field] = snapshot.lines[entry.line].text
			}
			replace[entry.line] = tomlLine{text: tomlKey + " = " + raw, eol: snapshot.lines[entry.line].eol}
			continue
		}
		// The key was not in the file before this module added it. Recording that
		// is what lets a later release delete the line instead of adopting it as
		// the original on the next write.
		if _, kept := original[field]; !kept {
			original[field] = contextOriginalAbsent
		}
		inserts = append(inserts, tomlKey+" = "+raw)
	}
	// A field the receipt recorded but the new policy no longer covers is handed
	// back here: its recorded original returns, or the line this module inserted
	// is removed. Leaving it behind would keep a value no receipt claims, which a
	// later release could only report as a foreign value.
	for _, field := range contextFieldNames {
		if covered[field] || receipt.kind != "valid" {
			continue
		}
		entry, present := snapshot.entries[field]
		if !present || !c.ownsValue(receipt, snapshot, field) {
			continue
		}
		if line, kept := receipt.value.Original[field]; kept && line != contextOriginalAbsent {
			replace[entry.line] = tomlLine{text: line, eol: snapshot.lines[entry.line].eol}
			continue
		}
		remove[entry.line] = true
	}
	headerEOLPatched := receipt.kind == "valid" && receipt.value.HeaderEOLPatched
	out := make([]tomlLine, 0, len(lines)+len(inserts)+1)
	inserted := false
	for i, line := range lines {
		if remove[i] {
			continue
		}
		if next, ok := replace[i]; ok {
			out = append(out, next)
		} else {
			out = append(out, line)
		}
		if !inserted && i == snapshot.headerAt && len(inserts) > 0 {
			for _, text := range inserts {
				out = append(out, tomlLine{text: text, eol: eol})
			}
			inserted = true
		}
	}
	if !inserted && len(inserts) > 0 {
		// No table yet: append one, and remember the separator this needed.
		if len(out) > 0 && out[len(out)-1].eol == "" {
			out[len(out)-1].eol = eol
			headerEOLPatched = true
		}
		out = append(out, tomlLine{text: "[" + contextTable + "]", eol: eol})
		for _, text := range inserts {
			out = append(out, tomlLine{text: text, eol: eol})
		}
		inserted = true
	}
	if !inserted && headerEOLPatched && len(out) > 0 &&
		strings.TrimSpace(out[len(out)-1].text) == "["+contextTable+"]" {
		// Every key went away, so the file ends on the header again, without the
		// newline the insert once added.
		out[len(out)-1].eol = ""
	}
	next := &contextReceiptFile{SchemaVersion: 1, OwnerID: c.ownerID, KimiDir: c.paths.homeDir,
		ConfigPath: c.paths.configPath, Layout: "table", Values: values, Written: written,
		Original: original, HeaderEOLPatched: headerEOLPatched}
	previous := c.readReceipt()
	if failure := c.writeReceipt(next); failure != "" {
		return ContextStatus{State: NativeStateConflict, Reason: failure}
	}
	rendered := joinTOML(out)
	if rendered != snapshot.raw {
		if failure := c.commit(snapshot, rendered); failure != "" {
			if previous.kind == "valid" {
				_ = c.writeReceipt(previous.value)
			} else {
				c.removeReceipt()
			}
			return ContextStatus{State: NativeStateConflict, Reason: failure}
		}
	}
	return ContextStatus{State: NativeStateFileApplied, Mode: contextModeCustom, Policy: policy, Layout: "table"}
}

// release restores the file to what it held before this module wrote anything.
func (c *ContextPolicyConfig) release(snapshot contextScan) ContextStatus {
	receipt := c.readReceipt()
	if receipt.kind == "corrupt" || receipt.kind == "unsafe" {
		return ContextStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
	}
	if receipt.kind != "valid" {
		if snapshot.block != nil {
			return ContextStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
		}
		if len(snapshot.entries) > 0 {
			return ContextStatus{State: NativeStateConflict, Reason: "foreign_value"}
		}
		return ContextStatus{State: NativeStateInherit, Mode: contextModeInherit}
	}
	if snapshot.block != nil && receipt.value.Block != nil &&
		receipt.value.Block.BodyHash != snapshot.block.digest {
		return ContextStatus{State: NativeStateConflict, Reason: "owner_conflict"}
	}
	if snapshot.block != nil {
		if failure := c.commit(snapshot, withoutSubagentBlock(contextAsSubagentScan(snapshot))); failure != "" {
			return ContextStatus{State: NativeStateConflict, Reason: failure}
		}
		c.removeReceipt()
		return ContextStatus{State: NativeStateRestored}
	}
	// Table layout: put back every key this module replaced, and delete the ones
	// it added.
	lines := append([]tomlLine(nil), snapshot.lines...)
	var remove []int
	for _, field := range contextFieldNames {
		entry, present := snapshot.entries[field]
		if !present {
			continue
		}
		if !c.ownsValue(receipt, snapshot, field) {
			return ContextStatus{State: NativeStateConflict, Reason: "foreign_value"}
		}
		if original, kept := receipt.value.Original[field]; kept && original != contextOriginalAbsent {
			lines[entry.line] = tomlLine{text: original, eol: snapshot.lines[entry.line].eol}
		} else {
			remove = append(remove, entry.line)
		}
	}
	for i := len(remove) - 1; i >= 0; i-- {
		lines = append(lines[:remove[i]], lines[remove[i]+1:]...)
	}
	// A [loop_control] header this module added is removed again once it holds
	// nothing: leaving it behind would be a change to the file we cannot
	// attribute to anything.
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i].text) != "["+contextTable+"]" {
			continue
		}
		empty := true
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j].text)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			empty = false
			break
		}
		if !empty {
			continue
		}
		// Drop the header line; if it was the last line, the file keeps no
		// trailing separator it did not have before.
		removedLast := i == len(lines)-1
		lines = append(lines[:i], lines[i+1:]...)
		if removedLast && len(lines) > 0 && receipt.value.HeaderEOLPatched {
			lines[len(lines)-1].eol = ""
		}
		i--
	}
	rendered := joinTOML(lines)
	if rendered != snapshot.raw {
		if failure := c.commit(snapshot, rendered); failure != "" {
			return ContextStatus{State: NativeStateConflict, Reason: failure}
		}
	}
	c.removeReceipt()
	return ContextStatus{State: NativeStateRestored}
}

func (c *ContextPolicyConfig) releaseOwnedReceipt() {
	if receipt := c.readReceipt(); receipt.kind == "valid" && receipt.value.OwnerID == c.ownerID {
		c.removeReceipt()
	}
}

func (c *ContextPolicyConfig) snapshot() (contextScan, string) {
	if issue := c.paths.guardConfig(); issue != "" {
		return contextScan{}, issue
	}
	raw, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		if isNotExist(err) {
			return contextScan{}, "config_missing"
		}
		return contextScan{}, "config_unavailable"
	}
	scan := scanContext(string(raw))
	if scan.issue != "" {
		return scan, scan.issue
	}
	return scan, ""
}

func (c *ContextPolicyConfig) commit(snapshot contextScan, next string) string {
	current, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		if isNotExist(err) {
			return "config_missing"
		}
		return "config_unavailable"
	}
	if string(current) != snapshot.raw {
		return "concurrent_modification"
	}
	mode := os.FileMode(0o600)
	if info, err := lstatPath(c.paths.configPath); err == nil {
		mode = info.mode
	}
	if err := writePrivateFile(c.paths.configPath, []byte(next), mode); err != nil {
		return "write_failed"
	}
	return ""
}

func renderContextBlock(policy *ContextPolicy) []string {
	body := []string{"[" + contextTable + "]"}
	if policy.TriggerRatio != nil {
		body = append(body, contextKeyOf["triggerRatio"]+" = "+renderContextRatio(*policy.TriggerRatio))
	}
	if policy.ReservedTokens != nil {
		body = append(body, contextKeyOf["reservedTokens"]+" = "+strconv.FormatInt(*policy.ReservedTokens, 10))
	}
	return body
}

// contextAsSubagentScan reuses the block editing helpers, which are identical
// for both marker blocks.
func contextAsSubagentScan(scan contextScan) subagentScan {
	return subagentScan{raw: scan.raw, lines: scan.lines, block: scan.block}
}
