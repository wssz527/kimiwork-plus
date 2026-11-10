// Package ledger is a durable, redacted account of the token usage this process
// actually saw.
//
// Three rules shape it:
//
//   - Only evidence is stored. A count the upstream did not report is null in
//     the record and in the aggregate — never a zero, because a zero-token
//     request and a request whose usage was never reported are different facts
//     and the user acts differently on them. Every published field is always
//     present, so a consumer never has to tell a missing key from an unknown
//     value.
//   - Nothing from the conversation is stored. Records are built from a
//     whitelist of fields, so a prompt, a body or a key cannot ride along even
//     if a caller passes one. Account and key identifiers are reduced to a
//     digest before they are written.
//   - Writing must never cost a reply. Every filesystem call is contained; a
//     damaged file, an unwritable directory or a failed rename leaves the
//     in-memory history intact and reports a diagnosable state instead of
//     returning an error into the request path.
package ledger

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SchemaVersion is the on-disk and query envelope version.
const SchemaVersion = 1

// statuses is the closed vocabulary a record may carry.
var statuses = []string{"started", "completed", "incomplete", "failed", "cancelled"}

var statusSet = map[string]bool{"started": true, "completed": true, "incomplete": true,
	"failed": true, "cancelled": true}

var tokenFields = []string{"inputTokens", "outputTokens", "totalTokens", "cacheReadTokens", "cacheWriteTokens"}

var identityFields = []string{"providerId", "connectionId", "modelId", "modelAlias", "account", "apiKeyId",
	"conversationId"}

var mergeFields = append(append([]string{}, tokenFields...), append(identityFields,
	"protocol", "upstreamProtocol", "latencyMs", "httpStatus", "errorCode", "priceSource", "estimatedCost")...)

// Published is the shape of one record, in the order the panel renders it.
var Published = []string{"providerId", "connectionId", "modelId", "modelAlias", "account", "apiKeyId",
	"conversationId", "protocol", "upstreamProtocol", "inputTokens", "outputTokens", "totalTokens",
	"cacheReadTokens", "cacheWriteTokens", "latencyMs", "httpStatus", "errorCode", "priceSource", "estimatedCost"}

const (
	maxBuckets    = 5000
	maxIdentifier = 128
	dayMillis     = 86400000
)

// Errors a caller can act on.
var (
	ErrInvalidLedgerFile    = errors.New("invalid_ledger_file")
	ErrInvalidLedgerOptions = errors.New("invalid_ledger_options")
	ErrInvalidEntry         = errors.New("invalid_entry")
	ErrInvalidOperationID   = errors.New("invalid_operation_id")
	ErrInvalidSource        = errors.New("invalid_source")
	ErrInvalidAt            = errors.New("invalid_at")
	ErrInvalidStatus        = errors.New("invalid_status")
	ErrInvalidTimeZone      = errors.New("invalid_time_zone")
	ErrInvalidRange         = errors.New("invalid_range")
	ErrInvalidRangePreset   = errors.New("invalid_range_preset")
	ErrInvalidFilter        = errors.New("invalid_filter")
	ErrInvalidLimit         = errors.New("invalid_limit")
	ErrInvalidNow           = errors.New("invalid_now")
	ErrRangeTooLarge        = errors.New("range_too_large")
)

// Options configures one ledger. Retention is bounded in both directions so a
// long-running gateway cannot grow the file without limit.
type Options struct {
	File         string
	MaxRecords   int
	MaxAgeDays   int
	CompactEvery int
	MaxFileBytes int64
	Now          func() int64
}

func defaultOptions(options Options) (Options, error) {
	if options.File == "" {
		return options, ErrInvalidLedgerFile
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = 20000
	}
	if options.MaxAgeDays == 0 {
		options.MaxAgeDays = 90
	}
	if options.CompactEvery == 0 {
		options.CompactEvery = 128
	}
	if options.MaxFileBytes == 0 {
		options.MaxFileBytes = 16 * 1024 * 1024
	}
	if options.MaxRecords < 1 || options.MaxAgeDays < 1 || options.CompactEvery < 1 || options.MaxFileBytes < 1 {
		return options, ErrInvalidLedgerOptions
	}
	if options.Now == nil {
		options.Now = func() int64 { return time.Now().UnixMilli() }
	}
	absolute, err := filepath.Abs(options.File)
	if err != nil {
		return options, ErrInvalidLedgerFile
	}
	options.File = absolute
	return options, nil
}

// Record is one published request. Fields are pointers so "unknown" is a null
// and never a zero.
type Record struct {
	OperationID      string   `json:"operationId"`
	Source           string   `json:"source"`
	At               string   `json:"at"`
	Status           string   `json:"status"`
	DataSource       string   `json:"dataSource"`
	ProviderID       *string  `json:"providerId"`
	ConnectionID     *string  `json:"connectionId"`
	ModelID          *string  `json:"modelId"`
	ModelAlias       *string  `json:"modelAlias"`
	Account          *string  `json:"account"`
	APIKeyID         *string  `json:"apiKeyId"`
	ConversationID   *string  `json:"conversationId"`
	Protocol         *string  `json:"protocol"`
	UpstreamProtocol *string  `json:"upstreamProtocol"`
	InputTokens      *int64   `json:"inputTokens"`
	OutputTokens     *int64   `json:"outputTokens"`
	TotalTokens      *int64   `json:"totalTokens"`
	CacheReadTokens  *int64   `json:"cacheReadTokens"`
	CacheWriteTokens *int64   `json:"cacheWriteTokens"`
	LatencyMS        *int64   `json:"latencyMs"`
	HTTPStatus       *int64   `json:"httpStatus"`
	ErrorCode        *string  `json:"errorCode"`
	PriceSource      *string  `json:"priceSource"`
	EstimatedCost    *float64 `json:"estimatedCost"`
}

func (r *Record) clone() *Record {
	copied := *r
	return &copied
}

func (r *Record) field(name string) any {
	switch name {
	case "providerId":
		return r.ProviderID
	case "connectionId":
		return r.ConnectionID
	case "modelId":
		return r.ModelID
	case "modelAlias":
		return r.ModelAlias
	case "account":
		return r.Account
	case "apiKeyId":
		return r.APIKeyID
	case "conversationId":
		return r.ConversationID
	case "protocol":
		return r.Protocol
	case "upstreamProtocol":
		return r.UpstreamProtocol
	case "inputTokens":
		return r.InputTokens
	case "outputTokens":
		return r.OutputTokens
	case "totalTokens":
		return r.TotalTokens
	case "cacheReadTokens":
		return r.CacheReadTokens
	case "cacheWriteTokens":
		return r.CacheWriteTokens
	case "latencyMs":
		return r.LatencyMS
	case "httpStatus":
		return r.HTTPStatus
	case "errorCode":
		return r.ErrorCode
	case "priceSource":
		return r.PriceSource
	case "estimatedCost":
		return r.EstimatedCost
	}
	return nil
}

func (r *Record) setField(name string, value any) {
	switch name {
	case "providerId":
		r.ProviderID, _ = value.(*string)
	case "connectionId":
		r.ConnectionID, _ = value.(*string)
	case "modelId":
		r.ModelID, _ = value.(*string)
	case "modelAlias":
		r.ModelAlias, _ = value.(*string)
	case "account":
		r.Account, _ = value.(*string)
	case "apiKeyId":
		r.APIKeyID, _ = value.(*string)
	case "conversationId":
		r.ConversationID, _ = value.(*string)
	case "protocol":
		r.Protocol, _ = value.(*string)
	case "upstreamProtocol":
		r.UpstreamProtocol, _ = value.(*string)
	case "inputTokens":
		r.InputTokens, _ = value.(*int64)
	case "outputTokens":
		r.OutputTokens, _ = value.(*int64)
	case "totalTokens":
		r.TotalTokens, _ = value.(*int64)
	case "cacheReadTokens":
		r.CacheReadTokens, _ = value.(*int64)
	case "cacheWriteTokens":
		r.CacheWriteTokens, _ = value.(*int64)
	case "latencyMs":
		r.LatencyMS, _ = value.(*int64)
	case "httpStatus":
		r.HTTPStatus, _ = value.(*int64)
	case "errorCode":
		r.ErrorCode, _ = value.(*string)
	case "priceSource":
		r.PriceSource, _ = value.(*string)
	case "estimatedCost":
		r.EstimatedCost, _ = value.(*float64)
	}
}

// Public renders the published shape: every field present, unknown as null.
// Pointer fields are dereferenced here so the map is ready to be serialised —
// a caller handing this to encoding/json would otherwise get a pointer's
// address, which is neither the value nor a null.
func (r *Record) Public() map[string]any {
	out := map[string]any{"operationId": r.OperationID, "source": r.Source, "at": r.At,
		"status": r.Status, "dataSource": r.DataSource}
	for _, field := range Published {
		out[field] = derefValue(r.field(field))
	}
	return out
}

// derefValue turns a typed pointer into the value or nil.
func derefValue(value any) any {
	switch item := value.(type) {
	case *string:
		if item == nil {
			return nil
		}
		return *item
	case *int64:
		if item == nil {
			return nil
		}
		return *item
	case *float64:
		if item == nil {
			return nil
		}
		return *item
	}
	return value
}

// Entry is what a caller hands over. It is deliberately a separate type: a
// caller cannot smuggle an unpublished field into a record.
type Entry struct {
	OperationID      string   `json:"operationId"`
	Source           string   `json:"source"`
	At               any      `json:"at"`
	Status           string   `json:"status"`
	ProviderID       *string  `json:"providerId"`
	ConnectionID     *string  `json:"connectionId"`
	ModelID          *string  `json:"modelId"`
	ModelAlias       *string  `json:"modelAlias"`
	Account          *string  `json:"account"`
	APIKeyID         *string  `json:"apiKeyId"`
	ConversationID   *string  `json:"conversationId"`
	Protocol         *string  `json:"protocol"`
	UpstreamProtocol *string  `json:"upstreamProtocol"`
	InputTokens      *int64   `json:"inputTokens"`
	OutputTokens     *int64   `json:"outputTokens"`
	TotalTokens      *int64   `json:"totalTokens"`
	CacheReadTokens  *int64   `json:"cacheReadTokens"`
	CacheWriteTokens *int64   `json:"cacheWriteTokens"`
	LatencyMS        *int64   `json:"latencyMs"`
	HTTPStatus       *int64   `json:"httpStatus"`
	ErrorCode        *string  `json:"errorCode"`
	PriceSource      *string  `json:"priceSource"`
	EstimatedCost    *float64 `json:"estimatedCost"`
	// Usage is the upstream envelope, normalised only when the caller did not
	// hand over separated counts.
	Usage         map[string]any `json:"usage"`
	UsageProtocol string         `json:"usageProtocol"`
}

// Result reports what one write did.
type Result struct {
	OK        bool           `json:"ok"`
	Reason    string         `json:"reason,omitempty"`
	Key       string         `json:"key,omitempty"`
	Duplicate bool           `json:"duplicate"`
	Changed   bool           `json:"changed"`
	Record    map[string]any `json:"record,omitempty"`
}

// Status is the diagnostic view of the ledger file.
type Status struct {
	SchemaVersion int            `json:"schemaVersion"`
	State         string         `json:"state"`
	File          string         `json:"file"`
	Records       int            `json:"records"`
	LastWriteAt   *string        `json:"lastWriteAt"`
	SkippedLines  int            `json:"skippedLines"`
	Retention     map[string]int `json:"retention"`
}

// Ledger is one durable history.
type Ledger struct {
	mu           sync.Mutex
	file         string
	maxRecords   int
	maxAgeDays   int
	compactEvery int
	maxFileBytes int64
	now          func() int64
	records      []*Record
	index        map[string]*Record
	appended     int
	bytes        int64
	skippedLines int
	readFailed   bool
	writeIssue   string
	lastWriteAt  *string
}

// New loads the history from disk. A missing file is the normal first run, not
// a failure.
func New(options Options) (*Ledger, error) {
	resolved, err := defaultOptions(options)
	if err != nil {
		return nil, err
	}
	ledger := &Ledger{file: resolved.File, maxRecords: resolved.MaxRecords, maxAgeDays: resolved.MaxAgeDays,
		compactEvery: resolved.CompactEvery, maxFileBytes: resolved.MaxFileBytes, now: resolved.Now,
		index: map[string]*Record{}}
	ledger.load()
	return ledger, nil
}

// State reports the health of the file in one word.
func (l *Ledger) State() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked()
}

func (l *Ledger) stateLocked() string {
	switch {
	case l.writeIssue == "unsafe_file":
		return "unsafe_file"
	case l.writeIssue != "":
		return "write_failed"
	case l.readFailed:
		return "unavailable"
	case l.skippedLines > 0:
		return "corrupt"
	case len(l.records) > 0:
		return "ok"
	default:
		return "empty"
	}
}

// Status is the diagnostic view.
func (l *Ledger) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	var last *string
	if l.lastWriteAt != nil {
		copied := *l.lastWriteAt
		last = &copied
	}
	return Status{SchemaVersion: SchemaVersion, State: l.stateLocked(), File: l.file,
		Records: len(l.records), LastWriteAt: last, SkippedLines: l.skippedLines,
		Retention: map[string]int{"maxRecords": l.maxRecords, "maxAgeDays": l.maxAgeDays}}
}

// Records returns a copy of the history, oldest first.
func (l *Ledger) Records() []*Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Record, len(l.records))
	for i, record := range l.records {
		out[i] = record.clone()
	}
	return out
}

// --- persistence -----------------------------------------------------------

func (l *Ledger) load() {
	stat, err := os.Lstat(l.file)
	if err != nil {
		if !os.IsNotExist(err) {
			l.readFailed = true
		}
		return
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		l.readFailed = true
		return
	}
	// A file far larger than this ledger ever writes is not this ledger's
	// document. Reading it to find that out would be the damage.
	if stat.Size() > l.maxFileBytes*4 {
		l.readFailed = true
		return
	}
	raw, err := os.ReadFile(l.file)
	if err != nil {
		l.readFailed = true
		return
	}
	l.bytes = int64(len(raw))
	skipped := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), int(l.maxFileBytes))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// Loaded lines go through the same whitelist as live ones: a
		// hand-edited or injected line cannot introduce a field this ledger
		// does not keep.
		record, err := l.normalizeJSON(line)
		if err != nil {
			skipped++
			continue
		}
		l.insert(record)
	}
	l.skippedLines = skipped
	// A damaged line is dropped from the file as soon as it is recognised, so a
	// later append cannot glue a fresh record onto a truncated tail.
	if l.prune() > 0 || skipped > 0 {
		l.compact()
	}
}

func (l *Ledger) normalizeJSON(raw []byte) (*Record, error) {
	var entry Entry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		// Unknown fields are expected in a file written by a newer build; they
		// are ignored rather than rejecting the whole line.
		entry = Entry{}
		if json.Unmarshal(raw, &entry) != nil {
			return nil, ErrInvalidEntry
		}
	}
	return l.normalize(entry)
}

// Normalize turns one entry into a record, applying the whitelist and the
// redaction. It never returns a partially-filled record.
func (l *Ledger) Normalize(entry Entry) (map[string]any, error) {
	record, err := l.normalize(entry)
	if err != nil {
		return nil, err
	}
	return record.Public(), nil
}

func (l *Ledger) normalize(entry Entry) (*Record, error) {
	operationID := identifier(entry.OperationID)
	if operationID == "" {
		return nil, ErrInvalidOperationID
	}
	source := entry.Source
	if source == "" {
		source = "gateway"
	}
	if !sourcePattern.MatchString(source) {
		return nil, ErrInvalidSource
	}
	at := entry.At
	if at == nil {
		at = l.now()
	}
	atMS, ok := parseInstant(at)
	if !ok || atMS < 0 {
		return nil, ErrInvalidAt
	}
	status := entry.Status
	if status == "" {
		status = "completed"
	}
	if !statusSet[status] {
		return nil, ErrInvalidStatus
	}
	record := &Record{OperationID: operationID, Source: source,
		At: time.UnixMilli(atMS).UTC().Format("2006-01-02T15:04:05.000Z"), Status: status}
	assign := func(target **string, raw *string, redact bool) {
		if raw == nil {
			return
		}
		value := identifier(*raw)
		if value == "" {
			return
		}
		if redact && !digestPattern.MatchString(value) {
			value = RedactIdentifier(value)
			if value == "" {
				return
			}
		}
		copied := value
		*target = &copied
	}
	assign(&record.ProviderID, entry.ProviderID, false)
	assign(&record.ConnectionID, entry.ConnectionID, false)
	assign(&record.ModelID, entry.ModelID, false)
	assign(&record.ModelAlias, entry.ModelAlias, false)
	assign(&record.Account, entry.Account, true)
	assign(&record.APIKeyID, entry.APIKeyID, true)
	assign(&record.ConversationID, entry.ConversationID, false)
	if entry.Protocol != nil && (*entry.Protocol == "chat" || *entry.Protocol == "responses") {
		value := *entry.Protocol
		record.Protocol = &value
	}
	if entry.UpstreamProtocol != nil && (*entry.UpstreamProtocol == "anthropic" || *entry.UpstreamProtocol == "openai") {
		value := *entry.UpstreamProtocol
		record.UpstreamProtocol = &value
	}

	derived := normalizeUsage(entry.Usage, entry.UsageProtocol)
	// An explicit count wins over a derived one.
	pick := func(explicit *int64, derived *int64) *int64 {
		if explicit != nil {
			value := *explicit
			if value < 0 {
				return nil
			}
			return &value
		}
		if derived != nil {
			value := *derived
			return &value
		}
		return nil
	}
	record.InputTokens = pick(entry.InputTokens, derived.input)
	record.OutputTokens = pick(entry.OutputTokens, derived.output)
	record.TotalTokens = pick(entry.TotalTokens, derived.total)
	record.CacheReadTokens = pick(entry.CacheReadTokens, derived.cacheRead)
	record.CacheWriteTokens = pick(entry.CacheWriteTokens, derived.cacheWrite)
	if entry.LatencyMS != nil && *entry.LatencyMS >= 0 {
		value := *entry.LatencyMS
		record.LatencyMS = &value
	}
	if entry.HTTPStatus != nil && *entry.HTTPStatus >= 100 && *entry.HTTPStatus <= 599 {
		value := *entry.HTTPStatus
		record.HTTPStatus = &value
	}
	if entry.ErrorCode != nil && errorCodePattern.MatchString(*entry.ErrorCode) {
		value := *entry.ErrorCode
		record.ErrorCode = &value
	}
	if entry.PriceSource != nil {
		if value := identifier(*entry.PriceSource); value != "" {
			record.PriceSource = &value
		}
	}
	if entry.EstimatedCost != nil && *entry.EstimatedCost >= 0 {
		value := *entry.EstimatedCost
		record.EstimatedCost = &value
	}
	// Where the numbers came from is derived from the numbers themselves: a
	// record with no token count is a counted request, not a zero-token one.
	record.DataSource = "request_only"
	for _, field := range tokenFields {
		if !isNilPointer(record.field(field)) {
			record.DataSource = "upstream_usage"
			break
		}
	}
	return record, nil
}

func parseInstant(value any) (int64, bool) {
	switch item := value.(type) {
	case int64:
		return item, true
	case int:
		return int64(item), true
	case float64:
		if item != float64(int64(item)) {
			return 0, false
		}
		return int64(item), true
	case json.Number:
		parsed, err := item.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if parsed, err := time.Parse(layout, item); err == nil {
				return parsed.UnixMilli(), true
			}
		}
		return 0, false
	}
	return 0, false
}

func keyOf(record *Record) string { return record.Source + ":" + record.OperationID }

func (l *Ledger) insert(record *Record) *Record {
	key := keyOf(record)
	if existing, ok := l.index[key]; ok {
		mergeEntry(existing, record)
		return existing
	}
	at := record.atMillis()
	// Records are kept in time order so a retention cut is a prefix trim.
	position := sort.Search(len(l.records), func(i int) bool { return l.records[i].atMillis() > at })
	l.records = append(l.records, nil)
	copy(l.records[position+1:], l.records[position:])
	l.records[position] = record
	l.index[key] = record
	return record
}

func (r *Record) atMillis() int64 {
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", r.At)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// mergeEntry fills what is still unknown and never adds. A terminal status
// replaces `started`; between two terminal deliveries the first wins, because a
// duplicate receipt cannot retroactively make a completed request failed.
func mergeEntry(into, from *Record) bool {
	changed := false
	for _, field := range mergeFields {
		current := into.field(field)
		if current != nil && !isNilPointer(current) {
			continue
		}
		incoming := from.field(field)
		if incoming == nil || isNilPointer(incoming) {
			continue
		}
		into.setField(field, incoming)
		changed = true
	}
	if into.Status == "started" && from.Status != "started" {
		into.Status = from.Status
		changed = true
	}
	if into.DataSource == "request_only" && from.DataSource == "upstream_usage" {
		into.DataSource = "upstream_usage"
		changed = true
	}
	return changed
}

func isNilPointer(value any) bool {
	switch item := value.(type) {
	case *string:
		return item == nil
	case *int64:
		return item == nil
	case *float64:
		return item == nil
	}
	return value == nil
}

func (l *Ledger) prune() int {
	cutoff := l.now() - int64(l.maxAgeDays)*dayMillis
	removed := 0
	for len(l.records) > 0 && l.records[0].atMillis() < cutoff {
		delete(l.index, keyOf(l.records[0]))
		l.records = l.records[1:]
		removed++
	}
	for len(l.records) > l.maxRecords {
		delete(l.index, keyOf(l.records[0]))
		l.records = l.records[1:]
		removed++
	}
	return removed
}

func (l *Ledger) guard() error {
	directory := filepath.Dir(l.file)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("unsafe_ledger_file")
	}
	directoryStat, err := os.Lstat(directory)
	if err != nil {
		return errors.New("unsafe_ledger_file")
	}
	// A directory that is a link could be repointed at another user's tree
	// between two writes, taking the history with it.
	if directoryStat.Mode()&os.ModeSymlink != 0 || !directoryStat.IsDir() {
		return errors.New("unsafe_ledger_file")
	}
	stat, err := os.Lstat(l.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// The ledger holds a user's history: it must be a private regular file, not
	// a link someone else can retarget.
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() {
		return errors.New("unsafe_ledger_file")
	}
	return nil
}

func (l *Ledger) append(record *Record) {
	if err := l.guard(); err != nil {
		l.writeIssue = "unsafe_file"
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		l.writeIssue = "write_failed"
		return
	}
	line = append(line, '\n')
	file, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.writeIssue = "write_failed"
		return
	}
	_, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		l.writeIssue = "write_failed"
		return
	}
	l.bytes += int64(len(line))
	l.appended++
	stamp := time.UnixMilli(l.now()).UTC().Format("2006-01-02T15:04:05.000Z")
	l.lastWriteAt = &stamp
	l.writeIssue = ""
	if l.appended >= l.compactEvery || l.bytes >= l.maxFileBytes {
		l.compact()
	}
}

// compact rewrites the retained history atomically: a reader never sees a
// half-written file, and a failed write leaves the previous one in place.
func (l *Ledger) compact() {
	if err := l.guard(); err != nil {
		l.writeIssue = "unsafe_file"
		return
	}
	var body bytes.Buffer
	for _, record := range l.records {
		line, err := json.Marshal(record)
		if err != nil {
			l.writeIssue = "write_failed"
			return
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	temporary := l.file + "." + randomToken() + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		l.writeIssue = "write_failed"
		return
	}
	_, writeErr := file.Write(body.Bytes())
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		l.writeIssue = "write_failed"
		return
	}
	if err := os.Rename(temporary, l.file); err != nil {
		_ = os.Remove(temporary)
		l.writeIssue = "write_failed"
		return
	}
	l.appended = 0
	l.bytes = int64(body.Len())
	stamp := time.UnixMilli(l.now()).UTC().Format("2006-01-02T15:04:05.000Z")
	l.lastWriteAt = &stamp
	l.writeIssue = ""
}

func randomToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(buf[:])
}

// --- writing ---------------------------------------------------------------

// Record stores one receipt. It never returns an error into the request path: a
// caller in the hot path gets an answer about what happened, and a storage
// problem is reported through State instead.
func (l *Ledger) Record(entry Entry) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, err := l.normalize(entry)
	if err != nil {
		return Result{OK: false, Reason: err.Error()}
	}
	key := keyOf(record)
	if existing, ok := l.index[key]; ok {
		if !mergeEntry(existing, record) {
			return Result{OK: true, Key: key, Duplicate: true, Changed: false, Record: existing.Public()}
		}
		l.append(existing)
		return Result{OK: true, Key: key, Duplicate: true, Changed: true, Record: existing.Public()}
	}
	stored := l.insert(record)
	l.prune()
	// A record outside the retention window is already gone from memory; it
	// must not be appended back to the file.
	if l.index[key] == stored {
		l.append(stored)
	}
	return Result{OK: true, Key: key, Duplicate: false, Changed: true, Record: stored.Public()}
}

// ClearResult reports what a clear removed.
type ClearResult struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Removed int    `json:"removed"`
	State   string `json:"state"`
}

// Clear removes history. A clear is explicit and must never remove more than
// asked, so an unparseable boundary is refused.
func (l *Ledger) Clear(before any, timeZone string) ClearResult {
	zone, err := ResolveTimeZone(timeZone)
	if err != nil {
		return ClearResult{OK: false, Reason: "invalid_time_zone"}
	}
	var cutoff *int64
	if before != nil {
		value, ok := BoundaryMillis(before, zone, "start")
		if !ok {
			return ClearResult{OK: false, Reason: "invalid_before"}
		}
		cutoff = &value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := make([]*Record, 0, len(l.records))
	index := make(map[string]*Record, len(l.records))
	for _, record := range l.records {
		// No boundary means "everything", which is what an explicit clear with
		// no `before` asks for.
		if cutoff == nil || record.atMillis() < *cutoff {
			continue
		}
		kept = append(kept, record)
		index[keyOf(record)] = record
	}
	removed := len(l.records) - len(kept)
	l.records = kept
	l.index = index
	l.compact()
	return ClearResult{OK: l.writeIssue == "", Removed: removed, State: l.stateLocked()}
}

// --- identity --------------------------------------------------------------

// RedactIdentifier turns a credential into a stable, non-reversible group key.
// The raw value never leaves this function.
func RedactIdentifier(value string) string {
	if value == "" || len(value) > 4096 {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "id_" + hex.EncodeToString(sum[:])[:12]
}

func identifier(value string) string {
	if len(value) > maxIdentifier || !identifierPattern.MatchString(value) {
		return ""
	}
	return value
}
