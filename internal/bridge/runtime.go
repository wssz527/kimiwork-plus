package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/ledger"
	"kimiworkplus.local/migration/internal/protocol"
	"kimiworkplus.local/migration/internal/quota"
)

type Runtime struct {
	mu               sync.RWMutex
	store            *core.Store
	keys             KeyStore
	token            string
	gateway          *protocol.Gateway
	active           core.Document
	usage            *ledger.Ledger
	quota            *quota.Accounts
	budget           *budgetGate
	instructionsPath string
}

func NewRuntime(dir string, store *core.Store, keys KeyStore) (*Runtime, error) {
	token, err := persistentToken(dir)
	if err != nil {
		return nil, err
	}
	usage, err := ledger.New(ledger.Options{File: filepath.Join(dir, "usage", "ledger.jsonl")})
	if err != nil {
		return nil, err
	}
	r := &Runtime{store: store, keys: keys, token: token, usage: usage, instructionsPath: filepath.Join(dir, "shared-instructions.md"),
		budget: newBudgetGate(usage, nil)}
	r.quota = quota.New(quota.Options{StateDir: filepath.Join(dir, "quota"), Credentials: r.credential})
	d, _, err := store.Read()
	if err != nil {
		return nil, err
	}
	if err = r.Apply(d); err != nil {
		return nil, err
	}
	return r, nil
}
func persistentToken(dir string) (string, error) {
	path := filepath.Join(dir, "gateway-token.json")
	b, e := core.ReadPrivateFile(path, 4096)
	if errors.Is(e, os.ErrNotExist) {
		token := RandomKey()
		return token, core.AtomicJSON(path, map[string]string{"token": token})
	}
	if e != nil {
		return "", e
	}
	var d map[string]string
	if json.Unmarshal(b, &d) != nil || len(d["token"]) != 64 {
		return "", errors.New("invalid_gateway_token")
	}
	return d["token"], nil
}
func (r *Runtime) Token() string { return r.token }
func (r *Runtime) credential(ref string) string {
	if r.keys == nil {
		return ""
	}
	d, _, e := r.store.Read()
	if e != nil {
		return ""
	}
	id := strings.TrimPrefix(ref, "connection:")
	for _, v := range core.Rows(d["connections"]) {
		c := core.Object(v)
		if c["id"] == id {
			key, _ := credentialFor(c, r.keys)
			return key
		}
	}
	return ""
}
func (r *Runtime) Apply(d core.Document) error {
	if e := core.Validate(d); e != nil {
		return e
	}
	models, e := protocol.ValidateModelsAny(core.Rows(d["models"]))
	if e != nil {
		return e
	}
	policy, e := protocol.ParseBudget(d["budget"])
	if e != nil {
		return e
	}
	prices, e := core.PriceTable(d)
	if e != nil {
		return e
	}
	endpoints := []protocol.Endpoint{}
	contexts := map[string]protocol.UsageContext{}
	for _, v := range core.Rows(d["connections"]) {
		c := core.Object(v)
		id := core.String(c["id"])
		key := ""
		if r.keys != nil {
			key, e = credentialFor(c, r.keys)
			if e != nil {
				continue
			}
		}
		if key == "" {
			continue
		}
		protocols := []string{}
		for _, p := range core.Rows(c["protocols"]) {
			protocols = append(protocols, core.String(p))
		}
		endpoints = append(endpoints, protocol.Endpoint{ID: id, BaseURL: core.String(c["baseURL"]), APIKey: key, Protocols: protocols})
		contexts[id] = protocol.UsageContext{ConnectionID: optional(id), ProviderID: optional(core.String(c["providerPresetId"])), Account: optional(id), APIKeyID: optional(key)}
	}
	backend, e := protocol.NewBackend(endpoints, models)
	if e != nil {
		return e
	}
	g, e := protocol.NewGateway(protocol.Config{Backend: backend, Key: r.token, InstructionsProvider: func() (string, error) { return core.ReadInstructions(r.instructionsPath) }, RecordRequests: true, UsageSink: protocol.UsageSinkFunc(r.record), Budget: r.budget, ResolveUsageContext: func(q protocol.UsageContextRequest) (protocol.UsageContext, error) {
		c := protocol.UsageContext{}
		if q.ConnectionID != nil {
			c = contexts[*q.ConnectionID]
		}
		session := q.Headers["x-session-id"]
		if session == "" {
			session = q.Headers["session_id"]
		}
		if session == "" {
			session = q.PromptCacheKey
		}
		c.ConversationID = optional(session)
		return c, nil
	}})
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(d)
	var active core.Document
	_ = json.Unmarshal(raw, &active)
	// The policy is applied together with the gateway, never before it: a
	// configuration that fails to build must leave the running policy untouched,
	// because a new limit with an old catalog would price the wrong models.
	r.budget.configure(policy, prices)
	r.mu.Lock()
	r.gateway = g
	r.active = active
	r.mu.Unlock()
	return nil
}
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func (r *Runtime) record(e protocol.UsageEntry) error {
	entry := ledger.Entry{OperationID: e.OperationID, Source: e.Source, At: e.At, Status: e.Status, ModelID: optional(e.ModelID), ModelAlias: optional(e.ModelAlias), ConnectionID: e.ConnectionID, ProviderID: e.ProviderID, Account: e.Account, APIKeyID: e.APIKeyID, ConversationID: e.ConversationID, Protocol: optional(e.Protocol), UpstreamProtocol: optional(e.UpstreamProtocol), LatencyMS: e.LatencyMS, ErrorCode: optional(e.ErrorCode), PriceSource: e.PriceSource, EstimatedCost: e.EstimatedCost}
	if e.HTTPStatus != nil {
		n := int64(*e.HTTPStatus)
		entry.HTTPStatus = &n
	}
	if e.Usage != nil {
		entry.Usage = map[string]any{}
		for k, v := range map[string]*int64{"inputTokens": e.Usage.InputTokens, "outputTokens": e.Usage.OutputTokens, "totalTokens": e.Usage.TotalTokens, "cachedInputTokens": e.Usage.CachedInputTokens, "cacheWriteTokens": e.Usage.CacheWriteTokens} {
			if v != nil {
				entry.Usage[k] = *v
			}
		}
	}
	r.usage.Record(entry)
	return nil
}
func (r *Runtime) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	r.mu.RLock()
	g := r.gateway
	r.mu.RUnlock()
	g.ServeHTTP(w, q)
}
func (r *Runtime) State() core.Document {
	r.mu.RLock()
	g, active := r.gateway, r.active
	r.mu.RUnlock()
	d, _, _ := r.store.Read()
	applied := core.Revision(d) == core.Revision(active)
	status := "applied"
	if !applied {
		status = "failed"
	}
	activeRows := connectionRows(active, r.keys)
	apply := core.Document{"state": status}
	for _, row := range activeRows {
		if core.Object(row)["credentialError"] != nil {
			apply["state"] = "failed"
			apply["reason"] = "credentials_unavailable"
		}
	}
	return core.Document{"metrics": g.Metrics(), "usage": r.usage.Status(), "budget": r.budget.budgetStatus(core.Rows(active["models"])), "connections": core.Document{"active": activeRows, "saved": connectionRows(d, r.keys), "restartRequired": !applied, "apply": apply}, "gateway": core.Document{"state": "running", "modelCount": len(core.Rows(active["models"])), "activeRequests": g.Inflight()}}
}
func (r *Runtime) Action(ctx context.Context, path string, in core.Document) (any, error) {
	switch path {
	case "/api/usage/query":
		raw, _ := json.Marshal(in)
		var q ledger.Query
		if json.Unmarshal(raw, &q) != nil {
			return nil, errors.New("invalid_usage_query")
		}
		q.Now = nil
		q.Unlimited = false
		return r.usage.Query(q)
	case "/api/usage/export":
		b, e := r.usage.ExportJSON(ledger.Query{})
		return json.RawMessage(b), e
	case "/api/usage/clear":
		if in["confirm"] != true {
			return nil, errors.New("invalid_confirmation")
		}
		return r.usage.Clear(in["before"], core.String(in["timeZone"])), nil
	case "/api/budget":
		return r.budgetView()
	case "/api/quota", "/api/quota/refresh", "/api/quota/sources":
		d, _, e := r.store.Read()
		if e != nil {
			return nil, e
		}
		connections := []quota.ConnectionView{}
		for _, v := range core.Rows(d["connections"]) {
			c := core.Object(v)
			key, _ := credentialFor(c, r.keys)
			connections = append(connections, quota.ConnectionView{ID: core.String(c["id"]), Name: core.String(c["name"]), ProviderPreset: optional(core.String(c["providerPresetId"])), HasCredential: key != ""})
		}
		return r.quota.PanelAction(ctx, path, map[string]any(in), connections)
	}
	return nil, errors.New("native_preview_unavailable")
}
