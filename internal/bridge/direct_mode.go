package bridge

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wssz527/kimiwork-plus/internal/byokdns"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

// Direct Mode turns the gateway's own catalog into the providers and models
// Kimi 3.2.28 accepts through `byokSources`. The gateway document stays the one
// source of truth: the same connections and models are read here, only the
// wire protocol is translated into the protocol names Kimi's BYOK surface uses.
//
// The split matters: `DirectSpec` is what reconciliation verifies and what the
// panel renders, and it never carries a key. A key is read only inside a
// user-authorized import, and the request that holds it is dropped as soon as
// the call returns.

// Skip reasons a model can be reported with. They are the vocabulary the panel
// shows and the operation result records, so they never change silently.
const (
	directSkipMissingConnection   = "missing_connection"
	directSkipInvalidBaseURL      = "invalid_base_url"
	directSkipUnsupportedProtocol = "unsupported_protocol"
	directSkipToolsRequired       = "tools_required"
	directSkipContextWindow       = "invalid_context_window"
	directSkipDuplicateEndpoint   = "duplicate_endpoint"
	directSkipMissingAPIKey       = "missing_api_key"
)

// directProtocolLabels name a protocol in a provider's display name when one
// connection serves more than one of them.
var directProtocolLabels = map[string]string{
	"openai":           "Chat",
	"openai_responses": "Responses",
	"anthropic":        "Anthropic",
	"google-genai":     "Google",
	"kimi":             "Kimi",
}

// directProtocolFor maps the wire a model is served over onto the protocol name
// Kimi's BYOK store knows. `chat` is ambiguous: a Kimi Code preset speaks Kimi's
// own chat protocol, everything else speaks OpenAI chat completions.
func directProtocolFor(wire, providerPresetID string) string {
	switch wire {
	case "chat":
		if strings.HasPrefix(providerPresetID, "kimi-code") {
			return "kimi"
		}
		return "openai"
	case "responses":
		return "openai_responses"
	case "anthropic":
		return "anthropic"
	case "google-genai":
		return "google-genai"
	case "kimi":
		return "kimi"
	}
	return ""
}

// directWire is the protocol a model is served over on a connection that
// declares none. A connection that names one decides for every model on it, so
// this rule is reached only by documents saved before declarations existed;
// those keep importing exactly as they did.
func directWire(row map[string]any) string {
	if value := core.String(row["upstreamProtocol"]); value != "" {
		return value
	}
	return core.String(row["protocol"])
}

// directBaseURL is the endpoint a provider is imported with: https, or loopback
// http, without userinfo, query or fragment, and without a path that already
// names an operation the provider client appends itself.
func directBaseURL(raw string) (string, bool) {
	base, err := core.BaseURL(raw)
	if err != nil {
		return "", false
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		if strings.HasSuffix(path, suffix) {
			return "", false
		}
	}
	return base, true
}

// normalizeDirectEndpoint is the comparison Kimi itself makes between two
// provider endpoints: case-folded scheme and host, no trailing slash, and no
// trailing /v1 on an anthropic endpoint.
func normalizeDirectEndpoint(value, protocol string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	if protocol == "anthropic" {
		path = strings.TrimSuffix(path, "/v1")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host) + path
}

func jsonInt(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case int32:
		return int64(typed)
	}
	return 0
}

// directProviderPlan is one provider the import will declare, plus the models
// that belong to it. It is built without any key.
type directProviderPlan struct {
	id           string
	displayName  string
	protocol     string
	baseURL      string
	connectionID string
	models       []kimi.DirectModel
}

// directPlan is the whole translation of one gateway document.
type directPlan struct {
	connections map[string]map[string]any
	providers   []*directProviderPlan
	skipped     []core.Document
	total       int
	duplicates  int
}

func (p *directPlan) skip(id, reason string) {
	p.skipped = append(p.skipped, core.Document{"id": id, "reason": reason})
}

func (p *directPlan) eligible() int {
	count := 0
	for _, provider := range p.providers {
		count += len(provider.models)
	}
	return count
}

// skippedRows is never nil: the panel renders an empty list, not a missing one.
func (p *directPlan) skippedRows() []core.Document {
	rows := make([]core.Document, 0, len(p.skipped))
	for _, row := range p.skipped {
		rows = append(rows, core.Document{"id": row["id"], "reason": row["reason"]})
	}
	return rows
}

type directCandidate struct {
	connectionID string
	protocol     string
	baseURL      string
	model        kimi.DirectModel
}

// buildDirectPlan translates the document. Credential availability, when given,
// contains only booleans so a state poll never publishes a key.
func buildDirectPlan(d core.Document, availability ...map[string]bool) directPlan {
	var credentials map[string]bool
	if len(availability) > 0 {
		credentials = availability[0]
	}
	plan := directPlan{connections: map[string]map[string]any{}}
	for _, value := range core.Rows(d["connections"]) {
		connection := core.Object(value)
		if id := core.String(connection["id"]); id != "" {
			plan.connections[id] = connection
		}
	}
	rows := core.Rows(d["models"])
	plan.total = len(rows)

	endpoints := map[string]string{}
	seenModels := map[string]bool{}
	candidates := make([]directCandidate, 0, len(rows))
	for _, value := range rows {
		row := core.Object(value)
		id := core.String(row["id"])
		if id != "" && seenModels[id] {
			continue
		}
		connectionID := core.String(row["connectionId"])
		connection := plan.connections[connectionID]
		if connection == nil {
			plan.skip(id, directSkipMissingConnection)
			continue
		}
		if credentials != nil && !credentials[connectionID] {
			plan.skip(id, directSkipMissingAPIKey)
			continue
		}
		wire := directWire(row)
		// A connection that names chat or responses decides the wire for every
		// model on it, so a model id that maps to the other Kimi route is imported
		// on the connection's wire rather than split into a second provider.
		// `upstreamProtocol` is the one exception: a Messages row is a model-level
		// format this build keeps exactly as it was, because a connection can only
		// declare one of the two OpenAI wires.
		if declared := core.ConnectionProtocol(connection); declared != "" && core.String(row["upstreamProtocol"]) == "" {
			wire = declared
		}
		protocol := directProtocolFor(wire, core.String(connection["providerPresetId"]))
		if protocol == "" {
			plan.skip(id, directSkipUnsupportedProtocol)
			continue
		}
		baseURL, ok := directBaseURL(core.String(connection["baseURL"]))
		if !ok {
			plan.skip(id, directSkipInvalidBaseURL)
			continue
		}
		window := jsonInt(row["contextWindow"])
		if window <= 0 {
			plan.skip(id, directSkipContextWindow)
			continue
		}
		// An omitted `tools` and an explicit false mean the same thing here: the
		// gateway catalog renders such a model without tool support, and Kimi's
		// own import requires `tool_use`.
		if row["tools"] != true {
			plan.skip(id, directSkipToolsRequired)
			continue
		}
		if id == "" {
			plan.skip(id, directSkipMissingConnection)
			continue
		}
		pair := protocol + "\x00" + normalizeDirectEndpoint(baseURL, protocol)
		if owner, taken := endpoints[pair]; taken && owner != connectionID {
			// Two providers on one endpoint would collapse into the same provider
			// on Kimi's next sync, so the later connection is reported rather than
			// silently merged with the first.
			plan.duplicates++
			plan.skip(id, directSkipDuplicateEndpoint)
			continue
		}
		endpoints[pair] = connectionID
		seenModels[id] = true
		candidates = append(candidates, directCandidate{
			connectionID: connectionID,
			protocol:     protocol,
			baseURL:      baseURL,
			model:        directModelOf(row, id),
		})
	}

	byPair := map[string]*directProviderPlan{}
	protocols := map[string]map[string]bool{}
	for _, candidate := range candidates {
		key := candidate.connectionID + "\x00" + candidate.protocol
		provider := byPair[key]
		if provider == nil {
			provider = &directProviderPlan{
				id:           "kwp-" + candidate.connectionID + "-" + candidate.protocol,
				protocol:     candidate.protocol,
				baseURL:      candidate.baseURL,
				connectionID: candidate.connectionID,
			}
			byPair[key] = provider
			plan.providers = append(plan.providers, provider)
			if protocols[candidate.connectionID] == nil {
				protocols[candidate.connectionID] = map[string]bool{}
			}
			protocols[candidate.connectionID][candidate.protocol] = true
		}
		provider.models = append(provider.models, candidate.model)
	}
	for _, provider := range plan.providers {
		name := strings.TrimSpace(core.String(plan.connections[provider.connectionID]["name"]))
		if name == "" {
			name = provider.connectionID
		}
		if len(protocols[provider.connectionID]) > 1 {
			name = name + " (" + directProtocolLabels[provider.protocol] + ")"
		}
		provider.displayName = name
		for index := range provider.models {
			provider.models[index].ProviderID = provider.id
		}
	}
	return plan
}

// directModelOf builds one model entry. `tool_use` is always present; the rest
// follow the fields the row actually states.
func directModelOf(row map[string]any, id string) kimi.DirectModel {
	capabilities := []string{"tool_use"}
	if row["reasoning"] == true {
		capabilities = append(capabilities, "thinking")
	}
	image, video := false, false
	for _, modality := range core.Rows(row["inputModalities"]) {
		switch core.String(modality) {
		case "image":
			image = true
		case "video":
			video = true
		}
	}
	if image {
		capabilities = append(capabilities, "image_in")
	}
	if video {
		capabilities = append(capabilities, "video_in")
	}
	upstream := core.String(row["upstreamModelId"])
	if upstream == "" {
		upstream = id
	}
	displayName := strings.TrimSpace(core.String(row["name"]))
	if displayName == "" {
		displayName = upstream
	}
	model := kimi.DirectModel{
		ID:             id,
		Model:          upstream,
		DisplayName:    displayName,
		MaxContextSize: jsonInt(row["contextWindow"]),
		Capabilities:   capabilities,
	}
	if output := jsonInt(row["maxOutputTokens"]); output > 0 {
		model.MaxOutputSize = output
	}
	model.SupportEfforts = directEfforts(row["reasoningEfforts"])
	if effort := core.String(row["defaultEffort"]); effort != "" && containsText(model.SupportEfforts, effort) {
		model.DefaultEffort = effort
	}
	return model
}

func directEfforts(raw any) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range core.Rows(raw) {
		effort := core.String(value)
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func containsText(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// buildDirectSpec is the non-secret declaration handed to the controller. It is
// nil in gateway mode: a Direct spec in the settings would make reconciliation
// look for a source that mode never creates.
func buildDirectSpec(d core.Document, availability ...map[string]bool) *kimi.DirectSpec {
	if core.IntegrationMode(d) != core.IntegrationModeDirect {
		return nil
	}
	return buildDirectSpecForPlan(buildDirectPlan(d, availability...))
}

func buildDirectSpecForPlan(plan directPlan) *kimi.DirectSpec {
	spec := &kimi.DirectSpec{DisplayName: kimi.DirectDisplayName, Providers: []kimi.DirectProviderSpec{}, Models: []kimi.DirectModel{}}
	for _, provider := range plan.providers {
		spec.Providers = append(spec.Providers, kimi.DirectProviderSpec{
			ID: provider.id, DisplayName: provider.displayName,
			Protocol: provider.protocol, BaseURL: provider.baseURL,
		})
		spec.Models = append(spec.Models, provider.models...)
	}
	return spec
}

// directImportRequest assembles the one-shot request with keys read from the
// credential store. Providers whose key cannot be read are left out, with their
// models reported as skipped; when nothing is left the import refuses instead of
// writing a source that would answer every request with an authentication error.
func directImportRequest(d core.Document, keys KeyStore, visible func(KeyStore, string) (string, error)) (kimi.DirectImportRequest, []core.Document, error) {
	plan := buildDirectPlan(d)
	request := kimi.DirectImportRequest{
		DisplayName:       kimi.DirectDisplayName,
		DisconnectGateway: true,
		OpenSettings:      true,
	}
	skipped := make([]core.Document, 0, len(plan.skipped))
	skipped = append(skipped, plan.skipped...)
	for _, provider := range plan.providers {
		connection := plan.connections[provider.connectionID]
		key, err := credentialFor(connection, keys)
		if (err != nil || key == "") && keys != nil && visible != nil {
			if recovered, visibleErr := visible(keys, credentialReference(connection)); visibleErr == nil {
				key = recovered
			}
		}
		if key == "" {
			for _, model := range provider.models {
				skipped = append(skipped, core.Document{"id": model.ID, "reason": directSkipMissingAPIKey})
			}
			continue
		}
		request.Providers = append(request.Providers, kimi.DirectProvider{
			ID: provider.id, DisplayName: provider.displayName,
			Protocol: provider.protocol, BaseURL: provider.baseURL, APIKey: key,
		})
		request.Models = append(request.Models, provider.models...)
	}
	if len(request.Models) == 0 {
		if plan.eligible() == 0 && plan.duplicates > 0 {
			return kimi.DirectImportRequest{}, skipped, errors.New("direct_duplicate_endpoint")
		}
		if plan.eligible() == 0 {
			return kimi.DirectImportRequest{}, skipped, errors.New("direct_no_eligible_models")
		}
		return kimi.DirectImportRequest{}, skipped, errors.New("direct_missing_api_key")
	}
	return request, skipped, nil
}

// ---------------------------------------------------------------------------
// native side

// isDirectMode reads the mode the controller was configured with, which is the
// document's own value after Configure.
func (n *Native) isDirectMode() bool {
	if n == nil || n.controller == nil {
		return false
	}
	return n.controller.Settings().IntegrationMode == kimi.IntegrationModeDirect
}

func (n *Native) darwinResolverPath() string {
	if n.resolverPath != "" {
		return n.resolverPath
	}
	return "/etc/resolver/" + byokdns.Domain
}

func (n *Native) directDNSCanStop() bool {
	if runtime.GOOS != "darwin" || !byokdns.HasManagedHostsEntry(n.darwinHostsPath()) {
		return false
	}
	_, err := os.Lstat(n.darwinResolverPath())
	return errors.Is(err, os.ErrNotExist)
}

// darwinHostsPath is the file a static entry goes into. It is a field so a test
// can point it at a temporary file instead of the machine's real /etc/hosts.
func (n *Native) darwinHostsPath() string {
	if n.hostsPath != "" {
		return n.hostsPath
	}
	return byokdns.DarwinHostsPath
}

// staticHostsReady reports whether the intranet name resolves without this
// product's DNS listener. Only macOS can answer that from a file; Windows always
// uses its hosts file, so its answer needs the entry and a passing lookup.
func (n *Native) staticHostsReady() bool {
	if n.staticHostsProbe != nil {
		return n.staticHostsProbe()
	}
	hostsPath := n.hostsPath
	if hostsPath == "" {
		if runtime.GOOS == "windows" {
			hostsPath = byokdns.DefaultHostsPath(nil)
		} else {
			hostsPath = byokdns.DarwinHostsPath
		}
	}
	return staticHostsAvailable(hostsPath, n.darwinResolverPath(), n.probe)
}

func staticHostsAvailable(hostsPath, resolverPath string, probe *byokdns.Probe) bool {
	switch runtime.GOOS {
	case "darwin":
		if !byokdns.HasManagedHostsEntry(hostsPath) {
			return false
		}
		if resolverPath == "" {
			resolverPath = "/etc/resolver/" + byokdns.Domain
		}
		_, err := os.Lstat(resolverPath)
		return errors.Is(err, os.ErrNotExist)
	case "windows":
		if hostsPath == "" {
			hostsPath = byokdns.DefaultHostsPath(nil)
		}
		if !byokdns.HasManagedHostsEntry(hostsPath) || probe == nil {
			return false
		}
		return probe.DNS(context.Background()).Passed
	}
	return true
}

// webToolsEnabled reports whether either managed web service is on. It reads
// the view the policy layer already produced, so a state poll does not read the
// same intent file twice.
func webToolsEnabled(search core.Document) bool {
	rows := core.Object(search["services"])
	for _, name := range []string{"search", "fetch"} {
		if core.Object(rows[name])["enabled"] == true {
			return true
		}
	}
	return false
}

// directBypassedFeatures lists the gateway features an active Direct Mode
// installation no longer has, in the order the panel shows them.
func (n *Native) directBypassedFeatures(d, search core.Document) []string {
	bypassed := []string{}
	if !n.isDirectMode() {
		return bypassed
	}
	if core.Object(d["budget"])["enabled"] == true {
		bypassed = append(bypassed, "budget")
	}
	for _, value := range core.Rows(d["models"]) {
		if len(core.Rows(core.Object(value)["failover"])) > 0 {
			bypassed = append(bypassed, "failover")
			break
		}
	}
	if n.service != nil && n.service.StateDir != "" {
		if text, err := core.ReadInstructions(filepath.Join(n.service.StateDir, "shared-instructions.md")); err == nil && text != "" {
			bypassed = append(bypassed, "shared_instructions")
		}
	}
	if webToolsEnabled(search) {
		bypassed = append(bypassed, "web_tools")
	}
	return bypassed
}

// directSupported reports whether this installation can carry a Direct Mode
// import at all: a 3.2.28 account store that publishes `byokSources.list`. An
// unknown answer stays true, because a check that could not run is not evidence
// of a missing surface.
func directSupported(report kimi.CompatibilityReport) bool {
	if report.Status == "" || report.Status == kimi.CompatStatusChecking {
		return true
	}
	if report.Checks.StoreFormat.Format == kimi.StoreFormatProfiles {
		return false
	}
	api := report.Checks.AccountAPI
	if api.Status == "" || api.Status == kimi.CheckStatusUnavailable || api.Status == kimi.CheckStatusAbsent {
		return true
	}
	return api.ByokSourcesList
}

// directModeRow is the exact JSON shape `/api/state` publishes. Keeping it one
// function is what makes the native and fallback answers agree.
func directModeRow(mode string, supported, staticHostsReady bool, bypassed []string, integration core.Document, plan directPlan) core.Document {
	if bypassed == nil {
		bypassed = []string{}
	}
	receipt := core.Object(integration["nativeImport"])
	routeCleanupPending := integration["routeCleanupPending"] == true ||
		receipt["routeCleanupPending"] == true || receipt["reason"] == "route_cleanup_pending"
	ready := mode == core.IntegrationModeDirect &&
		core.String(integration["mode"]) == kimi.IntegrationModeDirect &&
		core.String(integration["state"]) == kimi.IntegrationReady &&
		integration["adopted"] == true && !routeCleanupPending &&
		staticHostsReady
	eligibleModelIDs := make([]string, 0, plan.eligible())
	for _, provider := range plan.providers {
		for _, model := range provider.models {
			eligibleModelIDs = append(eligibleModelIDs, model.ID)
		}
	}
	return core.Document{
		"mode":                   mode,
		"supported":              supported,
		"staticHostsReady":       staticHostsReady,
		"routeCleanupPending":    routeCleanupPending,
		"eligibleModelIds":       eligibleModelIDs,
		"eligibleModels":         plan.eligible(),
		"totalModels":            plan.total,
		"skippedModels":          plan.skippedRows(),
		"activeFeaturesBypassed": bypassed,
		"readyForZeroGateway":    ready,
	}
}

// directModeState is the native answer: it adds what only a live installation
// can know (compatibility, hosts, enabled features) to the document translation.
func (n *Native) directModeState(d, integration, search core.Document) core.Document {
	plan := buildDirectPlan(d)
	if n.isDirectMode() {
		plan = buildDirectPlan(d, n.credentialAvailability(d))
	}
	n.mu.Lock()
	report := n.compatibility
	n.mu.Unlock()
	return directModeRow(core.IntegrationMode(d), directSupported(report), n.staticHostsReady(),
		n.directBypassedFeatures(d, search), integration, plan)
}

// directModeBase is the answer before a native layer exists. It makes no claim
// that needs a live check: unsupported is not asserted, and a static hosts entry
// is only claimed on a platform that does not need one.
func directModeBase(d core.Document) core.Document {
	return directModeRow(core.IntegrationMode(d), true, runtime.GOOS != "darwin" && runtime.GOOS != "windows",
		nil, core.Document{}, buildDirectPlan(d))
}

func (n *Native) credentialAvailability(d core.Document) map[string]bool {
	available := map[string]bool{}
	if n == nil || n.service == nil {
		return available
	}
	for _, value := range core.Rows(d["connections"]) {
		connection := core.Object(value)
		id := core.String(connection["id"])
		if id == "" {
			continue
		}
		key, err := credentialFor(connection, n.service.Keys)
		available[id] = err == nil && key != ""
	}
	return available
}

// cleanupOrphanedLegacy removes the dead `byok-kwp-*` providers an earlier
// installation left behind. It is best effort: a store this product cannot read
// is not a reason to fail an import.
func (n *Native) cleanupOrphanedLegacy() {
	if n == nil || n.controller == nil {
		return
	}
	home, err := n.controller.WorkHome()
	if err != nil || home == "" {
		return
	}
	_, _ = kimi.CleanupOrphanedLegacyEntries(home)
}

// directImportFor reads the live document and assembles the request. The
// document is re-read here rather than taken from an earlier snapshot, because a
// save could have landed while the operation waited for its deadline.
func (n *Native) directImportFor() (kimi.DirectImportRequest, error) {
	if n.service == nil || n.service.Store == nil {
		return kimi.DirectImportRequest{}, errors.New("configuration_unavailable")
	}
	d, _, err := n.service.Store.Read()
	if err != nil {
		return kimi.DirectImportRequest{}, err
	}
	request, _, err := directImportRequest(d, n.service.Keys, getVisibleCredential)
	if err != nil {
		return kimi.DirectImportRequest{}, err
	}
	return request, nil
}
