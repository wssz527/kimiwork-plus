package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The bounds the legacy scanner used. They exist for different reasons and must
// not be unified:
//
//   - A JSON reply is one document, so a total byte budget is the only bound
//     that makes sense: past it the metric is dropped, not the reply.
//   - An SSE stream is a sequence of independent events whose terminal usage
//     arrives last. Bounding the whole stream would lose the usage of any long
//     but ordinary answer, so SSE is bounded per event: a runaway single line is
//     skipped up to its newline and scanning resumes with the next event.
const (
	UsageScanLimit = 256 * 1024
	UsageLineLimit = 64 * 1024
)

// Usage is the token evidence one upstream reply reported. Every field is a
// pointer so "the upstream did not say" stays distinguishable from a real zero.
type Usage struct {
	InputTokens       *int64 `json:"inputTokens,omitempty"`
	OutputTokens      *int64 `json:"outputTokens,omitempty"`
	TotalTokens       *int64 `json:"totalTokens,omitempty"`
	CachedInputTokens *int64 `json:"cachedInputTokens,omitempty"`
	CacheWriteTokens  *int64 `json:"cacheWriteTokens,omitempty"`
}

// Outcome is the terminal state a stream announced about itself.
type Outcome string

const (
	OutcomeCompleted  Outcome = "completed"
	OutcomeIncomplete Outcome = "incomplete"
	OutcomeFailed     Outcome = "failed"
)

// ScanKind selects how a body is read for usage.
type ScanKind int

const (
	ScanNone ScanKind = iota
	ScanSSE
	ScanJSON
)

// ScannerFor picks a scanner from the upstream Content-Type.
func ScannerFor(contentType string) ScanKind {
	value := strings.ToLower(contentType)
	switch {
	case strings.Contains(value, "text/event-stream"):
		return ScanSSE
	case strings.Contains(value, "json"):
		return ScanJSON
	default:
		return ScanNone
	}
}

// UsageScanner reads usage out of a reply without buffering the whole body and
// without altering a single forwarded byte.
type UsageScanner struct {
	kind      ScanKind
	remainder string
	body      []byte
	seen      int
	skipping  bool
	usage     *Usage
	outcome   Outcome
	// partial holds an incomplete UTF-8 tail so a multi-byte character split
	// across two chunks is decoded once, not twice.
	partial []byte
}

// NewUsageScanner returns a scanner for one reply body.
func NewUsageScanner(kind ScanKind) *UsageScanner {
	return &UsageScanner{kind: kind}
}

// Push feeds one chunk of the reply.
func (s *UsageScanner) Push(chunk []byte) {
	if s == nil {
		return
	}
	if s.kind == ScanNone || (s.kind == ScanJSON && s.seen > UsageScanLimit) {
		return
	}
	value := s.decode(chunk)
	if s.kind == ScanJSON {
		s.seen += len(chunk)
		if s.seen > UsageScanLimit {
			s.body = nil
			return
		}
		s.body = append(s.body, value...)
		return
	}
	s.lines(s.remainder + string(value))
}

func (s *UsageScanner) decode(chunk []byte) []byte {
	buf := append(s.partial, chunk...)
	s.partial = nil
	if utf8.Valid(buf) {
		return buf
	}
	// Keep at most one incomplete trailing rune for the next chunk.
	cut := len(buf)
	for i := 0; i < utf8.UTFMax && i < len(buf); i++ {
		if utf8.RuneStart(buf[len(buf)-1-i]) {
			if !utf8.FullRune(buf[len(buf)-1-i:]) {
				cut = len(buf) - 1 - i
			}
			break
		}
	}
	s.partial = append([]byte(nil), buf[cut:]...)
	return buf[:cut]
}

func (s *UsageScanner) lines(value string) {
	parts := strings.Split(value, "\n")
	s.remainder = parts[len(parts)-1]
	for _, part := range parts[:len(parts)-1] {
		if s.skipping {
			s.skipping = false
			continue
		}
		s.line(part)
	}
	if len(s.remainder) > UsageLineLimit {
		s.remainder = ""
		s.skipping = true
	}
}

func (s *UsageScanner) line(raw string) {
	line := strings.TrimSpace(raw)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(line[5:])
	if data == "" {
		return
	}
	if data == "[DONE]" {
		if s.outcome == "" {
			s.outcome = OutcomeCompleted
		}
		return
	}
	if len(data) > UsageLineLimit {
		return
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &payload) != nil {
		return
	}
	if usage := usageFromJSON(payload); usage != nil {
		s.usage = usage
	}
	s.classify(payload)
}

// classify records what the event said about the request's outcome. A failure
// can never be overwritten by a later completion: the stream already broke.
func (s *UsageScanner) classify(payload map[string]json.RawMessage) {
	if s.outcome == OutcomeFailed {
		return
	}
	if raw, ok := payload["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		s.outcome = OutcomeFailed
		return
	}
	if typ, _ := rawString(payload["type"]); typ == "response.failed" {
		s.outcome = OutcomeFailed
		return
	}
	if response, ok := rawObject(payload["response"]); ok {
		if status, _ := rawString(response["status"]); status == "failed" {
			s.outcome = OutcomeFailed
			return
		}
	}
	if typ, _ := rawString(payload["type"]); typ == "response.incomplete" {
		s.outcome = OutcomeIncomplete
		return
	}
	if response, ok := rawObject(payload["response"]); ok {
		if status, _ := rawString(response["status"]); status == "incomplete" {
			s.outcome = OutcomeIncomplete
			return
		}
	}
	if typ, _ := rawString(payload["type"]); typ == "response.completed" {
		s.outcome = OutcomeCompleted
		return
	}
	if choices, ok := payload["choices"]; ok {
		var rows []map[string]json.RawMessage
		if json.Unmarshal(choices, &rows) == nil {
			finished := false
			length := false
			for _, row := range rows {
				reason, present := rawString(row["finish_reason"])
				if !present || reason == "" {
					continue
				}
				finished = true
				if reason == "length" {
					length = true
				}
			}
			if finished {
				if length {
					s.outcome = OutcomeIncomplete
				} else {
					s.outcome = OutcomeCompleted
				}
			}
		}
	}
}

// Finish returns the usage the reply reported, or nil when it reported none.
func (s *UsageScanner) Finish() *Usage {
	if s == nil {
		return nil
	}
	tail := append(append([]byte(nil), s.partial...), s.decode(nil)...)
	s.partial = nil
	switch s.kind {
	case ScanJSON:
		if s.seen <= UsageScanLimit && len(tail) > 0 {
			s.body = append(s.body, tail...)
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(s.body, &payload) == nil {
			if usage := usageFromJSON(payload); usage != nil {
				s.usage = usage
			}
		}
	case ScanSSE:
		value := s.remainder + string(tail)
		if value != "" && !s.skipping {
			s.line(value)
		}
	}
	s.body = nil
	s.remainder = ""
	s.skipping = false
	return s.usage
}

// Outcome reports the terminal state the stream announced, or "" when it never
// said. A missing terminal state is not a completion.
func (s *UsageScanner) Outcome() Outcome {
	if s == nil {
		return ""
	}
	return s.outcome
}

func intPtr(value int64) *int64 { return &value }

// countOf mirrors `Number.isSafeInteger(value) && value >= 0`.
func countOf(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil
	}
	number, ok := v.(json.Number)
	if !ok {
		return nil
	}
	f, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < 0 || f > maxSafeInt {
		return nil
	}
	return intPtr(int64(f))
}

// cacheCreated reads Anthropic's cache creation, which is either the flat
// counter or a nested per-lifetime breakdown. Only the parts that are really
// there are summed, so a missing breakdown stays unknown instead of zero.
func cacheCreated(value map[string]json.RawMessage) *int64 {
	if flat := countOf(value["cache_write_tokens"]); flat != nil {
		return flat
	}
	if flat := countOf(value["cache_creation_input_tokens"]); flat != nil {
		return flat
	}
	nested, ok := rawObject(value["cache_creation"])
	if !ok {
		return nil
	}
	var sum int64
	found := false
	for _, key := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
		if part := countOf(nested[key]); part != nil {
			sum += *part
			found = true
		}
	}
	if !found {
		return nil
	}
	return intPtr(sum)
}

func firstCount(values ...json.RawMessage) *int64 {
	for _, value := range values {
		if count := countOf(value); count != nil {
			return count
		}
	}
	return nil
}

func nestedCount(raw json.RawMessage, field string) *int64 {
	object, ok := rawObject(raw)
	if !ok {
		return nil
	}
	return countOf(object[field])
}

// usageOf reduces one upstream `usage` object to the counts the panel shows.
func usageOf(value map[string]json.RawMessage) *Usage {
	if value == nil {
		return nil
	}
	cached := nestedCount(value["prompt_tokens_details"], "cached_tokens")
	if cached == nil {
		cached = nestedCount(value["input_tokens_details"], "cached_tokens")
	}
	usage := &Usage{
		InputTokens:       firstCount(value["prompt_tokens"], value["input_tokens"]),
		OutputTokens:      firstCount(value["completion_tokens"], value["output_tokens"]),
		TotalTokens:       countOf(value["total_tokens"]),
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheCreated(value),
	}
	if usage.InputTokens == nil && usage.OutputTokens == nil && usage.TotalTokens == nil &&
		usage.CachedInputTokens == nil && usage.CacheWriteTokens == nil {
		return nil
	}
	return usage
}

// usageFromJSON reads the usage out of either envelope. A Responses stream only
// reports it on the terminal event, so the last one seen wins.
func usageFromJSON(payload map[string]json.RawMessage) *Usage {
	if payload == nil {
		return nil
	}
	if inner, ok := rawObject(payload["usage"]); ok {
		if usage := usageOf(inner); usage != nil {
			return usage
		}
	}
	if response, ok := rawObject(payload["response"]); ok {
		if inner, ok := rawObject(response["usage"]); ok {
			return usageOf(inner)
		}
	}
	return nil
}

// Context is the measured context consumption of one request: the complete
// input of the request just served, never an estimate of a running session.
type Context struct {
	UsedTokens  int64   `json:"usedTokens"`
	LimitTokens int64   `json:"limitTokens"`
	Ratio       float64 `json:"ratio"`
	Source      string  `json:"source"`
}

// ContextOf builds the context measurement, or nil when it cannot be honest.
func ContextOf(usage *Usage, limitTokens int64) *Context {
	if limitTokens <= 0 || usage == nil || usage.InputTokens == nil {
		return nil
	}
	return &Context{UsedTokens: *usage.InputTokens, LimitTokens: limitTokens,
		Ratio:  math.Round(float64(*usage.InputTokens)/float64(limitTokens)*1000) / 1000,
		Source: "upstream_usage"}
}

// Rate is the average output rate over the whole request, which is what a user
// actually waits through: it includes the time before the first byte.
type Rate struct {
	AverageOutputTokensPerSecond float64 `json:"averageOutputTokensPerSecond"`
	Basis                        string  `json:"basis"`
}

// RateOf computes the rate, or nil when there is no honest one to report.
func RateOf(status int, hasError bool, usage *Usage, ms int64) *Rate {
	if status < 200 || status >= 300 || hasError {
		return nil
	}
	if usage == nil || usage.OutputTokens == nil || ms <= 0 {
		return nil
	}
	value := float64(*usage.OutputTokens) / (float64(ms) / 1000)
	return &Rate{AverageOutputTokensPerSecond: math.Round(value*100) / 100, Basis: "total_request_time"}
}

var vendorTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// VendorType keeps a vendor error identifier only when it is a plain token;
// anything else could carry markup, a stack trace or an echoed prompt.
func VendorType(value string) string {
	if vendorTypePattern.MatchString(value) {
		return value
	}
	return ""
}

// ErrorTypeScan reads only the vendor's own error `type` out of a failed reply.
// The body is never stored: a body that happens to echo a prompt cannot leak
// through the request record.
type ErrorTypeScan struct {
	body []byte
	seen int
}

// Push feeds one chunk of a failed reply.
func (e *ErrorTypeScan) Push(chunk []byte) {
	if e == nil || e.seen > UsageScanLimit {
		return
	}
	e.seen += len(chunk)
	if e.seen > UsageScanLimit {
		e.body = nil
		return
	}
	e.body = append(e.body, chunk...)
}

// Finish returns the bounded vendor error type, or "".
func (e *ErrorTypeScan) Finish() string {
	if e == nil {
		return ""
	}
	body := e.body
	e.body = nil
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if inner, ok := rawObject(payload["error"]); ok {
		if typ, _ := rawString(inner["type"]); typ != "" {
			return VendorType(typ)
		}
	}
	if typ, _ := rawString(payload["type"]); typ != "" {
		return VendorType(typ)
	}
	if code, _ := rawString(payload["code"]); code != "" {
		return VendorType(code)
	}
	return ""
}

// UpstreamReasons maps a status to the reason a user can act on.
var UpstreamReasons = map[int]string{
	400: "upstream_bad_request", 401: "upstream_unauthorized", 403: "upstream_forbidden",
	404: "upstream_not_found", 409: "upstream_conflict", 413: "upstream_request_too_large",
	422: "upstream_unprocessable", 429: "upstream_rate_limited",
}

var machineReason = regexp.MustCompile(`^(?:[A-Za-z]*Error:\s*)?([A-Z][A-Z0-9_]{2,40})\b`)

// SafeReasonText reduces a transport failure message to something publishable:
// no URL, no host, no path, no credential and no request body can survive it.
func SafeReasonText(message string) string {
	first := strings.TrimSpace(strings.SplitN(message, "\n", 2)[0])
	if runes := []rune(first); len(runes) > 160 {
		first = string(runes[:160])
	}
	if match := machineReason.FindStringSubmatch(first); match != nil {
		return strings.ToLower(match[1])
	}
	if strings.EqualFold(first, "fetch failed") {
		return "fetch_failed"
	}
	if strings.HasPrefix(strings.ToLower(first), "socket") {
		return "socket"
	}
	return ""
}
