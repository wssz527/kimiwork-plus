package kimi

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The web tools are two kernel services, each a TOML table:
//
//	[services.moonshot_search]  base_url / api_key   → WebSearch
//	[services.moonshot_fetch]   base_url / api_key   → FetchURL
//
// Writing these tables is the one real way to replace the official search and
// fetch services, so every rule here is about not writing something the panel
// would then misreport: an enabled service without an endpoint and a key is
// never written (the kernel would fall back to the account provider anyway,
// which would make "enabled" a lie), and a saved key is reused only for the same
// endpoint because a key belongs to the address it was entered for.

const (
	ServiceSearch = "search"
	ServiceFetch  = "fetch"

	serviceURLMax = 2048
	serviceKeyMax = 4096

	servicesRepairLimit  = 3
	servicesRepairWindow = 10 * time.Minute
)

var serviceTables = map[string]string{ServiceSearch: "moonshot_search", ServiceFetch: "moonshot_fetch"}
var serviceOfTable = map[string]string{"moonshot_search": ServiceSearch, "moonshot_fetch": ServiceFetch}

// ServiceSettings is one service's requested state.
type ServiceSettings struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseURL,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
}

// ServicesInput is the caller's request. A service that is omitted keeps its
// saved intent; `enabled: false` restores that one table and leaves the other.
type ServicesInput struct {
	Search *ServiceSettings `json:"search,omitempty"`
	Fetch  *ServiceSettings `json:"fetch,omitempty"`
}

// ServiceOutcome is one service's reported state.
type ServiceOutcome struct {
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	BaseURL string `json:"baseURL,omitempty"`
	Applied bool   `json:"applied,omitempty"`
}

// ServicesStatus is the whole answer.
type ServicesStatus struct {
	State   string                    `json:"state"`
	Reason  string                    `json:"reason,omitempty"`
	Search  *ServiceOutcome           `json:"search,omitempty"`
	Fetch   *ServiceOutcome           `json:"fetch,omitempty"`
	Details map[string]map[string]any `json:"details,omitempty"`
}

// ServicesConfig edits one explicit Kimi home.
type ServicesConfig struct {
	stateDir string
	paths    configPaths
	receipt  string
	intent   string
	now      func() time.Time
}

// NewServicesConfig prepares a writer for an explicit Kimi home.
func NewServicesConfig(stateDir, kimiHome string) (*ServicesConfig, error) {
	if stateDir == "" || !isAbsPath(stateDir) {
		return nil, errors.New("state_dir_required")
	}
	paths, err := newConfigPaths(kimiHome)
	if err != nil {
		return nil, err
	}
	return &ServicesConfig{stateDir: stateDir, paths: paths,
		receipt: filepath.Join(stateDir, servicesReceipt),
		intent:  filepath.Join(stateDir, servicesIntent),
		now:     time.Now}, nil
}

// NormalizeServiceURL validates one endpoint.
//
// The kernel posts to `base_url` verbatim, so the address must already be the
// complete endpoint. Plain HTTP is accepted only for the machine itself: a key
// typed into the panel must not travel in clear text to another host. A query
// string is refused because it would put a credential into a file this module
// can only treat as public.
func NormalizeServiceURL(value string) string {
	text := strings.TrimSpace(value)
	if text == "" || len(text) > serviceURLMax {
		return ""
	}
	for _, character := range text {
		if character < 0x20 || character == 0x7f {
			return ""
		}
	}
	parsed, err := url.Parse(text)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return ""
	}
	return parsed.String()
}

func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost", "[::1]":
		return true
	}
	return false
}

func validServiceKey(value string) bool {
	if value == "" || len(value) > serviceKeyMax {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// intent and receipt

type servicesIntentFile struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Search        *serviceIntentEntry `json:"search,omitempty"`
	Fetch         *serviceIntentEntry `json:"fetch,omitempty"`
}

type serviceIntentEntry struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseURL,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
}

type servicesReceiptFile struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Search        *serviceReceiptEntry `json:"search,omitempty"`
	Fetch         *serviceReceiptEntry `json:"fetch,omitempty"`
	Attempts      map[string][]int64   `json:"attempts,omitempty"`
}

type serviceReceiptEntry struct {
	Applied bool   `json:"applied"`
	BaseURL string `json:"baseURL,omitempty"`
	KeyHash string `json:"keyHash,omitempty"`
	At      int64  `json:"at,omitempty"`
}

type jsonFileState struct {
	kind  string
	raw   []byte
	value json.RawMessage
}

func readJSONState(path string, limit int64) jsonFileState {
	info, err := lstatPath(path)
	if err != nil {
		return jsonFileState{kind: "absent"}
	}
	if info.isSymlink || !info.isFile || info.size > limit {
		return jsonFileState{kind: "unsafe"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return jsonFileState{kind: "unsafe"}
	}
	if !json.Valid(raw) {
		return jsonFileState{kind: "corrupt"}
	}
	return jsonFileState{kind: "valid", raw: raw, value: raw}
}

func (c *ServicesConfig) readIntent() (servicesIntentFile, jsonFileState) {
	state := readJSONState(c.intent, 1<<20)
	if state.kind != "valid" {
		return servicesIntentFile{}, state
	}
	var intent servicesIntentFile
	if json.Unmarshal(state.raw, &intent) != nil || intent.SchemaVersion != 1 {
		return servicesIntentFile{}, jsonFileState{kind: "corrupt"}
	}
	return intent, state
}

func (c *ServicesConfig) readReceipt() (servicesReceiptFile, jsonFileState) {
	state := readJSONState(c.receipt, 1<<20)
	if state.kind != "valid" {
		return servicesReceiptFile{}, state
	}
	var receipt servicesReceiptFile
	if json.Unmarshal(state.raw, &receipt) != nil || receipt.SchemaVersion != 1 {
		return servicesReceiptFile{}, jsonFileState{kind: "corrupt"}
	}
	return receipt, state
}

func (c *ServicesConfig) writeJSON(path string, value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "write_failed"
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return "write_failed"
	}
	if info, err := lstatPath(c.stateDir); err != nil || info.isSymlink || !info.isDir {
		return "write_failed"
	}
	if err := writePrivateFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "write_failed"
	}
	return ""
}

// ---------------------------------------------------------------------------
// scan

type serviceScan struct {
	raw    string
	lines  []tomlLine
	tables map[string]*serviceTable
	issue  string
}

type serviceTable struct {
	headerAt int
	endAt    int
	baseURL  string
	baseLine int
	keyLine  int
	hasBase  bool
	hasKey   bool
}

// scanServices reads the two service tables without re-serialising anything.
func scanServices(raw string) serviceScan {
	lines := splitTOML(raw)
	scan := serviceScan{raw: raw, lines: lines, tables: map[string]*serviceTable{}}
	table := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if _, issue := tomlCode(line.text); issue != "" {
			scan.issue = issue
			return scan
		}
		lead := strings.IndexFunc(line.text, func(r rune) bool { return r != ' ' && r != '\t' })
		if lead < 0 {
			continue
		}
		if line.text[lead] == '[' {
			close := strings.LastIndexByte(line.text, ']')
			if close < 0 {
				table = ""
				continue
			}
			segments := tomlKeySegments(line.text, lead+1, close)
			table = ""
			if len(segments) == 2 && segments[0] == "services" {
				if service, ok := serviceOfTable[segments[1]]; ok {
					scan.tables[service] = &serviceTable{headerAt: i, endAt: i, baseLine: -1, keyLine: -1}
					table = service
				}
			}
			continue
		}
		if table == "" {
			continue
		}
		current := scan.tables[table]
		current.endAt = i
		eq := strings.IndexByte(line.text, '=')
		if eq < 0 {
			continue
		}
		key := tomlKeySegments(line.text, lead, eq)
		if len(key) != 1 {
			continue
		}
		switch key[0] {
		case "base_url":
			value, ok := tomlValueString(line.text, eq+1)
			if !ok {
				scan.issue = "unreadable_value"
				return scan
			}
			current.baseURL = value
			current.baseLine = i
			current.hasBase = true
		case "api_key":
			if _, ok := tomlValueString(line.text, eq+1); !ok {
				scan.issue = "unreadable_value"
				return scan
			}
			current.keyLine = i
			current.hasKey = true
		}
	}
	return scan
}

// ---------------------------------------------------------------------------
// public operations

// State reports what is configured without writing anything.
func (c *ServicesConfig) State() ServicesStatus {
	intent, intentState := c.readIntent()
	receipt, receiptState := c.readReceipt()
	status := ServicesStatus{Details: map[string]map[string]any{}}
	if intentState.kind == "corrupt" || intentState.kind == "unsafe" {
		return ServicesStatus{State: NativeStateConflict, Reason: "intent_unverified"}
	}
	if receiptState.kind == "corrupt" || receiptState.kind == "unsafe" {
		return ServicesStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
	}
	if issue := c.paths.guardConfig(); issue != "" {
		if issue == "config_missing" {
			status.State = NativeStateWaiting
			status.Reason = issue
		} else {
			status.State = NativeStateConflict
			status.Reason = issue
		}
		status.Search = c.blockedOutcome(intent.Search)
		status.Fetch = c.blockedOutcome(intent.Fetch)
		return status
	}
	raw, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		status.State = NativeStateConflict
		status.Reason = "config_unavailable"
		return status
	}
	scan := scanServices(string(raw))
	if scan.issue != "" {
		status.State = NativeStateConflict
		status.Reason = scan.issue
		return status
	}
	status.Search = c.outcomeOf(scan, ServiceSearch, intent.Search, receipt.Search)
	status.Fetch = c.outcomeOf(scan, ServiceFetch, intent.Fetch, receipt.Fetch)
	switch {
	case status.Search.State == NativeStateConflict || status.Fetch.State == NativeStateConflict:
		status.State = NativeStateConflict
	case status.Search.State == "enabled" || status.Fetch.State == "enabled":
		status.State = "enabled"
	case status.Search.State == "disabled" && status.Fetch.State == "disabled":
		status.State = "disabled"
	default:
		status.State = NativeStateInherit
	}
	return status
}

func (c *ServicesConfig) blockedOutcome(wanted *serviceIntentEntry) *ServiceOutcome {
	if wanted == nil || !wanted.Enabled {
		return &ServiceOutcome{State: "disabled"}
	}
	return &ServiceOutcome{State: NativeStateWaiting, Reason: "config_missing"}
}

// outcomeOf compares one service's wanted state against the file.
func (c *ServicesConfig) outcomeOf(scan serviceScan, service string, wanted *serviceIntentEntry,
	receipt *serviceReceiptEntry) *ServiceOutcome {
	table := scan.tables[service]
	if wanted == nil || !wanted.Enabled {
		if table == nil {
			return &ServiceOutcome{State: "disabled"}
		}
		if receipt != nil && receipt.Applied {
			// Still applied: the caller asked for it off and the file has not
			// caught up yet.
			return &ServiceOutcome{State: NativeStateWaiting, Reason: "removal_pending"}
		}
		return &ServiceOutcome{State: NativeStateConflict, Reason: "foreign_service"}
	}
	if table == nil {
		return &ServiceOutcome{State: NativeStateWaiting, Reason: "table_missing", BaseURL: wanted.BaseURL}
	}
	if !table.hasBase || !table.hasKey {
		return &ServiceOutcome{State: NativeStateConflict, Reason: "incomplete_service", BaseURL: wanted.BaseURL}
	}
	if NormalizeServiceURL(table.baseURL) != wanted.BaseURL {
		return &ServiceOutcome{State: NativeStateConflict, Reason: "foreign_value", BaseURL: wanted.BaseURL}
	}
	if receipt == nil || !receipt.Applied || receipt.BaseURL != wanted.BaseURL ||
		receipt.KeyHash != digestHex(wanted.APIKey) {
		return &ServiceOutcome{State: NativeStateConflict, Reason: "foreign_value", BaseURL: wanted.BaseURL}
	}
	return &ServiceOutcome{State: "enabled", BaseURL: wanted.BaseURL, Applied: true}
}

// Update applies the request. It never writes an enabled service without an
// endpoint and a key.
func (c *ServicesConfig) Update(input ServicesInput) ServicesStatus {
	if input.Search == nil && input.Fetch == nil {
		return ServicesStatus{State: NativeStateConflict, Reason: "invalid_services"}
	}
	current, intentState := c.readIntent()
	if intentState.kind == "corrupt" || intentState.kind == "unsafe" {
		return ServicesStatus{State: NativeStateConflict, Reason: "intent_unverified"}
	}
	if _, receiptState := c.readReceipt(); receiptState.kind == "corrupt" || receiptState.kind == "unsafe" {
		return ServicesStatus{State: NativeStateConflict, Reason: "receipt_unverified"}
	}
	next := servicesIntentFile{SchemaVersion: 1, Search: current.Search, Fetch: current.Fetch}
	for _, service := range []string{ServiceSearch, ServiceFetch} {
		row := input.Search
		if service == ServiceFetch {
			row = input.Fetch
		}
		if row == nil {
			continue
		}
		if !row.Enabled {
			entry := &serviceIntentEntry{Enabled: false}
			if service == ServiceSearch {
				next.Search = entry
			} else {
				next.Fetch = entry
			}
			continue
		}
		baseURL := NormalizeServiceURL(row.BaseURL)
		if baseURL == "" {
			return ServicesStatus{State: NativeStateConflict, Reason: "invalid_service_endpoint"}
		}
		saved := ""
		previous := current.Search
		if service == ServiceFetch {
			previous = current.Fetch
		}
		if previous != nil && previous.Enabled && previous.BaseURL == baseURL {
			saved = previous.APIKey
		}
		apiKey := row.APIKey
		if apiKey == "" {
			apiKey = saved
		}
		if !validServiceKey(apiKey) {
			return ServicesStatus{State: NativeStateConflict, Reason: "invalid_service_key"}
		}
		entry := &serviceIntentEntry{Enabled: true, BaseURL: baseURL, APIKey: apiKey}
		if service == ServiceSearch {
			next.Search = entry
		} else {
			next.Fetch = entry
		}
	}
	if failure := c.writeJSON(c.intent, next); failure != "" {
		return ServicesStatus{State: NativeStateConflict, Reason: failure}
	}

	status := ServicesStatus{Details: map[string]map[string]any{}}
	if issue := c.paths.guardConfig(); issue != "" {
		status.State = NativeStateWaiting
		status.Reason = issue
		status.Search = c.blockedOutcome(next.Search)
		status.Fetch = c.blockedOutcome(next.Fetch)
		return status
	}
	for _, service := range []string{ServiceSearch, ServiceFetch} {
		outcome := c.applyService(service)
		if service == ServiceSearch {
			status.Search = outcome
		} else {
			status.Fetch = outcome
		}
	}
	status.State = combinedServicesState(status.Search, status.Fetch)
	return status
}

func combinedServicesState(search, fetch *ServiceOutcome) string {
	for _, outcome := range []*ServiceOutcome{search, fetch} {
		if outcome != nil && outcome.State == NativeStateConflict {
			return NativeStateConflict
		}
	}
	for _, outcome := range []*ServiceOutcome{search, fetch} {
		if outcome != nil && (outcome.State == "enabled" || outcome.State == NativeStateFileApplied) {
			return NativeStateFileApplied
		}
	}
	for _, outcome := range []*ServiceOutcome{search, fetch} {
		if outcome != nil && outcome.State == NativeStateWaiting {
			return NativeStateWaiting
		}
	}
	return "disabled"
}

// applyService reconciles one table against the saved intent, against a fresh
// read: the other service may have just written the file.
func (c *ServicesConfig) applyService(service string) *ServiceOutcome {
	intent, _ := c.readIntent()
	wanted := intent.Search
	if service == ServiceFetch {
		wanted = intent.Fetch
	}
	raw, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		return &ServiceOutcome{State: NativeStateWaiting, Reason: "config_missing"}
	}
	scan := scanServices(string(raw))
	if scan.issue != "" {
		return &ServiceOutcome{State: NativeStateConflict, Reason: scan.issue}
	}
	tableName := serviceTables[service]
	table := scan.tables[service]
	lines := append([]tomlLine(nil), scan.lines...)
	eol := tomlEOL(string(raw))

	if wanted == nil || !wanted.Enabled {
		if table == nil {
			return &ServiceOutcome{State: "disabled"}
		}
		// Only a table this module wrote may be removed.
		receipt, _ := c.readReceipt()
		owned := false
		if service == ServiceSearch {
			owned = receipt.Search != nil && receipt.Search.Applied
		} else {
			owned = receipt.Fetch != nil && receipt.Fetch.Applied
		}
		if !owned {
			return &ServiceOutcome{State: NativeStateConflict, Reason: "foreign_service"}
		}
		lines = append(lines[:table.headerAt], lines[table.endAt+1:]...)
		if failure := c.commitServices(string(raw), joinTOML(lines)); failure != "" {
			return &ServiceOutcome{State: NativeStateConflict, Reason: failure}
		}
		c.setReceipt(service, nil)
		return &ServiceOutcome{State: "disabled"}
	}

	desiredBase := wanted.BaseURL
	desiredKey := wanted.APIKey
	if table == nil {
		if len(lines) > 0 && lines[len(lines)-1].eol == "" {
			lines[len(lines)-1].eol = eol
		}
		lines = append(lines,
			tomlLine{text: "[services." + tableName + "]", eol: eol},
			tomlLine{text: "base_url = " + tomlString(desiredBase), eol: eol},
			tomlLine{text: "api_key = " + tomlString(desiredKey), eol: eol})
	} else {
		// An existing table that this module did not write is never edited: a
		// different endpoint there belongs to the user or another tool.
		receipt, _ := c.readReceipt()
		owned := false
		if service == ServiceSearch {
			owned = receipt.Search != nil && receipt.Search.Applied
		} else {
			owned = receipt.Fetch != nil && receipt.Fetch.Applied
		}
		if !owned && (table.hasBase || table.hasKey) {
			if !table.hasBase || NormalizeServiceURL(table.baseURL) != desiredBase {
				return &ServiceOutcome{State: NativeStateConflict, Reason: "foreign_service"}
			}
		}
		if table.hasBase {
			lines[table.baseLine] = tomlLine{text: "base_url = " + tomlString(desiredBase), eol: scan.lines[table.baseLine].eol}
		} else {
			lines = insertTOML(lines, table.headerAt+1, tomlLine{text: "base_url = " + tomlString(desiredBase), eol: eol})
			table.keyLine = bumpAfter(table.keyLine, table.headerAt+1)
		}
		if table.hasKey {
			lines[table.keyLine] = tomlLine{text: "api_key = " + tomlString(desiredKey), eol: scan.lines[table.keyLine].eol}
		} else {
			lines = insertTOML(lines, table.headerAt+2, tomlLine{text: "api_key = " + tomlString(desiredKey), eol: eol})
		}
	}
	if failure := c.commitServices(string(raw), joinTOML(lines)); failure != "" {
		return &ServiceOutcome{State: NativeStateConflict, Reason: failure}
	}
	c.setReceipt(service, &serviceReceiptEntry{Applied: true, BaseURL: desiredBase,
		KeyHash: digestHex(desiredKey), At: c.now().UnixMilli()})
	return &ServiceOutcome{State: NativeStateFileApplied, BaseURL: desiredBase, Applied: true}
}

func bumpAfter(line, insertedAt int) int {
	if line >= insertedAt {
		return line + 1
	}
	return line
}

func insertTOML(lines []tomlLine, at int, line tomlLine) []tomlLine {
	if at > len(lines) {
		at = len(lines)
	}
	out := make([]tomlLine, 0, len(lines)+1)
	out = append(out, lines[:at]...)
	out = append(out, line)
	return append(out, lines[at:]...)
}

func (c *ServicesConfig) setReceipt(service string, entry *serviceReceiptEntry) {
	receipt, _ := c.readReceipt()
	receipt.SchemaVersion = 1
	if service == ServiceSearch {
		receipt.Search = entry
	} else {
		receipt.Fetch = entry
	}
	_ = c.writeJSON(c.receipt, receipt)
}

func (c *ServicesConfig) commitServices(previous, next string) string {
	current, err := os.ReadFile(c.paths.configPath)
	if err != nil {
		if isNotExist(err) {
			return "config_missing"
		}
		return "config_unavailable"
	}
	if string(current) != previous {
		return "concurrent_modification"
	}
	mode := os.FileMode(0o600)
	if info, err := lstatPath(c.paths.configPath); err == nil {
		mode = info.mode
	}
	if next == previous {
		return ""
	}
	if err := writePrivateFile(c.paths.configPath, []byte(next), mode); err != nil {
		return "write_failed"
	}
	return ""
}

// Intent is the saved request, for a panel that must show what was asked for.
func (c *ServicesConfig) Intent() ServicesInput {
	intent, _ := c.readIntent()
	out := ServicesInput{}
	if intent.Search != nil {
		out.Search = &ServiceSettings{Enabled: intent.Search.Enabled, BaseURL: intent.Search.BaseURL,
			APIKey: intent.Search.APIKey}
	}
	if intent.Fetch != nil {
		out.Fetch = &ServiceSettings{Enabled: intent.Fetch.Enabled, BaseURL: intent.Fetch.BaseURL,
			APIKey: intent.Fetch.APIKey}
	}
	return out
}

var _ = strconv.Itoa
