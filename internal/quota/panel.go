package quota

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// PanelActionError is a refusal the panel can show. `Status` is the HTTP status
// the legacy panel answered with, and `Code` is the identifier its error
// envelope carried, so a bridge can pass both straight through.
type PanelActionError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error. The code is the bare identifier, matching the other
// error types in this package.
func (e *PanelActionError) Error() string { return e.Code }

func panelError(status int, code string) *PanelActionError {
	return &PanelActionError{Status: status, Code: code}
}

// Panel paths the quota page uses. They are the legacy controller's own routes,
// kept so an existing UI keeps working without a second translation table.
const (
	PanelPathState   = "/api/quota"
	PanelPathRefresh = "/api/quota/refresh"
	PanelPathSources = "/api/quota/sources"
)

var (
	panelSourceID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	panelAdapters = map[string]bool{"custom": true, "deepseek": true, "moonshot-cn": true,
		"moonshot-global": true}
	panelCustomKinds = map[string]bool{"api_balance": true, "coding_plan": true, "gateway_limit": true}
	panelAuthTypes   = map[string]bool{"bearer": true, "x-api-key": true}
)

// PanelAction answers one quota request from the existing panel, in the shape
// its JSON contract expects. `connections` is the caller's own connection list,
// which is the only thing this package cannot know.
//
// It is a thin adapter, not a second implementation: every decision it makes is
// the legacy controller's own input semantics, and the actual work is delegated
// to Refresh, AddSource and RemoveSource.
//
//	GET  /api/quota           -> PanelState
//	POST /api/quota/refresh   -> PanelState + lastRefresh
//	POST /api/quota/sources   -> PanelState (upsert or remove)
//
// The four rules worth knowing at the call site:
//
//   - `sourceIds` omitted means every source; an empty list means none. A
//     caller that sends `[]` is asking for no work, not for all work.
//   - `force` defaults to false, so a second click inside the throttle window is
//     answered as `throttled` rather than sent again.
//   - A source bound to a user connection may be removed, including vendor
//     adapters. Product defaults without a user connection are retained.
//   - The panel names its credential by connection, not by reference: the body
//     carries `auth.connectionId` and this method turns it into the
//     `connection:<id>` reference AddSource stores.
func (a *Accounts) PanelAction(ctx context.Context, path string, input map[string]any,
	connections []ConnectionView) (map[string]any, error) {
	switch path {
	case PanelPathState, "":
		return a.PanelState(connections), nil
	case PanelPathRefresh:
		return a.panelRefresh(ctx, input, connections)
	case PanelPathSources:
		return a.panelSources(input, connections)
	}
	return nil, panelError(404, "not_found")
}

func (a *Accounts) panelRefresh(ctx context.Context, input map[string]any,
	connections []ConnectionView) (map[string]any, error) {
	if input == nil {
		return nil, panelError(400, "invalid_quota_refresh")
	}
	var sourceIDs []string
	if raw, ok := input["sourceIds"]; ok && raw != nil {
		rows, ok := raw.([]any)
		if !ok {
			return nil, panelError(400, "invalid_quota_refresh")
		}
		if len(rows) > maxSources {
			return nil, panelError(400, "invalid_quota_refresh")
		}
		// An explicit empty list selects nothing, which is different from an
		// omitted one; `sourceIDs` stays non-nil so Refresh sees the difference.
		sourceIDs = make([]string, 0, len(rows))
		for _, row := range rows {
			id, ok := row.(string)
			if !ok || !panelSourceID.MatchString(id) {
				return nil, panelError(400, "invalid_quota_refresh")
			}
			sourceIDs = append(sourceIDs, id)
		}
	}
	force := false
	if raw, ok := input["force"]; ok && raw != nil {
		value, ok := raw.(bool)
		if !ok {
			return nil, panelError(400, "invalid_quota_refresh")
		}
		force = value
	}
	report := a.Refresh(ctx, sourceIDs, force)
	state := a.PanelState(connections)
	state["lastRefresh"] = report
	return state, nil
}

func (a *Accounts) panelSources(input map[string]any,
	connections []ConnectionView) (map[string]any, error) {
	if input == nil {
		return nil, panelError(400, "invalid_quota_source")
	}
	switch stringOf(input["action"]) {
	case "remove":
		sourceID, ok := input["sourceId"].(string)
		if !ok {
			return nil, panelError(400, "invalid_quota_source")
		}
		source := a.findSource(sourceID)
		if source == nil || !strings.HasPrefix(source.CredentialRef, "connection:") {
			return nil, panelError(409, "quota_source_not_removable")
		}
		a.RemoveSource(sourceID)
		return a.PanelState(connections), nil
	case "upsert":
		value, ok := input["source"].(map[string]any)
		if !ok {
			return nil, panelError(400, "invalid_quota_source")
		}
		applied, err := a.upsertFromPanel(value, connections)
		if err != nil {
			return nil, err
		}
		if !applied {
			// An unchanged preset is answered with the current state rather than
			// rewritten, so a repeated save does not churn the store.
			return a.PanelState(connections), nil
		}
		return a.PanelState(connections), nil
	}
	return nil, panelError(400, "invalid_quota_source")
}

// upsertFromPanel turns one panel source body into the shape AddSource stores.
// It reports whether anything was written.
func (a *Accounts) upsertFromPanel(value map[string]any, connections []ConnectionView) (bool, error) {
	adapter := stringOf(value["adapter"])
	sourceID := stringOf(value["sourceId"])
	sourceName := stringOf(value["sourceName"])
	auth, _ := value["auth"].(map[string]any)
	connectionID := stringOf(auth["connectionId"])
	if !panelAdapters[adapter] || sourceID == "" || sourceName == "" || auth == nil || connectionID == "" {
		return false, panelError(400, "invalid_quota_source")
	}
	connection := findConnection(connections, connectionID)
	if connection == nil {
		return false, panelError(409, "quota_credential_connection_unavailable")
	}
	credentialRef := "connection:" + connection.ID
	if adapter != "custom" {
		if existing := a.findSource(sourceID); existing != nil &&
			existing.Adapter == adapter && existing.CredentialRef == credentialRef {
			return false, nil
		}
		_, err := a.AddSource(map[string]any{"adapter": adapter, "sourceId": sourceID,
			"sourceName": sourceName, "credentialRef": credentialRef})
		if err != nil {
			return false, translateSourceError(err)
		}
		return true, nil
	}
	kind := stringOf(value["kind"])
	authType := stringOf(auth["type"])
	valuePath := stringOf(value["valuePath"])
	if !panelCustomKinds[kind] || !panelAuthTypes[authType] || valuePath == "" {
		return false, panelError(400, "invalid_quota_source")
	}
	providerID := "custom"
	if connection.ProviderPreset != nil && *connection.ProviderPreset != "" {
		providerID = *connection.ProviderPreset
	}
	fields := map[string]any{"remaining": valuePath}
	for _, pair := range [][2]string{{"usedPath", "used"}, {"limitPath", "limit"}, {"resetAtPath", "resetAt"}} {
		if text := stringOf(value[pair[0]]); text != "" {
			fields[pair[1]] = text
		}
	}
	source := map[string]any{"adapter": "custom", "sourceId": sourceID, "sourceName": sourceName,
		"providerId": providerID, "kind": kind, "unit": value["unit"],
		"endpoint": value["endpoint"], "method": value["method"], "body": value["body"],
		"credentialRef": credentialRef, "windowLabel": value["windowLabel"], "fields": fields}
	if authType == "bearer" {
		source["auth"] = map[string]any{"mode": "bearer"}
	} else {
		source["auth"] = map[string]any{"mode": "header", "headerName": "x-api-key"}
	}
	if _, err := a.AddSource(source); err != nil {
		return false, translateSourceError(err)
	}
	return true, nil
}

// translateSourceError keeps the panel's status convention: a validation
// failure is a 400 with the code the store produced, anything else is the code
// as-is.
func translateSourceError(err error) error {
	var source *SourceError
	if ok := asSourceError(err, &source); ok {
		return panelError(400, source.Code)
	}
	return panelError(400, "invalid_quota_source")
}

func asSourceError(err error, target **SourceError) bool {
	if typed, ok := err.(*SourceError); ok {
		*target = typed
		return true
	}
	return false
}

func (a *Accounts) findSource(sourceID string) *Source {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.sources {
		if a.sources[i].SourceID == sourceID {
			copied := a.sources[i]
			return &copied
		}
	}
	return nil
}

func findConnection(connections []ConnectionView, id string) *ConnectionView {
	for i := range connections {
		if connections[i].ID == id {
			return &connections[i]
		}
	}
	return nil
}

// PanelActionFromJSON is the convenience form a bridge uses when it already has
// the request body as bytes. An empty body decodes as a nil input, which the
// GET path accepts and the POST paths refuse.
func (a *Accounts) PanelActionFromJSON(ctx context.Context, path string, body []byte,
	connections []ConnectionView) (map[string]any, error) {
	var input map[string]any
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			return nil, panelError(400, panelPathErrorCode(path))
		}
	}
	return a.PanelAction(ctx, path, input, connections)
}

func panelPathErrorCode(path string) string {
	switch path {
	case PanelPathRefresh:
		return "invalid_quota_refresh"
	case PanelPathSources:
		return "invalid_quota_source"
	}
	return "invalid_quota_source"
}
