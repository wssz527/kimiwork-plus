// Package quota answers one question per account: "what is left, as of when,
// from which source?" It never answers "what is my total", because a
// subscription window, a Coding Plan window, a topped-up balance and a
// gateway's own self-set limit are different units on different clocks. No
// value here is summed across sources, and no missing value is written as 0.
//
// What this package deliberately does not do:
//
//   - It sends no inference request. Every adapter is a documented read-only
//     balance/usage endpoint.
//   - It reads no browser cookie, console session or agent sign-in store.
//     Credentials arrive from the caller through an in-memory resolver, are
//     used for exactly one request, and are never written to disk, never
//     returned from State and never put in an error message.
//   - It does not poll. State is pure. Refresh runs only when a person asked.
//
// A vendor without a documented read-only usage endpoint is reported as
// unsupported, with the official page a person can open instead. Kimi Code's
// subscription allowance is exactly that case.
package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kinds are the quota shapes this package understands.
var Kinds = []string{"subscription_window", "coding_plan", "api_balance", "gateway_limit"}

// Statuses are the answers a card can carry.
//
//	ok             — the last attempt succeeded and the reading is fresh
//	stale          — a reading is shown that is older than the freshness window
//	                 or from before a failed attempt; the numbers are real
//	error          — the last attempt failed and there is no earlier reading
//	not_read       — a credential is available but no read has happened yet
//	not_configured — no credential resolved, so no request was attempted
//	unsupported    — no documented read-only interface exists for this source
var Statuses = []string{"ok", "stale", "error", "not_read", "not_configured", "unsupported"}

// The documented read-only endpoints. A stored config can name an adapter and a
// credential, but never a different host for it, so editing the config file
// cannot repoint DeepSeek at an internal address.
const (
	DeepSeekEndpoint       = "https://api.deepseek.com/user/balance"
	MoonshotCNEndpoint     = "https://api.moonshot.cn/v1/users/me/balance"
	MoonshotGlobalEndpoint = "https://api.moonshot.ai/v1/users/me/balance"
)

const (
	maxSources          = 20
	maxRecordsPerSource = 12
	maxResponseBytes    = 64 * 1024
	maxErrorBodyBytes   = 2048
	defaultTimeoutMS    = 15000
	minTimeoutMS        = 1000
	maxTimeoutMS        = 60000
	defaultFreshnessMS  = 15 * 60 * 1000
	defaultThrottleMS   = 1000
	maxPathChars        = 200
	maxPathSegments     = 8
	maxAmount           = 1e12
	maxLabelChars       = 40
	maxNameChars        = 80
	maxBodyChars        = 2048
	maxJSONDepth        = 6
	maxJSONKeys         = 32
	maxURLChars         = 2048
)

var (
	controlPattern    = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	sourceIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	credentialPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+/-]{0,79}$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
	numericString     = regexp.MustCompile(`^-?\d{1,15}(?:\.\d{1,6})?$`)
	pathSegment       = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_-]{0,63}|\d{1,3})$`)
	secretShaped      = regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}`)
	bodySecretKey     = regexp.MustCompile("(?i)(?:token|secret|pass" + "word|passwd|apikey|api_key|authorization|credential)")
	bodyInferenceKey  = regexp.MustCompile(`(?i)^(?:model|messages|prompt|input|tools?|tool_calls?|images?|stream|max_tokens|max_output_tokens|modalities|context)$`)
	inferencePath     = regexp.MustCompile(`(?i)/(?:chat/completions|completions|responses|messages|embeddings|images(?:/|$)|audio(?:/|$)|videos(?:/|$)|generateContent|streamGenerateContent)(?:/|$)`)
	bodyFieldPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
)

var customUnits = map[string]bool{"CNY": true, "USD": true, "EUR": true, "JPY": true, "HKD": true,
	"GBP": true, "credits": true, "requests": true, "tokens": true}

// A custom source declares what its own gateway reports. `subscription_window`
// is deliberately not offered: that kind means a vendor's own subscription
// allowance, and a gateway's window is not the vendor's statement.
var customKinds = map[string]bool{"api_balance": true, "coding_plan": true, "gateway_limit": true}

var authModes = map[string]bool{"bearer": true, "header": true}
var authHeaders = map[string]bool{"authorization": true, "x-api-key": true, "api-key": true}
var reservedSegments = map[string]bool{"__proto__": true, "prototype": true, "constructor": true}
var blockedSuffixes = []string{".local", ".internal", ".localdomain", ".lan", ".home.arpa", ".intranet", ".corp"}
var loopbackNames = map[string]bool{"localhost": true, "localhost.localdomain": true}

// SourceError is a validation failure a user can act on.
type SourceError struct {
	Code    string
	Message string
}

// Error implements error.
func (e *SourceError) Error() string { return e.Code }

func sourceError(code, message string) *SourceError {
	return &SourceError{Code: code, Message: message}
}

func isControl(value string) bool { return controlPattern.MatchString(value) }

func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }

// ToAmount reads an amount from a number or a numeric string. Everything else
// is "unknown", which is not 0. DeepSeek returns its amounts as strings, so this
// conversion is load-bearing rather than defensive.
func ToAmount(value any) *float64 {
	switch item := value.(type) {
	case float64:
		if math.IsNaN(item) || math.IsInf(item, 0) || math.Abs(item) > maxAmount {
			return nil
		}
		result := round6(item)
		return &result
	case int64:
		result := float64(item)
		return &result
	case int:
		result := float64(item)
		return &result
	case json.Number:
		parsed, err := strconv.ParseFloat(item.String(), 64)
		if err != nil || math.Abs(parsed) > maxAmount {
			return nil
		}
		result := round6(parsed)
		return &result
	case string:
		trimmed := strings.TrimSpace(item)
		if !numericString.MatchString(trimmed) {
			return nil
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsInf(parsed, 0) || math.Abs(parsed) > maxAmount {
			return nil
		}
		result := round6(parsed)
		return &result
	}
	return nil
}

var isoInstant = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)

// ToTimestamp reads a reset time from an epoch (seconds or milliseconds) or an
// ISO-8601 instant. A bare date, a locale string or a duration is refused
// rather than guessed at.
func ToTimestamp(value any) *int64 {
	switch item := value.(type) {
	case float64:
		if math.IsNaN(item) || item <= 0 {
			return nil
		}
		ms := item
		if ms <= 1e11 {
			ms *= 1000
		}
		if ms >= 8.64e15 {
			return nil
		}
		result := int64(ms)
		return &result
	case int64:
		if item <= 0 {
			return nil
		}
		ms := item
		if ms <= 1e11 {
			ms *= 1000
		}
		result := ms
		return &result
	case string:
		trimmed := strings.TrimSpace(item)
		if !isoInstant.MatchString(trimmed) {
			return nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, trimmed)
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04:05Z07:00", trimmed)
		}
		if err != nil {
			return nil
		}
		ms := parsed.UnixMilli()
		if ms <= 0 || ms >= 8.64e15 {
			return nil
		}
		return &ms
	}
	return nil
}

// AccountTag is a stable, non-secret tag for one account. The credential itself
// never leaves this function.
func AccountTag(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return "acct-" + hex.EncodeToString(sum[:])[:8]
}

// SanitizeLabel validates a display name. A label a person typed is only shown
// when it does not look like a credential: pasting a key into a display name
// would otherwise put it into State and into the cache file.
func SanitizeLabel(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if len([]rune(trimmed)) > maxLabelChars {
		return "", sourceError("invalid_label", "The account label is too long.")
	}
	if isControl(trimmed) {
		return "", sourceError("invalid_label", "The account label contains control characters.")
	}
	if secretShaped.MatchString(trimmed) {
		return "", sourceError("invalid_label", "The account label looks like a credential; use a short display name instead.")
	}
	return trimmed, nil
}

// --- URL and host checks ---------------------------------------------------

func ipv4Parts(host string) []int {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return nil
	}
	out := make([]int, 4)
	for i, part := range parts {
		if part == "" || len(part) > 3 {
			return nil
		}
		value := atoiSafe(part)
		if value < 0 || value > 255 {
			return nil
		}
		out[i] = value
	}
	return out
}

func atoiSafe(value string) int {
	result := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return -1
		}
		result = result*10 + int(r-'0')
	}
	return result
}

func isPrivateIPv4(parts []int) bool {
	a, b := parts[0], parts[1]
	switch {
	case a == 0 || a == 10:
		return true
	case a == 100 && b >= 64 && b <= 127:
		return true
	case a == 169 && b == 254:
		return true
	case a == 172 && b >= 16 && b <= 31:
		return true
	case a == 192 && b == 168:
		return true
	case a == 192 && b == 0:
		return true
	case a == 198 && (b == 18 || b == 19):
		return true
	case a >= 224:
		return true
	}
	return false
}

func normalizeHostname(hostname string) string {
	host := strings.ToLower(hostname)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return host
}

func isLoopbackHost(host string) bool {
	if loopbackNames[host] {
		return true
	}
	if parts := ipv4Parts(host); parts != nil && parts[0] == 127 {
		return true
	}
	return host == "::1" || host == "0:0:0:0:0:0:0:1"
}

func isBlockedHost(host string) bool {
	if host == "" {
		return true
	}
	if isLoopbackHost(host) {
		return false
	}
	if parts := ipv4Parts(host); parts != nil {
		return isPrivateIPv4(parts)
	}
	if strings.Contains(host, ":") {
		// IPv6 literal: unique-local, link-local, multicast, unspecified, and
		// IPv4-mapped forms of the ranges above.
		if host == "::" || host == "::1" {
			return true
		}
		for _, prefix := range []string{"fc", "fd", "fe8", "fe9", "fea", "feb", "ff"} {
			if strings.HasPrefix(host, prefix) {
				return true
			}
		}
		if strings.HasPrefix(host, "::ffff:") {
			return isBlockedHost(host[len("::ffff:"):])
		}
		return false
	}
	for _, suffix := range blockedSuffixes {
		if host == suffix[1:] || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	// A single label is an intranet name by construction (`metadata`, `nas`).
	if !strings.Contains(host, ".") {
		return true
	}
	return false
}

// CheckResult is the static gate every custom endpoint passes.
type CheckResult struct {
	OK       bool
	Code     string
	Href     string
	Host     string
	Protocol string
}

// CheckEndpoint is the static gate a custom endpoint passes before it is
// stored, and again before it is requested (a stored config file can be edited
// by hand).
//
// This is static validation, not a resolved-address check. A public hostname can
// resolve to a private address at request time (DNS rebinding), and nothing here
// resolves DNS to prevent it. Describe it as a narrow, number-only read with a
// host allowlist rule, not as a complete SSRF defence.
func CheckEndpoint(raw string) CheckResult {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxURLChars || isControl(value) {
		return CheckResult{Code: "invalid_endpoint"}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return CheckResult{Code: "invalid_endpoint"}
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return CheckResult{Code: "invalid_endpoint"}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return CheckResult{Code: "invalid_endpoint"}
	}
	host := normalizeHostname(parsed.Hostname())
	if host == "" {
		return CheckResult{Code: "invalid_endpoint"}
	}
	if parsed.Scheme == "http" && !isLoopbackHost(host) {
		// Plain HTTP to a remote host would put the credential on the wire in
		// clear.
		return CheckResult{Code: "insecure_endpoint"}
	}
	if isBlockedHost(host) {
		return CheckResult{Code: "blocked_target"}
	}
	if inferencePath.MatchString(parsed.Path) {
		return CheckResult{Code: "inference_endpoint"}
	}
	return CheckResult{OK: true, Href: parsed.String(), Host: host, Protocol: parsed.Scheme}
}

// --- JSON path reading -----------------------------------------------------

// ParsePath validates a dotted value path.
func ParsePath(dotted string) ([]string, error) {
	value := strings.TrimSpace(dotted)
	if value == "" || len(value) > maxPathChars {
		return nil, sourceError("invalid_path", "A value path must be 1-200 characters.")
	}
	segments := strings.Split(value, ".")
	if len(segments) > maxPathSegments {
		return nil, sourceError("invalid_path", "A value path is too deep.")
	}
	for _, segment := range segments {
		if !pathSegment.MatchString(segment) {
			return nil, sourceError("invalid_path", "A value path may only use letters, digits, \"_\", \"-\" and \".\" between fields.")
		}
		if reservedSegments[segment] {
			return nil, sourceError("invalid_path", "A value path may not name a prototype property.")
		}
	}
	return segments, nil
}

// ReadPath reads one value at a declared path. Only object members and array
// items are read, and a miss is a miss — never a default, never 0.
func ReadPath(document any, dotted string) (any, bool) {
	segments, err := ParsePath(dotted)
	if err != nil {
		return nil, false
	}
	current := document
	for _, segment := range segments {
		if isIndex(segment) {
			rows, ok := current.([]any)
			if !ok {
				return nil, false
			}
			index := atoiSafe(segment)
			if index < 0 || index >= len(rows) {
				return nil, false
			}
			current = rows[index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := object[segment]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func isIndex(segment string) bool {
	if segment == "" || len(segment) > 3 {
		return false
	}
	for _, r := range segment {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// --- built-in vendor parsers ----------------------------------------------

// Parsed is one adapter's answer before it becomes a card.
type Parsed struct {
	Available *bool
	Records   []Record
	Parts     []Part
}

// Part is what a balance is made of. Informational: these are not amounts that
// add up with `remaining`.
type Part struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	Unit   *string `json:"unit"`
}

// Record is one reading with its own unit.
type Record struct {
	Kind        string   `json:"kind"`
	Unit        *string  `json:"unit"`
	WindowLabel *string  `json:"windowLabel"`
	Limit       *float64 `json:"limit"`
	Used        *float64 `json:"used"`
	Remaining   *float64 `json:"remaining"`
	PeriodStart *int64   `json:"periodStart"`
	ResetAt     *int64   `json:"resetAt"`
}

// ParseDeepSeekBalance reads the documented DeepSeek reply. Amounts are strings
// in the official reply, and the order of `balance_infos` is not stable between
// reads, so the records are sorted: a currency with money left first, then by
// code. A card that reorders itself on every refresh is worse than a card that
// is slightly arbitrary.
func ParseDeepSeekBalance(document any) (Parsed, string) {
	object, ok := document.(map[string]any)
	if !ok {
		return Parsed{}, "invalid_response"
	}
	infos, ok := object["balance_infos"].([]any)
	if !ok {
		return Parsed{}, "invalid_response"
	}
	type reading struct {
		currency string
		total    float64
		granted  *float64
		topped   *float64
	}
	readings := []reading{}
	for _, row := range infos {
		info, ok := row.(map[string]any)
		if !ok {
			continue
		}
		currency := strings.ToUpper(strings.TrimSpace(stringOf(info["currency"])))
		if !currencyPattern.MatchString(currency) {
			continue
		}
		total := ToAmount(info["total_balance"])
		if total == nil {
			continue
		}
		readings = append(readings, reading{currency: currency, total: *total,
			granted: ToAmount(info["granted_balance"]), topped: ToAmount(info["topped_up_balance"])})
	}
	if len(readings) == 0 {
		return Parsed{}, "invalid_response"
	}
	sort.SliceStable(readings, func(i, j int) bool {
		left, right := readings[i], readings[j]
		if (left.total > 0) != (right.total > 0) {
			return left.total > 0
		}
		return left.currency < right.currency
	})
	parts := []Part{}
	records := []Record{}
	for _, reading := range readings {
		if reading.granted != nil && reading.topped != nil && *reading.granted != 0 {
			unit := reading.currency
			parts = append(parts, Part{Label: "赠金", Amount: *reading.granted, Unit: &unit})
			parts = append(parts, Part{Label: "充值", Amount: *reading.topped, Unit: &unit})
		}
		unit := reading.currency
		remaining := reading.total
		records = append(records, Record{Unit: &unit, Remaining: &remaining})
	}
	parsed := Parsed{Records: records, Parts: parts}
	if flag, ok := object["is_available"].(bool); ok {
		parsed.Available = &flag
	}
	return parsed, ""
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

// ParseMoonshotBalance reads the documented Moonshot reply. `code` 0 is the
// documented success value; anything else is a refusal whose text is not read.
// `cash_balance` may be negative (money owed), so only the available figure is
// used as `remaining`.
func ParseMoonshotBalance(document any) (Parsed, string) {
	object, ok := document.(map[string]any)
	if !ok {
		return Parsed{}, "invalid_response"
	}
	if code, ok := object["code"].(float64); !ok || code != 0 {
		return Parsed{}, "auth_rejected"
	}
	if status, ok := object["status"].(bool); ok && !status {
		return Parsed{}, "invalid_response"
	}
	data, ok := object["data"].(map[string]any)
	if !ok {
		return Parsed{}, "invalid_response"
	}
	available := ToAmount(data["available_balance"])
	if available == nil {
		return Parsed{}, "invalid_response"
	}
	parts := []Part{}
	if voucher := ToAmount(data["voucher_balance"]); voucher != nil {
		parts = append(parts, Part{Label: "代金券", Amount: *voucher})
	}
	if cash := ToAmount(data["cash_balance"]); cash != nil {
		parts = append(parts, Part{Label: "现金", Amount: *cash})
	}
	return Parsed{Records: []Record{{Remaining: available}}, Parts: parts}, ""
}
