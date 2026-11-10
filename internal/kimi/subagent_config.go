package kimi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The desktop owns `config.toml` and rewrites it wholesale. This module never
// edits a foreign section: it appends or replaces one marker-delimited block it
// can prove it wrote, and keeps an ownership receipt beside it so a later
// instance can take over and a generator rebuild can be repaired.
//
// The four modes are the product's own vocabulary and are preserved exactly:
//
//	inherit  no managed block; the kernel keeps its native default
//	blocked  no managed block, and the caller must not route subagents
//	force    a lone default_model, optionally with default_effort
//	pool     default_model plus a [secondary_model.models] table
const (
	SubagentModeInherit = "inherit"
	SubagentModeBlocked = "blocked"
	SubagentModeForce   = "force"
	SubagentModePool    = "pool"
)

var subagentModes = map[string]bool{SubagentModeInherit: true, SubagentModeBlocked: true,
	SubagentModeForce: true, SubagentModePool: true}

var (
	subagentOwnerLine = regexp.MustCompile(`^# owner=([0-9a-f-]{36}) body-sha256=([0-9a-f]{64}) added-eol=([01])$`)
	effortPattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// SubagentPoolEntry is one selectable model in pool mode.
type SubagentPoolEntry struct {
	ModelAlias  string `json:"modelAlias"`
	Description string `json:"description"`
}

// SubagentPolicy is the requested configuration.
type SubagentPolicy struct {
	Mode       string              `json:"mode"`
	ModelAlias string              `json:"modelAlias,omitempty"`
	Effort     string              `json:"effort,omitempty"`
	Models     []SubagentPoolEntry `json:"models,omitempty"`
}

// SubagentCapabilities records what the installed kernel supports. A kernel that
// dropped the `force` key pins a lone default_model by itself, so the key is left
// out rather than written as a value the schema would discard.
type SubagentCapabilities struct {
	ForceKeySupported *bool `json:"forceKeySupported,omitempty"`
}

// SubagentStatus is the result of an update, inspect or close.
type SubagentStatus struct {
	State     string `json:"state"`
	Transport string `json:"transport"`
	Mode      string `json:"mode,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// States a caller acts on.
const (
	NativeStateInherit     = "inherit"
	NativeStateBlocked     = "blocked"
	NativeStateForce       = "force"
	NativeStatePool        = "pool"
	NativeStateFileApplied = "file-applied"
	NativeStateWaiting     = "waiting-config"
	NativeStateConflict    = "conflict"
	NativeStateRestored    = "restored"
)

const subagentTransport = "native-config"

func subagentStatus(state string, extra ...string) SubagentStatus {
	status := SubagentStatus{State: state, Transport: subagentTransport}
	if len(extra) > 0 {
		status.Mode = extra[0]
	}
	if len(extra) > 1 {
		status.Reason = extra[1]
	}
	return status
}

// SubagentConfig writes the `[secondary_model]` block of one explicit Kimi home.
type SubagentConfig struct {
	stateDir string
	paths    configPaths
	receipt  string
	ownerID  string
}

// NewSubagentConfig prepares a writer. `kimiHome` is required and explicit: the
// caller must say which isolated tree this is, so no default can silently target
// the user's real installation.
func NewSubagentConfig(stateDir, kimiHome string) (*SubagentConfig, error) {
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
	return &SubagentConfig{stateDir: stateDir, paths: paths,
		receipt: filepath.Join(stateDir, subagentReceipt), ownerID: owner}, nil
}

// HomeDir is the Kimi home this writer targets, for diagnostics.
func (c *SubagentConfig) HomeDir() string { return c.paths.homeDir }

// ConfigPath is the file it edits.
func (c *SubagentConfig) ConfigPath() string { return c.paths.configPath }

// ---------------------------------------------------------------------------
// receipt

type subagentReceiptFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	OwnerID       string            `json:"ownerID"`
	HomeDir       string            `json:"homeDir"`
	Managed       string            `json:"managed"`
	BlockHash     string            `json:"blockHash"`
	Mode          string            `json:"mode,omitempty"`
	Previous      *subagentBlockRef `json:"previous,omitempty"`
}

// subagentBlockRef names the block a write replaced. It is what makes a crash
// between the receipt write and the config write recoverable: the receipt then
// describes a block that is not on disk yet, and the block that is on disk is
// the one recorded here.
type subagentBlockRef struct {
	OwnerID   string `json:"ownerID"`
	BlockHash string `json:"blockHash"`
}

type receiptState struct {
	kind  string // absent | unsafe | corrupt | valid
	raw   []byte
	value *subagentReceiptFile
}

func (c *SubagentConfig) readReceipt() receiptState {
	info, err := lstatPath(c.receipt)
	if err != nil {
		return receiptState{kind: "absent"}
	}
	if info.isSymlink || !info.isFile || info.size > 1<<20 {
		return receiptState{kind: "unsafe"}
	}
	raw, err := os.ReadFile(c.receipt)
	if err != nil {
		return receiptState{kind: "unsafe"}
	}
	var value subagentReceiptFile
	if json.Unmarshal(raw, &value) != nil || value.SchemaVersion != 1 ||
		!uuidPattern.MatchString(value.OwnerID) || value.HomeDir == "" ||
		len(value.Managed) > 1<<20 || len(value.BlockHash) != 64 {
		return receiptState{kind: "corrupt"}
	}
	return receiptState{kind: "valid", raw: raw, value: &value}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (c *SubagentConfig) writeReceipt(value *subagentReceiptFile) string {
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

func (c *SubagentConfig) removeReceipt() { removeFile(c.receipt) }

// ---------------------------------------------------------------------------
// scan

type subagentBlock struct {
	begin    int
	end      int
	ownerID  string
	digest   string
	addedEOL bool
	mode     string
	aliases  []string
}

type subagentScan struct {
	raw      string
	lines    []tomlLine
	block    *subagentBlock
	declared map[string]bool
	foreign  bool
	issue    string
}

// scanConfig reads the file's structure: the managed block, the model aliases the
// kernel declares, and whether a foreign `[secondary_model]` section exists.
func scanConfig(raw string) subagentScan {
	lines := splitTOML(raw)
	scan := subagentScan{raw: raw, lines: lines, declared: map[string]bool{}}
	var begins, ends []int
	for i, line := range lines {
		switch strings.TrimSpace(line.text) {
		case subagentBlockBegin:
			begins = append(begins, i)
		case subagentBlockEnd:
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
	// A near-miss marker outside our block is ambiguous: refuse rather than scan
	// the wrong region.
	for i, line := range lines {
		if start >= 0 && i >= start && i <= stop {
			continue
		}
		if strings.Contains(line.text, markerWord) {
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
		parsed, ok := parseSubagentBlockBody(body)
		if !ok || tomlDigest(body) != owner[2] {
			scan.issue = "block_modified"
			return scan
		}
		// A top-level `key = value` right after the block would extend our last
		// table; a new header is safe because it ends that table.
		header := false
		for _, line := range lines[stop+1:] {
			trimmed := strings.TrimSpace(line.text)
			if header || trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") {
				header = true
				continue
			}
			scan.issue = "block_not_terminal"
			return scan
		}
		aliases := parsed.aliases
		if parsed.mode == SubagentModeForce {
			aliases = []string{parsed.defaultModel}
		}
		scan.block = &subagentBlock{begin: start, end: stop, ownerID: owner[1], digest: owner[2],
			addedEOL: owner[3] == "1", mode: parsed.mode, aliases: aliases}
	}
	// Declared aliases and any foreign secondary_model section. The managed
	// block is skipped: its own [secondary_model.models] table is ours, not the
	// kernel's declaration, and counting it would make a managed alias look
	// declared even after the user removed it.
	table := ""
	for i, line := range lines {
		if scan.block != nil && i >= scan.block.begin && i <= scan.block.end {
			continue
		}
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if _, issue := tomlCode(line.text); issue != "" {
			scan.issue = issue
			return scan
		}
		// The leading offset is taken from the original line, because `code`
		// masks a quoted key completely and would leave no non-space character
		// to find.
		lead := strings.IndexFunc(line.text, func(r rune) bool { return r != ' ' && r != '\t' })
		if lead < 0 {
			continue
		}
		if line.text[lead] == '[' {
			// The header is read from the original line: a quoted segment is
			// masked in `code`, and the closing bracket index must match.
			close := strings.LastIndexByte(line.text, ']')
			if close < 0 {
				table = ""
				continue
			}
			segments := tomlKeySegments(line.text, lead+1, close)
			switch {
			case len(segments) == 1 && segments[0] == "secondary_model":
				scan.foreign = true
				table = "secondary_model"
			case len(segments) == 2 && segments[0] == "secondary_model" && segments[1] == "models":
				table = "secondary_model.models"
			default:
				table = ""
			}
			continue
		}
		// `code` masks a quoted key with spaces, so both the delimiter and the
		// key come from the original line. The masked form is only used to find
		// the first non-space character, which is never inside a quote.
		eq := strings.IndexByte(line.text, '=')
		if eq < 0 {
			continue
		}
		key := tomlKeySegments(line.text, lead, eq)
		if len(key) != 1 {
			continue
		}
		if table == "secondary_model.models" {
			if _, ok := tomlValueString(line.text, eq+1); ok {
				scan.declared[key[0]] = true
			}
		}
	}
	return scan
}

// parsedBlock is the block body read back from disk.
type parsedBlock struct {
	mode         string
	defaultModel string
	effort       string
	aliases      []string
	force        bool
}

// parseSubagentBlockBody reads the block this module writes. Anything else is
// refused: the block is ours only if it is exactly the shape we render.
func parseSubagentBlockBody(body []string) (parsedBlock, bool) {
	var out parsedBlock
	table := ""
	for _, text := range body {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if _, issue := tomlCode(text); issue != "" {
			return parsedBlock{}, false
		}
		// The leading offset is taken from the original line, because `code`
		// masks a quoted key completely and would leave no non-space character
		// to find.
		lead := strings.IndexFunc(text, func(r rune) bool { return r != ' ' && r != '\t' })
		if lead < 0 {
			continue
		}
		if text[lead] == '[' {
			segments := tomlKeySegments(text, lead+1, strings.LastIndexByte(text, ']'))
			switch {
			case len(segments) == 1 && segments[0] == "secondary_model":
				table = "section"
			case len(segments) == 2 && segments[0] == "secondary_model" && segments[1] == "models":
				table = "models"
			default:
				return parsedBlock{}, false
			}
			continue
		}
		eq := strings.IndexByte(text, '=')
		if eq < 0 {
			return parsedBlock{}, false
		}
		key := tomlKeySegments(text, lead, eq)
		if len(key) != 1 {
			return parsedBlock{}, false
		}
		switch {
		case table == "section" && key[0] == "force":
			if strings.TrimSpace(lineAfter(text, eq+1)) != "true" {
				return parsedBlock{}, false
			}
			out.force = true
		case table == "section" && key[0] == "default_model":
			value, ok := tomlValueString(text, eq+1)
			if !ok {
				return parsedBlock{}, false
			}
			out.defaultModel = value
		case table == "section" && key[0] == "default_effort":
			value, ok := tomlValueString(text, eq+1)
			if !ok {
				return parsedBlock{}, false
			}
			out.effort = value
		case table == "models":
			value, ok := tomlValueString(text, eq+1)
			if !ok || value == "" {
				return parsedBlock{}, false
			}
			out.aliases = append(out.aliases, key[0])
		default:
			return parsedBlock{}, false
		}
	}
	if out.defaultModel == "" {
		return parsedBlock{}, false
	}
	if out.force {
		if len(out.aliases) > 0 {
			return parsedBlock{}, false
		}
		out.mode = SubagentModeForce
		return out, true
	}
	// Newer kernels use a lone default_model for ordinary child agents while
	// retaining their native primary, fork and resume behaviour.
	if len(out.aliases) == 0 {
		out.mode = SubagentModeForce
		return out, true
	}
	for _, alias := range out.aliases {
		if alias == out.defaultModel {
			out.mode = SubagentModePool
			return out, true
		}
	}
	return parsedBlock{}, false
}

func lineAfter(text string, at int) string {
	if at >= len(text) {
		return ""
	}
	return text[at:]
}

// ---------------------------------------------------------------------------
// validation and rendering

func (p SubagentPolicy) valid() bool {
	switch p.Mode {
	case SubagentModeForce:
		if !tomlTextValid(p.ModelAlias, 2048) {
			return false
		}
		return p.Effort == "" || effortPattern.MatchString(p.Effort)
	case SubagentModePool:
		if !tomlTextValid(p.ModelAlias, 2048) || len(p.Models) == 0 || len(p.Models) > 32 {
			return false
		}
		seen := map[string]bool{}
		for _, row := range p.Models {
			if !tomlTextValid(row.ModelAlias, 2048) || seen[row.ModelAlias] {
				return false
			}
			if len(row.Description) > 1000 || !tomlTextValidAllowEmpty(row.Description) {
				return false
			}
			// The marker word may only ever appear in the lines this module writes.
			if strings.Contains(row.Description, markerWord) || strings.Contains(row.ModelAlias, markerWord) {
				return false
			}
			seen[row.ModelAlias] = true
		}
		return seen[p.ModelAlias]
	}
	return false
}

func tomlTextValidAllowEmpty(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// renderSubagentBlock produces the block body and its digest.
func renderSubagentBlock(policy SubagentPolicy, capabilities SubagentCapabilities) ([]string, string) {
	forceKey := true
	if capabilities.ForceKeySupported != nil && !*capabilities.ForceKeySupported {
		forceKey = false
	}
	var body []string
	if policy.Mode == SubagentModeForce {
		body = append(body, "[secondary_model]")
		if forceKey {
			body = append(body, "force = true")
		}
		body = append(body, "default_model = "+tomlString(policy.ModelAlias))
		if policy.Effort != "" {
			body = append(body, "default_effort = "+tomlString(policy.Effort))
		}
	} else {
		body = append(body, "[secondary_model]",
			"default_model = "+tomlString(policy.ModelAlias),
			"[secondary_model.models]")
		for _, row := range policy.Models {
			body = append(body, tomlString(row.ModelAlias)+" = "+tomlString(row.Description))
		}
	}
	return body, tomlDigest(body)
}

// blockMarkers are the two comment lines that delimit a managed block. The
// subagent and context writers each own one, and a writer must never rewrite or
// delete the other's block.
type blockMarkers struct {
	begin string
	end   string
}

var (
	subagentMarkers = blockMarkers{begin: subagentBlockBegin, end: subagentBlockEnd}
	contextMarkers  = blockMarkers{begin: contextBlockBegin, end: contextBlockEnd}
)

func managedBlockLines(body []string, markers blockMarkers, ownerID string, addedEOL bool) []string {
	flag := "0"
	if addedEOL {
		flag = "1"
	}
	out := []string{markers.begin, "# owner=" + ownerID + " body-sha256=" + tomlDigest(body) + " added-eol=" + flag}
	out = append(out, body...)
	return append(out, markers.end)
}

// ---------------------------------------------------------------------------
// editing

// withManagedBlock replaces or appends the managed block, preserving every other
// byte including the file's line endings.
func withManagedBlock(scan subagentScan, body []string, markers blockMarkers, ownerID, eol string) (string, bool) {
	lines := append([]tomlLine(nil), scan.lines...)
	if scan.block != nil {
		text := managedBlockLines(body, markers, ownerID, scan.block.addedEOL)
		closing := lines[scan.block.end].eol
		rendered := make([]tomlLine, len(text))
		for i, line := range text {
			lineEOL := eol
			if i == len(text)-1 {
				lineEOL = closing
			}
			rendered[i] = tomlLine{text: line, eol: lineEOL}
		}
		out := append([]tomlLine(nil), lines[:scan.block.begin]...)
		out = append(out, rendered...)
		out = append(out, lines[scan.block.end+1:]...)
		return joinTOML(out), scan.block.addedEOL
	}
	// Appending needs one separator newline when the file does not end with one;
	// the block records that so removal can restore the original bytes.
	addedEOL := !strings.HasSuffix(scan.raw, "\n") && !strings.HasSuffix(scan.raw, "\r")
	text := managedBlockLines(body, markers, ownerID, addedEOL)
	if addedEOL && len(lines) > 0 {
		lines[len(lines)-1].eol = eol
	}
	trailing := eol
	if addedEOL {
		trailing = ""
	}
	for i, line := range text {
		lineEOL := eol
		if i == len(text)-1 {
			lineEOL = trailing
		}
		lines = append(lines, tomlLine{text: line, eol: lineEOL})
	}
	return joinTOML(lines), addedEOL
}

// withoutSubagentBlock removes the block and restores the surrounding bytes.
func withoutSubagentBlock(scan subagentScan) string {
	start, end := 0, 0
	for i := 0; i <= scan.block.end; i++ {
		size := len(scan.lines[i].text) + len(scan.lines[i].eol)
		if i < scan.block.begin {
			start += size
		}
		end += size
	}
	head := scan.raw[:start]
	tail := scan.raw[end:]
	if tail == "" && scan.block.addedEOL {
		head = strings.TrimSuffix(strings.TrimSuffix(head, "\r\n"), "\n")
	}
	return head + tail
}

// ---------------------------------------------------------------------------
// public operations

// Update applies a policy. It returns a status rather than an error, because
// every refusal is a state the panel renders.
func (c *SubagentConfig) Update(policy SubagentPolicy, capabilities SubagentCapabilities) SubagentStatus {
	if !subagentModes[policy.Mode] {
		return subagentStatus(NativeStateConflict, policy.Mode, "invalid_policy")
	}
	if (policy.Mode == SubagentModeForce || policy.Mode == SubagentModePool) && !policy.valid() {
		return subagentStatus(NativeStateConflict, policy.Mode, "invalid_policy")
	}
	snapshot, issue := c.snapshot()
	if issue != "" {
		if issue == "config_missing" && (policy.Mode == SubagentModeInherit || policy.Mode == SubagentModeBlocked) {
			c.releaseOwnedReceipt()
			return subagentStatus(policy.Mode, policy.Mode)
		}
		if issue == "config_missing" {
			return subagentStatus(NativeStateWaiting, policy.Mode, issue)
		}
		return subagentStatus(NativeStateConflict, policy.Mode, issue)
	}
	owned, ownershipIssue := c.ownership(snapshot)
	if ownershipIssue != "" {
		return subagentStatus(NativeStateConflict, policy.Mode, ownershipIssue)
	}
	if policy.Mode == SubagentModeInherit || policy.Mode == SubagentModeBlocked {
		superseded, revokeIssue := c.revoke(snapshot, owned)
		if revokeIssue != "" {
			return subagentStatus(NativeStateConflict, policy.Mode, revokeIssue)
		}
		// A block a newer instance still holds keeps its policy: this instance
		// must not report the requested mode as if it had taken effect.
		if superseded {
			return subagentStatus(NativeStateConflict, policy.Mode, "ownership_superseded")
		}
		return subagentStatus(policy.Mode, policy.Mode)
	}
	aliases := []string{policy.ModelAlias}
	if policy.Mode == SubagentModePool {
		aliases = aliases[:0]
		seen := map[string]bool{}
		for _, row := range policy.Models {
			if !seen[row.ModelAlias] {
				seen[row.ModelAlias] = true
				aliases = append(aliases, row.ModelAlias)
			}
		}
	}
	for _, alias := range aliases {
		if !snapshot.declared[alias] {
			// A block naming an undeclared model would make the kernel's own
			// section invalid, so ours is dropped and the policy waits.
			superseded, revokeIssue := c.revoke(snapshot, owned)
			if revokeIssue != "" {
				return subagentStatus(NativeStateConflict, policy.Mode, revokeIssue)
			}
			if superseded {
				return subagentStatus(NativeStateConflict, policy.Mode, "ownership_superseded")
			}
			return subagentStatus(NativeStateWaiting, policy.Mode, "models_missing")
		}
	}
	body, digest := renderSubagentBlock(policy, capabilities)
	rendered, addedEOL := withManagedBlock(snapshot, body, subagentMarkers, c.ownerID, snapshot.eol())
	receipt := &subagentReceiptFile{SchemaVersion: 1, OwnerID: c.ownerID, HomeDir: c.paths.homeDir,
		BlockHash: digest, Managed: strings.Join(managedBlockLines(body, subagentMarkers, c.ownerID, addedEOL), "\n"),
		Mode: policy.Mode}
	if snapshot.block != nil && rendered != snapshot.raw {
		// The block this write replaces is recorded so a crash between the
		// receipt and the config write is still attributable on restart.
		receipt.Previous = &subagentBlockRef{OwnerID: snapshot.block.ownerID, BlockHash: snapshot.block.digest}
	}
	previous := c.readReceipt()
	if failure := c.writeReceipt(receipt); failure != "" {
		return subagentStatus(NativeStateConflict, policy.Mode, failure)
	}
	if rendered != snapshot.raw {
		if failure := c.commit(snapshot, rendered); failure != "" {
			// Ownership must not outlive a failed apply.
			if previous.kind == "valid" {
				_ = c.writeReceipt(previous.value)
			} else {
				c.removeReceipt()
			}
			if failure == "config_missing" {
				return subagentStatus(NativeStateWaiting, policy.Mode, failure)
			}
			return subagentStatus(NativeStateConflict, policy.Mode, failure)
		}
	}
	return subagentStatus(NativeStateFileApplied, policy.Mode)
}

// Inspect reports what is on disk without writing anything.
func (c *SubagentConfig) Inspect() SubagentStatus {
	snapshot, issue := c.snapshot()
	if issue != "" {
		if issue == "config_missing" {
			return subagentStatus(NativeStateWaiting, "", issue)
		}
		return subagentStatus(NativeStateConflict, "", issue)
	}
	owned, ownershipIssue := c.ownership(snapshot)
	if ownershipIssue != "" {
		return subagentStatus(NativeStateConflict, "", ownershipIssue)
	}
	if owned == "none" {
		if receipt := c.readReceipt(); receipt.kind == "valid" && receipt.value.OwnerID == c.ownerID {
			return subagentStatus(NativeStateWaiting, "", "managed_block_missing")
		}
		return subagentStatus(NativeStateInherit, "")
	}
	if owned == "recoverable" {
		return subagentStatus(NativeStateWaiting, "", "handoff_pending")
	}
	block := snapshot.block
	for _, alias := range block.aliases {
		if !snapshot.declared[alias] {
			return subagentStatus(NativeStateWaiting, block.mode, "models_missing")
		}
	}
	return subagentStatus(NativeStateFileApplied, block.mode)
}

// Close removes the managed block, restoring the file's original bytes.
func (c *SubagentConfig) Close() SubagentStatus {
	snapshot, issue := c.snapshot()
	if issue != "" {
		if issue == "config_missing" {
			c.releaseOwnedReceipt()
			return subagentStatus(NativeStateRestored)
		}
		return subagentStatus(NativeStateConflict, "", issue)
	}
	owned, ownershipIssue := c.ownership(snapshot)
	if ownershipIssue != "" {
		return subagentStatus(NativeStateConflict, "", ownershipIssue)
	}
	switch owned {
	case "none":
		if receipt := c.readReceipt(); receipt.kind == "valid" && receipt.value.OwnerID == c.ownerID {
			c.removeReceipt()
			return subagentStatus(NativeStateRestored)
		}
		return subagentStatus(NativeStateInherit, "", "no_managed_block")
	case "superseded":
		// Never erase a block another instance is holding.
		return subagentStatus(NativeStateInherit, "", "ownership_superseded")
	case "adoptable":
		// Nor one whose receipt is gone: it cannot be attributed.
		return subagentStatus(NativeStateConflict, "", "receipt_unverified")
	}
	if failure := c.commit(snapshot, withoutSubagentBlock(snapshot)); failure != "" {
		return subagentStatus(NativeStateConflict, "", failure)
	}
	c.releaseOwnedReceipt()
	// A closed instance no longer claims the owner id, so a later close of the
	// same object cannot delete a block a new instance wrote.
	if fresh, err := randomUUID(); err == nil {
		c.ownerID = fresh
	}
	return subagentStatus(NativeStateRestored)
}

// ---------------------------------------------------------------------------
// internals

func (s subagentScan) eol() string { return tomlEOL(s.raw) }

func (c *SubagentConfig) snapshot() (subagentScan, string) {
	if issue := c.paths.guardConfig(); issue != "" {
		return subagentScan{}, issue
	}
	raw, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		if isNotExist(err) {
			return subagentScan{}, "config_missing"
		}
		return subagentScan{}, "config_unavailable"
	}
	scan := scanConfig(string(raw))
	if scan.issue != "" {
		return scan, scan.issue
	}
	if scan.foreign {
		return scan, "foreign_secondary_model"
	}
	return scan, ""
}

// ownership answers who holds the block: ours, superseded, adoptable, or a
// conflict that must not be written.
func (c *SubagentConfig) ownership(snapshot subagentScan) (string, string) {
	block := snapshot.block
	if block == nil {
		return "none", ""
	}
	receipt := c.readReceipt()
	if receipt.kind != "valid" {
		return "adoptable", ""
	}
	if receipt.value.HomeDir != c.paths.homeDir {
		return "", "owner_conflict"
	}
	if receipt.value.OwnerID != block.ownerID || receipt.value.BlockHash != block.digest {
		// A write that recorded the block it replaced but never reached the file
		// leaves the receipt describing the new block while the old one is still
		// on disk. That is recoverable, not a conflict: the next write replaces
		// the old block and the disabled path still may not delete it.
		previous := receipt.value.Previous
		if previous != nil && previous.OwnerID == block.ownerID && previous.BlockHash == block.digest {
			if previous.OwnerID == c.ownerID {
				return "superseded", ""
			}
			return "recoverable", ""
		}
		return "", "owner_conflict"
	}
	if receipt.value.OwnerID == c.ownerID {
		return "ours", ""
	}
	// A valid receipt naming another instance means that instance still holds
	// the block: this one may replace it but must never delete it.
	return "superseded", ""
}

// revoke removes the managed block for an inherit/blocked request.
func (c *SubagentConfig) revoke(snapshot subagentScan, owned string) (bool, string) {
	if owned == "none" {
		c.releaseOwnedReceipt()
		return false, ""
	}
	if owned == "superseded" {
		return true, ""
	}
	if owned == "adoptable" {
		return false, "receipt_unverified"
	}
	if failure := c.commit(snapshot, withoutSubagentBlock(snapshot)); failure != "" {
		return false, failure
	}
	c.removeReceipt()
	return false, ""
}

// releaseOwnedReceipt drops a receipt this instance owns when no block remains.
func (c *SubagentConfig) releaseOwnedReceipt() {
	if receipt := c.readReceipt(); receipt.kind == "valid" && receipt.value.OwnerID == c.ownerID {
		c.removeReceipt()
	}
}

// commit replaces the config file, and only while the bytes that were parsed are
// still the bytes on disk: the generator or an editor racing this call wins.
func (c *SubagentConfig) commit(snapshot subagentScan, next string) string {
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

// digestHex is the ownership hash used by the services module.
func digestHex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
