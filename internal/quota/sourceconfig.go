package quota

import (
	"encoding/json"
	"sort"
	"strings"
)

// Preset is one adapter the panel can offer. `Supported: false` is a real
// answer: it says the vendor has no documented read-only usage interface, and
// names the page a person can open instead.
type Preset struct {
	Adapter        string   `json:"adapter"`
	ProviderID     string   `json:"providerId"`
	SourceName     string   `json:"sourceName"`
	Kind           string   `json:"kind"`
	Unit           *string  `json:"unit"`
	Supported      bool     `json:"supported"`
	CredentialRef  string   `json:"credentialRef"`
	CredentialHint *string  `json:"credentialHint"`
	OfficialPage   *string  `json:"officialPage"`
	ConsoleURL     *string  `json:"consoleUrl"`
	Notes          []string `json:"notes"`
}

func str(value string) *string { return &value }

// Presets are the documented adapters plus the custom one.
func Presets() []Preset {
	return []Preset{
		{Adapter: "deepseek", ProviderID: "deepseek", SourceName: "DeepSeek 余额", Kind: "api_balance",
			Unit: str("CNY"), Supported: true, CredentialRef: "deepseek:default",
			CredentialHint: str("DeepSeek 平台 API Key"),
			OfficialPage:   str("https://api-docs.deepseek.com/api/get-user-balance"),
			ConsoleURL:     str("https://platform.deepseek.com/"),
			Notes:          []string{"按币种分行显示总余额；同一账户的 CNY 与 USD 是两个余额，不可相加。"}},
		{Adapter: "moonshot-cn", ProviderID: "moonshot-open-cn", SourceName: "Kimi 开放平台余额（国内）",
			Kind: "api_balance", Unit: str("CNY"), Supported: true, CredentialRef: "moonshot:default",
			CredentialHint: str("Kimi 开放平台 API Key（platform.kimi.com / platform.moonshot.cn）"),
			OfficialPage:   str("https://platform.moonshot.cn/docs/api/balance"),
			ConsoleURL:     str("https://platform.moonshot.cn/"),
			Notes:          []string{"这是开放平台按量余额，与 Kimi Code 会员订阅额度是两个产品、两套 Key。"}},
		{Adapter: "moonshot-global", ProviderID: "moonshot-open-global", SourceName: "Kimi 开放平台余额（国际）",
			Kind: "api_balance", Unit: str("USD"), Supported: true, CredentialRef: "moonshot:global",
			CredentialHint: str("Kimi 开放平台 API Key（platform.kimi.ai）"),
			OfficialPage:   str("https://platform.kimi.ai/docs/api/balance"),
			ConsoleURL:     str("https://platform.kimi.ai/"),
			Notes:          []string{"国内站与国际站的 Key 互不通用，混用会返回 401。"}},
		{Adapter: "kimi-code", ProviderID: "kimi-code", SourceName: "Kimi Code 订阅额度", Kind: "coding_plan",
			Unit: nil, Supported: false, CredentialRef: "kimi-code:default", CredentialHint: nil,
			OfficialPage: str("https://www.kimi.com/code/docs/kimi-code/membership.html"),
			ConsoleURL:   str("https://www.kimi.com/code/console"),
			Notes: []string{
				"Kimi Code 2.4.1 的本机服务器提供实验性只读额度接口，需要运行服务器并明确授权访问令牌；本版本尚未接入。",
				"可在官方控制台查看额度。本产品不自动读取服务器令牌、控制台登录态或 Cookie。",
			}},
		{Adapter: "custom", ProviderID: "custom", SourceName: "自定义余额来源", Kind: "api_balance",
			Unit: nil, Supported: true, CredentialRef: "custom:default",
			CredentialHint: str("该服务自己签发的 Key 或令牌"), OfficialPage: nil, ConsoleURL: nil, Notes: []string{}},
	}
}

func presetFor(adapter string) *Preset {
	for _, preset := range Presets() {
		if preset.Adapter == adapter {
			copied := preset
			return &copied
		}
	}
	return nil
}

// Source is one stored quota source, in the only shape this package keeps.
type Source struct {
	SourceID      string            `json:"sourceId"`
	Adapter       string            `json:"adapter"`
	ProviderID    string            `json:"providerId"`
	SourceName    string            `json:"sourceName"`
	Kind          string            `json:"kind"`
	Unit          *string           `json:"unit"`
	Supported     bool              `json:"supported"`
	CredentialRef string            `json:"credentialRef"`
	AccountLabel  *string           `json:"accountLabel"`
	Method        string            `json:"method,omitempty"`
	Endpoint      string            `json:"endpoint,omitempty"`
	Host          string            `json:"host,omitempty"`
	Auth          map[string]string `json:"auth,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
	Body          map[string]any    `json:"body,omitempty"`
	WindowLabel   *string           `json:"windowLabel,omitempty"`
}

// NormalizeSource turns caller input into the only shape this package stores.
// Every field is checked here, once, so a later refresh can trust the config.
func NormalizeSource(input map[string]any) (Source, error) {
	if input == nil {
		return Source{}, sourceError("invalid_source", "A source must be an object.")
	}
	adapter := strings.TrimSpace(stringOf(input["adapter"]))
	preset := presetFor(adapter)
	if preset == nil {
		return Source{}, sourceError("invalid_adapter", "Unknown source adapter.")
	}
	sourceID := strings.TrimSpace(stringOf(input["sourceId"]))
	if sourceID == "" {
		sourceID = adapter
	}
	if !sourceIDPattern.MatchString(sourceID) {
		return Source{}, sourceError("invalid_source_id", "A source id may use a-z, 0-9, \".\", \"_\" and \"-\".")
	}
	credentialRef := preset.CredentialRef
	if raw, ok := input["credentialRef"]; ok && raw != nil {
		value, err := boundedText(stringOf(raw), 64, "invalid_credential_ref", "A credential reference is 1-64 plain characters.")
		if err != nil {
			return Source{}, err
		}
		credentialRef = value
	}
	if !credentialPattern.MatchString(credentialRef) {
		return Source{}, sourceError("invalid_credential_ref", "A credential reference is 1-64 plain characters.")
	}
	label, err := SanitizeLabel(stringOf(input["accountLabel"]))
	if err != nil {
		return Source{}, err
	}
	sourceName := preset.SourceName
	if raw := strings.TrimSpace(stringOf(input["sourceName"])); raw != "" {
		value, err := boundedText(raw, maxNameChars, "invalid_source_name", "A source name is 1-80 plain characters.")
		if err != nil {
			return Source{}, err
		}
		sourceName = value
	}
	unit := preset.Unit
	if preset.Adapter == "kimi-code" {
		// Nothing to configure beyond identity: there is no request to make.
		return Source{SourceID: sourceID, Adapter: adapter, ProviderID: preset.ProviderID,
			SourceName: sourceName, Kind: preset.Kind, Unit: unit, Supported: false,
			CredentialRef: credentialRef, AccountLabel: optString(label)}, nil
	}
	if preset.Adapter != "custom" {
		return Source{SourceID: sourceID, Adapter: adapter, ProviderID: preset.ProviderID,
			SourceName: sourceName, Kind: preset.Kind, Unit: unit, Supported: true,
			CredentialRef: credentialRef, AccountLabel: optString(label)}, nil
	}
	// --- custom source ---
	providerID := "custom"
	if raw := strings.TrimSpace(stringOf(input["providerId"])); raw != "" {
		providerID = raw
	}
	if !identifierPattern.MatchString(providerID) {
		return Source{}, sourceError("invalid_provider", "A provider id is 1-80 plain characters.")
	}
	kind := strings.TrimSpace(stringOf(input["kind"]))
	if !customKinds[kind] {
		// `subscription_window` is deliberately absent: that kind means a
		// vendor's own subscription allowance, which a user-configured endpoint
		// cannot assert.
		return Source{}, sourceError("invalid_kind", "A custom source is an API balance, a Coding Plan window or a gateway limit.")
	}
	unitValue := strings.TrimSpace(stringOf(input["unit"]))
	if !customUnits[unitValue] {
		return Source{}, sourceError("invalid_unit", "Choose one of the supported units.")
	}
	method := strings.ToUpper(strings.TrimSpace(stringOf(input["method"])))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "POST" {
		return Source{}, sourceError("invalid_method", "Only GET and POST are supported.")
	}
	endpoint := CheckEndpoint(stringOf(input["endpoint"]))
	if !endpoint.OK {
		message := "A custom source needs a plain HTTPS address without credentials, query or fragment."
		switch endpoint.Code {
		case "blocked_target":
			message = "That address is a local or private destination; use a public HTTPS address for a custom source."
		case "insecure_endpoint":
			message = "A custom source must use HTTPS, except for a gateway on this machine."
		}
		return Source{}, sourceError(endpoint.Code, message)
	}
	auth := map[string]string{"mode": "bearer"}
	if raw, ok := input["auth"].(map[string]any); ok {
		mode := strings.TrimSpace(stringOf(raw["mode"]))
		if mode == "" {
			mode = "bearer"
		}
		if !authModes[mode] {
			return Source{}, sourceError("invalid_auth", "Choose bearer or a named header for authentication.")
		}
		auth["mode"] = mode
		if mode == "header" {
			header := strings.ToLower(strings.TrimSpace(stringOf(raw["headerName"])))
			if !authHeaders[header] {
				return Source{}, sourceError("invalid_auth", "A custom header must be Authorization, x-api-key or api-key.")
			}
			auth["headerName"] = header
		}
	}
	fieldsInput, _ := input["fields"].(map[string]any)
	fields := map[string]string{}
	for _, name := range []string{"remaining", "used", "limit", "resetAt"} {
		raw := strings.TrimSpace(stringOf(fieldsInput[name]))
		if raw == "" {
			continue
		}
		if _, err := ParsePath(raw); err != nil {
			return Source{}, err
		}
		fields[name] = raw
	}
	if fields["remaining"] == "" && fields["used"] == "" && fields["limit"] == "" {
		return Source{}, sourceError("invalid_fields", "A custom source needs at least one of remaining, used or limit.")
	}
	windowLabel := ""
	if raw := strings.TrimSpace(stringOf(input["windowLabel"])); raw != "" {
		value, err := boundedText(raw, 40, "invalid_window", "A window label is 1-40 plain characters.")
		if err != nil {
			return Source{}, err
		}
		windowLabel = value
	}
	var body map[string]any
	if method == "POST" {
		normalized, err := normalizeBody(input["body"])
		if err != nil {
			return Source{}, err
		}
		body = normalized
	}
	return Source{SourceID: sourceID, Adapter: adapter, ProviderID: providerID, SourceName: sourceName,
		Kind: kind, Unit: optString(unitValue), Supported: true, CredentialRef: credentialRef,
		AccountLabel: optString(label), Method: method, Endpoint: endpoint.Href, Host: endpoint.Host,
		Auth: auth, Fields: fields, Body: body, WindowLabel: optString(windowLabel)}, nil
}

func optString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func boundedText(value string, limit int, code, message string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len([]rune(trimmed)) > limit || isControl(trimmed) {
		return "", sourceError(code, message)
	}
	return trimmed, nil
}

// normalizeBody validates a static POST body. A static body lives in a config
// file on disk, so a credential must not, and a quota request may not carry
// model inference fields.
func normalizeBody(value any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, sourceError("invalid_body", "A POST body must be a plain JSON object.")
	}
	normalized, err := walkBody(object, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded) > maxBodyChars {
		return nil, sourceError("invalid_body", "The POST body is too large.")
	}
	return normalized.(map[string]any), nil
}

func walkBody(node any, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, sourceError("invalid_body", "The POST body is nested too deeply.")
	}
	switch item := node.(type) {
	case []any:
		if len(item) > maxJSONKeys {
			return nil, sourceError("invalid_body", "The POST body has too many items.")
		}
		out := make([]any, 0, len(item))
		for _, entry := range item {
			value, err := walkBody(entry, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case map[string]any:
		if len(item) > maxJSONKeys {
			return nil, sourceError("invalid_body", "The POST body has too many fields.")
		}
		keys := make([]string, 0, len(item))
		for key := range item {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := map[string]any{}
		for _, key := range keys {
			if !bodyFieldPattern.MatchString(key) || reservedSegments[key] {
				return nil, sourceError("invalid_body", "The POST body has an unusable field name.")
			}
			if bodySecretKey.MatchString(key) {
				return nil, sourceError("invalid_body", "A POST body may not carry credentials; use the authentication header.")
			}
			if bodyInferenceKey.MatchString(key) {
				return nil, sourceError("invalid_body", "A quota request may not carry model inference fields.")
			}
			value, err := walkBody(item[key], depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case string:
		if len([]rune(item)) > 512 || isControl(item) {
			return nil, sourceError("invalid_body", "The POST body has an unusable string.")
		}
		return item, nil
	case float64, int64, int, bool, nil:
		return item, nil
	}
	return nil, sourceError("invalid_body", "The POST body has an unusable value.")
}

// DescribeSource is what the panel may see about a stored source, and also the
// on-disk shape: a round trip must not lose a field. Nothing secret is in here —
// the credential is only ever a reference name.
type SourceView struct {
	SourceID      string            `json:"sourceId"`
	Adapter       string            `json:"adapter"`
	ProviderID    string            `json:"providerId"`
	SourceName    string            `json:"sourceName"`
	Kind          string            `json:"kind"`
	Unit          *string           `json:"unit"`
	Supported     bool              `json:"supported"`
	CredentialRef string            `json:"credentialRef"`
	AccountLabel  *string           `json:"accountLabel"`
	Method        string            `json:"method,omitempty"`
	Endpoint      string            `json:"endpoint,omitempty"`
	Host          string            `json:"host,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
	Auth          map[string]string `json:"auth,omitempty"`
	Body          map[string]any    `json:"body,omitempty"`
	WindowLabel   *string           `json:"windowLabel,omitempty"`
	OfficialPage  *string           `json:"officialPage"`
	ConsoleURL    *string           `json:"consoleUrl"`
	Notes         []string          `json:"notes"`
}

// Describe renders the published source shape.
func Describe(source Source) SourceView {
	preset := presetFor(source.Adapter)
	view := SourceView{SourceID: source.SourceID, Adapter: source.Adapter, ProviderID: source.ProviderID,
		SourceName: source.SourceName, Kind: source.Kind, Unit: source.Unit, Supported: source.Supported,
		CredentialRef: source.CredentialRef, AccountLabel: source.AccountLabel, Notes: []string{}}
	if source.Adapter == "custom" {
		view.Method = source.Method
		view.Endpoint = source.Endpoint
		view.Host = source.Host
		view.Fields = map[string]string{}
		for key, value := range source.Fields {
			view.Fields[key] = value
		}
		view.Auth = map[string]string{}
		for key, value := range source.Auth {
			view.Auth[key] = value
		}
		if len(source.Body) > 0 {
			view.Body = source.Body
		}
		view.WindowLabel = source.WindowLabel
	}
	if preset != nil {
		view.OfficialPage = preset.OfficialPage
		view.ConsoleURL = preset.ConsoleURL
		view.Notes = append(view.Notes, preset.Notes...)
	}
	return view
}
