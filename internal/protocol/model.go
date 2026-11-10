package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// Error codes this package reports. They are the bare identifiers the legacy
// gateway used, because callers switch on them and the panel shows them.
var (
	ErrInvalidModels             = errors.New("invalid_models")
	ErrProtocolConflict          = errors.New("protocol_conflict")
	ErrContextWindowExceedsLimit = errors.New("context_window_exceeds_limit")
	ErrInvalidProtocol           = errors.New("invalid_protocol")
	ErrInvalidRequest            = errors.New("invalid_request")
	ErrInvalidInput              = errors.New("invalid_input")
	ErrInvalidMessages           = errors.New("invalid_messages")
	ErrInvalidThinkingBudget     = errors.New("invalid_thinking_budget")
	ErrThinkingToolRoundtrip     = errors.New("anthropic_thinking_tool_roundtrip_unsupported")
	ErrInvalidBaseURL            = errors.New("invalid_base_url")
	ErrInvalidAPIKey             = errors.New("invalid_api_key")
	ErrInvalidConnection         = errors.New("invalid_connection")
	ErrInvalidConnectionID       = errors.New("invalid_connection_id")
	ErrConnectionUnavailable     = errors.New("connection_unavailable")
	ErrCatalogUnavailable        = errors.New("catalog_unavailable")
	ErrInvalidUpstreamResponse   = errors.New("invalid_upstream_response")
	ErrInvalidUpstreamProtocol   = errors.New("invalid_upstream_protocol")
)

const (
	ProtocolChat      = "chat"
	ProtocolResponses = "responses"
	// ProtocolAnthropic is an upstream wire format, never a Kimi-facing route.
	ProtocolAnthropic = "anthropic"

	maxTextChars = 2048
	maxSafeInt   = 1<<53 - 1
)

// MetadataSource records where a model row's capabilities came from. A field
// the upstream never stated stays absent instead of becoming a guess.
type MetadataSource struct {
	Kind      string  `json:"kind"`
	URL       *string `json:"url,omitempty"`
	CheckedAt *string `json:"checkedAt,omitempty"`
}

// Model is one catalog row, in the field order the legacy JSON used. Pointers
// distinguish "absent" from "false"/"0"/"[]": an empty modality list is a real
// value that must survive a round trip.
type Model struct {
	ID               string          `json:"id"`
	Protocol         string          `json:"protocol"`
	ContextWindow    int64           `json:"contextWindow"`
	ContextLimit     *int64          `json:"contextLimit,omitempty"`
	MetadataSource   *MetadataSource `json:"metadataSource,omitempty"`
	ConnectionID     *string         `json:"connectionId,omitempty"`
	UpstreamModelID  *string         `json:"upstreamModelId,omitempty"`
	UpstreamProtocol *string         `json:"upstreamProtocol,omitempty"`
	Name             *string         `json:"name,omitempty"`
	MaxOutputTokens  *int64          `json:"maxOutputTokens,omitempty"`
	Tools            *bool           `json:"tools,omitempty"`
	Reasoning        *bool           `json:"reasoning,omitempty"`
	InputModalities  *[]string       `json:"inputModalities,omitempty"`
	ReasoningEfforts *[]string       `json:"reasoningEfforts,omitempty"`
	DefaultEffort    *string         `json:"defaultEffort,omitempty"`
}

// IsAnthropic reports whether this row is served from a Messages upstream.
func (m Model) IsAnthropic() bool {
	return m.UpstreamProtocol != nil && *m.UpstreamProtocol == ProtocolAnthropic
}

// UpstreamID is the model identifier the upstream receives.
func (m Model) UpstreamID() string {
	if m.UpstreamModelID != nil && *m.UpstreamModelID != "" {
		return *m.UpstreamModelID
	}
	return m.ID
}

// jsLen is the JavaScript string length (UTF-16 code units), which is what the
// legacy bounds were written against.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 {
			return true
		}
	}
	return false
}

// textOf mirrors the legacy `text()` guard: a bounded, control-free string or
// the empty string. It never trims, because trimming would silently change an
// identifier the user chose.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	if jsLen(s) > maxTextChars || hasControl(s) {
		return ""
	}
	return s
}

func jsonNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// positive mirrors `Number.isSafeInteger(value) && value > 0`.
func positive(raw json.RawMessage) (int64, bool) {
	f, ok := jsonNumber(raw)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > maxSafeInt || f <= 0 {
		return 0, false
	}
	return int64(f), true
}

func present(raw json.RawMessage) bool { return len(raw) > 0 }

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func rawBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, false
	}
	return b, true
}

func rawStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, false
	}
	return out, true
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ModelFamily decides which Kimi endpoint a model name routes to. Kimi reads
// this from the name itself, so a row that contradicts it is refused rather
// than quietly served from the wrong API.
func ModelFamily(id string) string {
	last := id
	if at := strings.LastIndex(id, "/"); at >= 0 {
		last = id[at+1:]
	}
	if strings.HasPrefix(strings.ToLower(last), "kimi-") {
		return ProtocolChat
	}
	return ProtocolResponses
}

// ValidateModelsAny accepts rows that already went through a JSON decode.
func ValidateModelsAny(rows []any) ([]Model, error) {
	raw := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			return nil, ErrInvalidModels
		}
		raw = append(raw, b)
	}
	return ValidateModels(raw)
}

var metadataFields = map[string]bool{"kind": true, "url": true, "checkedAt": true}
var metadataKinds = map[string]bool{"preset": true, "discovery": true, "manual": true}

var connectionIDPattern = func(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// checkedAtPattern is `^\d{4}-\d{2}-\d{2}(?:T[\d:.]+Z?)?$`.
func checkedAtPattern(s string) bool {
	if len(s) < 10 {
		return false
	}
	digits := func(v string) bool {
		for _, r := range v {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if !digits(s[0:4]) || s[4] != '-' || !digits(s[5:7]) || s[7] != '-' || !digits(s[8:10]) {
		return false
	}
	rest := s[10:]
	if rest == "" {
		return true
	}
	if rest[0] != 'T' {
		return false
	}
	rest = rest[1:]
	if strings.HasSuffix(rest, "Z") {
		rest = rest[:len(rest)-1]
	}
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9') && r != ':' && r != '.' {
			return false
		}
	}
	return true
}

// ValidateModels normalises a catalog. Every row is checked before any is
// returned, and the result is a copy so a caller cannot mutate its input.
func ValidateModels(rows []json.RawMessage) ([]Model, error) {
	out := make([]Model, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, raw := range rows {
		row, ok := rawObject(raw)
		if !ok {
			return nil, ErrInvalidModels
		}
		id := textOf(row["id"])
		if id == "" {
			return nil, ErrInvalidModels
		}
		if _, dup := seen[id]; dup {
			return nil, ErrInvalidModels
		}
		seen[id] = struct{}{}
		protocol, ok := rawString(row["protocol"])
		if !ok || (protocol != ProtocolChat && protocol != ProtocolResponses) {
			return nil, ErrInvalidModels
		}
		if ModelFamily(id) != protocol {
			return nil, ErrProtocolConflict
		}
		window, ok := positive(row["contextWindow"])
		if !ok {
			return nil, ErrInvalidModels
		}
		model := Model{ID: id, Protocol: protocol, ContextWindow: window}

		if present(row["maxOutputTokens"]) {
			v, ok := positive(row["maxOutputTokens"])
			if !ok {
				return nil, ErrInvalidModels
			}
			model.MaxOutputTokens = &v
		}
		if present(row["tools"]) {
			v, ok := rawBool(row["tools"])
			if !ok {
				return nil, ErrInvalidModels
			}
			model.Tools = &v
		}
		if present(row["reasoning"]) {
			v, ok := rawBool(row["reasoning"])
			if !ok {
				return nil, ErrInvalidModels
			}
			model.Reasoning = &v
		}
		if present(row["inputModalities"]) {
			values, ok := rawStrings(row["inputModalities"])
			if !ok {
				return nil, ErrInvalidModels
			}
			for _, value := range values {
				if value != "text" && value != "image" {
					return nil, ErrInvalidModels
				}
			}
			values = dedupe(values)
			model.InputModalities = &values
		}
		if present(row["reasoningEfforts"]) {
			values, ok := rawStrings(row["reasoningEfforts"])
			if !ok {
				return nil, ErrInvalidModels
			}
			for _, value := range values {
				// A level is a short, control-free token; anything else would be
				// echoed into a UI that cannot render it.
				if value == "" || jsLen(value) > 64 || hasControl(value) {
					return nil, ErrInvalidModels
				}
			}
			values = dedupe(values)
			model.ReasoningEfforts = &values
		}
		if present(row["defaultEffort"]) {
			effort, ok := rawString(row["defaultEffort"])
			if !ok || jsLen(effort) > 64 {
				return nil, ErrInvalidModels
			}
			allowed := false
			if model.ReasoningEfforts != nil {
				for _, value := range *model.ReasoningEfforts {
					if value == effort {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				return nil, ErrInvalidModels
			}
			model.DefaultEffort = &effort
		}
		if present(row["contextLimit"]) {
			limit, ok := positive(row["contextLimit"])
			if !ok {
				return nil, ErrInvalidModels
			}
			if model.ContextWindow > limit {
				return nil, ErrContextWindowExceedsLimit
			}
			model.ContextLimit = &limit
		}
		if present(row["metadataSource"]) {
			source, ok := rawObject(row["metadataSource"])
			if !ok {
				return nil, ErrInvalidModels
			}
			for key := range source {
				if !metadataFields[key] {
					return nil, ErrInvalidModels
				}
			}
			kind, _ := rawString(source["kind"])
			if !metadataKinds[kind] {
				return nil, ErrInvalidModels
			}
			meta := MetadataSource{Kind: kind}
			if present(source["url"]) {
				url := textOf(source["url"])
				if url == "" {
					return nil, ErrInvalidModels
				}
				meta.URL = &url
			}
			if present(source["checkedAt"]) {
				stamp, _ := rawString(source["checkedAt"])
				if !checkedAtPattern(stamp) {
					return nil, ErrInvalidModels
				}
				meta.CheckedAt = &stamp
			}
			model.MetadataSource = &meta
		}
		if present(row["connectionId"]) {
			value, ok := rawString(row["connectionId"])
			if !ok || !connectionIDPattern(value) {
				return nil, ErrInvalidModels
			}
			model.ConnectionID = &value
		}
		if present(row["upstreamModelId"]) {
			value := textOf(row["upstreamModelId"])
			if value == "" {
				return nil, ErrInvalidModels
			}
			model.UpstreamModelID = &value
		}
		if present(row["upstreamProtocol"]) {
			value, _ := rawString(row["upstreamProtocol"])
			if value != ProtocolAnthropic {
				return nil, ErrInvalidModels
			}
			model.UpstreamProtocol = &value
		}
		if name := textOf(row["name"]); name != "" {
			model.Name = &name
		}
		out = append(out, model)
	}
	return out, nil
}

// NativeCatalog renders the catalog Kimi's own model menu reads. The order of
// the rows is the order the menu shows them, so it is preserved by emitting
// JSON directly instead of going through a map.
func NativeCatalog(models []Model) []byte {
	var kimi, responses []Model
	for _, model := range models {
		if model.Protocol == ProtocolChat {
			kimi = append(kimi, model)
		} else {
			responses = append(responses, model)
		}
	}
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	writeGroup := func(name, kind string, rows []Model) {
		if len(rows) == 0 {
			return
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(quote(name))
		b.WriteString(`:{"type":`)
		b.WriteString(quote(kind))
		b.WriteString(`,"models":{`)
		for i, model := range rows {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quote(model.ID))
			b.WriteByte(':')
			b.Write(nativeModel(model))
		}
		b.WriteString("}}")
	}
	writeGroup("kimi", "kimi", kimi)
	writeGroup("responses", "openai_responses", responses)
	b.WriteByte('}')
	return b.Bytes()
}

func nativeModel(model Model) []byte {
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	name := model.ID
	if model.Name != nil && *model.Name != "" {
		name = *model.Name
	}
	b.WriteString(quote(name))
	b.WriteString(`,"limit":{"context":`)
	b.WriteString(strconv.FormatInt(model.ContextWindow, 10))
	if model.MaxOutputTokens != nil {
		b.WriteString(`,"output":`)
		b.WriteString(strconv.FormatInt(*model.MaxOutputTokens, 10))
	}
	b.WriteString(`},"tool_call":`)
	b.WriteString(boolString(model.Tools != nil && *model.Tools))
	b.WriteString(`,"reasoning":`)
	b.WriteString(boolString(model.Reasoning != nil && *model.Reasoning))
	b.WriteString(`,"modalities":{"input":`)
	if model.InputModalities != nil && contains(*model.InputModalities, "image") {
		b.WriteString(`["text","image"]`)
	} else {
		b.WriteString(`["text"]`)
	}
	b.WriteString(`,"output":["text"]}`)
	if model.ReasoningEfforts != nil && len(*model.ReasoningEfforts) > 0 {
		b.WriteString(`,"support_efforts":[`)
		for i, effort := range *model.ReasoningEfforts {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quote(effort))
		}
		b.WriteString(`],"default_effort":`)
		effort := (*model.ReasoningEfforts)[0]
		if model.DefaultEffort != nil && contains(*model.ReasoningEfforts, *model.DefaultEffort) {
			effort = *model.DefaultEffort
		}
		b.WriteString(quote(effort))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// OpenAIModels renders the OpenAI-compatible model list.
func OpenAIModels(models []Model) []byte {
	var b bytes.Buffer
	b.WriteString(`{"object":"list","data":[`)
	for i, model := range models {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":`)
		b.WriteString(quote(model.ID))
		b.WriteString(`,"object":"model","context_length":`)
		b.WriteString(strconv.FormatInt(model.ContextWindow, 10))
		b.WriteString(`,"supports_tool_call":`)
		b.WriteString(boolString(model.Tools != nil && *model.Tools))
		b.WriteString(`,"supports_reasoning":`)
		b.WriteString(boolString(model.Reasoning != nil && *model.Reasoning))
		b.WriteString(`,"supports_image_in":`)
		b.WriteString(boolString(model.InputModalities != nil && contains(*model.InputModalities, "image")))
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
