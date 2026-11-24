package kimi

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Model is the subset of a saved catalog entry the contract probe needs. The
// gateway owns the full document; this type only carries what is rendered into
// the Kimi-facing api.json shape.
type Model struct {
	ID              string
	CatalogID       string `json:"-"`
	Name            string
	Protocol        string
	ContextWindow   int64
	MaxOutputTokens int64
	Tools           bool
	Reasoning       bool
	InputModalities []string
	ReasoningEffort []string
	DefaultEffort   string
}

var ErrInvalidModels = errors.New("invalid_models")

// ValidateModels mirrors the gateway's acceptance rules for the fields this
// package renders. Anything it cannot represent is refused rather than
// silently dropped, because a probe that runs on a reduced catalog would report
// a compatibility verdict for models the user never saved.
func ValidateModels(models []Model) ([]Model, error) {
	seen := make(map[string]bool, len(models))
	out := make([]Model, 0, len(models))
	for _, row := range models {
		id := strings.TrimSpace(row.ID)
		if id == "" || len(id) > 512 || seen[id] {
			return nil, ErrInvalidModels
		}
		seen[id] = true
		if row.Protocol != "chat" && row.Protocol != "responses" {
			return nil, ErrInvalidModels
		}
		if ModelFamily(id) != row.Protocol {
			return nil, errors.New("protocol_conflict")
		}
		if row.ContextWindow <= 0 {
			return nil, ErrInvalidModels
		}
		if row.MaxOutputTokens < 0 {
			return nil, ErrInvalidModels
		}
		for _, value := range row.InputModalities {
			if value != "text" && value != "image" {
				return nil, ErrInvalidModels
			}
		}
		for _, value := range row.ReasoningEffort {
			if value == "" || len(value) > 64 {
				return nil, ErrInvalidModels
			}
		}
		if row.DefaultEffort != "" && !contains(row.ReasoningEffort, row.DefaultEffort) {
			return nil, ErrInvalidModels
		}
		out = append(out, row)
	}
	return out, nil
}

// ModelFamily decides which Kimi catalog group an id belongs to. It reads the
// last path segment only, so a grouped id keeps its prefix for the upstream.
func ModelFamily(id string) string {
	last := id
	if at := strings.LastIndex(id, "/"); at >= 0 {
		last = id[at+1:]
	}
	if strings.HasPrefix(strings.ToLower(last), "kimi-") {
		return "chat"
	}
	return "responses"
}

// SampleModels is the fixed probe catalog. It covers both groups, a nested id,
// image input, reasoning efforts and a plain id, so a bundle that accepts all
// five is accepting the shape the gateway emits rather than one lucky entry.
func SampleModels() []Model {
	return []Model{
		{ID: "kimi-probe-model", Name: "Kimi Probe", Protocol: "chat", ContextWindow: 131072,
			MaxOutputTokens: 8192, Tools: true, Reasoning: true,
			InputModalities: []string{"text", "image"}, ReasoningEffort: []string{"low", "high"}, DefaultEffort: "high"},
		{ID: "group/kimi-probe-nested", Protocol: "chat", ContextWindow: 65536, Tools: true},
		{ID: "gpt-probe-model", Protocol: "responses", ContextWindow: 262144, MaxOutputTokens: 16384,
			Tools: true, Reasoning: true, InputModalities: []string{"text", "image"}},
		{ID: "group/generic-probe-model", Protocol: "responses", ContextWindow: 32768, Tools: true},
		{ID: "plain-probe-id", Protocol: "responses", ContextWindow: 8192, Tools: true},
	}
}

// Catalog renders the api.json body Kimi's parser receives. Groups with no
// members are omitted, exactly as the gateway does, so an empty protocol never
// appears as an empty object.
//
// Member order follows the model slice, because the parser builds its model
// list by walking Object.values: sorting the ids here would reorder what the
// user sees in Kimi's menu.
func Catalog(models []Model) *orderedObject {
	kimi := newOrdered()
	responses := newOrdered()
	for _, row := range models {
		entry := newOrdered().
			set("name", firstNonEmpty(row.Name, row.ID)).
			set("limit", limitOf(row)).
			set("tool_call", row.Tools).
			set("reasoning", row.Reasoning).
			set("modalities", newOrdered().
				set("input", modalitiesOf(row.InputModalities)).
				set("output", []any{"text"}))
		if len(row.ReasoningEffort) > 0 {
			efforts := make([]any, len(row.ReasoningEffort))
			for i, value := range row.ReasoningEffort {
				efforts[i] = value
			}
			entry.set("support_efforts", efforts)
			entry.set("default_effort", firstNonEmpty(row.DefaultEffort, row.ReasoningEffort[0]))
		}
		if row.Protocol == "chat" {
			kimi.set(row.ID, entry)
		} else {
			responses.set(row.ID, entry)
		}
	}
	const probeAPI = "http://127.0.0.1:43117/v1"
	out := newOrdered()
	if len(kimi.keys) > 0 {
		out.set("kimi", newOrdered().set("type", "kimi").set("api", probeAPI).set("models", kimi))
	}
	if len(responses.keys) > 0 {
		out.set("responses", newOrdered().set("type", "openai_responses").set("api", probeAPI).set("models", responses))
	}
	return out
}

func limitOf(row Model) *orderedObject {
	limit := newOrdered().set("context", row.ContextWindow)
	if row.MaxOutputTokens > 0 {
		limit.set("output", row.MaxOutputTokens)
	}
	return limit
}

func modalitiesOf(declared []string) []any {
	for _, value := range declared {
		if value == "image" {
			return []any{"text", "image"}
		}
	}
	return []any{"text"}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// SortedIDs is the deterministic ordering used in verdicts and diagnostics.
func SortedIDs(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func (m Model) String() string {
	return fmt.Sprintf("%s(%s)", m.ID, m.Protocol)
}
