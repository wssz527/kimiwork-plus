package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

type nativePolicies struct {
	mu           sync.Mutex
	dir          string
	home         string
	subagents    *kimi.SubagentConfig
	context      *kimi.ContextPolicyConfig
	services     *kimi.ServicesConfig
	configured   core.Document
	capabilities kimi.SubagentCapabilities
}

func newNativePolicies(dir, home string) (*nativePolicies, error) {
	p := &nativePolicies{dir: dir, home: home}
	var err error
	p.subagents, err = kimi.NewSubagentConfig(dir, home)
	if err != nil {
		return nil, err
	}
	p.context, err = kimi.NewContextPolicyConfig(dir, home)
	if err != nil {
		return nil, err
	}
	p.services, err = kimi.NewServicesConfig(dir, home)
	return p, err
}

func (p *nativePolicies) setHome(home string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.home == home {
		return false, nil
	}
	subagents, err := kimi.NewSubagentConfig(p.dir, home)
	if err != nil {
		return false, err
	}
	contextPolicy, err := kimi.NewContextPolicyConfig(p.dir, home)
	if err != nil {
		return false, err
	}
	services, err := kimi.NewServicesConfig(p.dir, home)
	if err != nil {
		return false, err
	}
	p.home = home
	p.subagents = subagents
	p.context = contextPolicy
	p.services = services
	return true, nil
}
func decodeStrict(in any, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func nativeSubagentPolicy(d core.Document) kimi.SubagentPolicy {
	p := kimi.SubagentPolicy{Mode: core.String(d["mode"]), ModelAlias: core.String(d["modelAlias"]), Effort: core.String(d["effort"])}
	for _, v := range core.Rows(d["models"]) {
		row := core.Object(v)
		p.Models = append(p.Models, kimi.SubagentPoolEntry{ModelAlias: core.String(row["modelAlias"]), Description: core.String(row["description"])})
	}
	return p
}
func (p *nativePolicies) Configure(d core.Document) error {
	normalized, err := core.NormalizeSubagents(d["subagents"], core.Rows(d["models"]))
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configured = normalized
	p.subagents.Update(nativeSubagentPolicy(normalized), p.capabilities)
	if value := d["context"]; value != nil {
		var policy kimi.ContextPolicy
		if decodeStrict(value, &policy) != nil {
			return errors.New("invalid_context_policy")
		}
		p.context.Update(policy)
	}
	return nil
}
func (p *nativePolicies) State(d core.Document) core.Document {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := p.subagents.Inspect()
	binding := p.subagents.Binding(nativeSubagentPolicy(p.configured), false)
	return core.Document{"subagents": core.Document{"configured": p.configured, "policy": status, "runtime": binding.Runtime}, "context": p.contextView(d), "search": p.servicesView()}
}
func (p *nativePolicies) contextView(d core.Document) core.Document {
	status := p.context.State()
	policy := d["context"]
	if policy == nil {
		policy = core.Document{"mode": "inherit"}
	}
	return core.Document{"policy": policy, "status": status.State, "reason": status.Reason, "observed": status.Observed}
}
func (p *nativePolicies) servicesView() core.Document {
	status := p.services.State()
	intent := p.services.Intent()
	rows := core.Document{}
	for name, pair := range map[string]struct {
		intent *kimi.ServiceSettings
		actual *kimi.ServiceOutcome
	}{"search": {intent.Search, status.Search}, "fetch": {intent.Fetch, status.Fetch}} {
		row := core.Document{"enabled": false, "endpoint": "", "keySet": false, "status": status.State, "reason": status.Reason}
		if pair.intent != nil {
			row["enabled"] = pair.intent.Enabled
			row["endpoint"] = pair.intent.BaseURL
			row["keySet"] = pair.intent.APIKey != ""
		}
		if pair.actual != nil {
			row["status"] = pair.actual.State
			row["reason"] = pair.actual.Reason
		}
		rows[name] = row
	}
	return core.Document{"status": status.State, "reason": status.Reason, "services": rows}
}
func (s *Service) saveNativeContext(p *nativePolicies, in core.Document) (any, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if services, exists := in["services"]; exists {
		if _, mixed := in["policy"]; mixed {
			return nil, errors.New("invalid_context_policy")
		}
		var input kimi.ServicesInput
		if core.Object(services) == nil || decodeStrict(services, &input) != nil {
			return nil, errors.New("invalid_services")
		}
		result := p.services.Update(input)
		if result.State == kimi.NativeStateConflict {
			return nil, errors.New(result.Reason)
		}
		return core.Document{"search": p.servicesView()}, nil
	}
	var policy kimi.ContextPolicy
	if core.Object(in["policy"]) == nil || decodeStrict(in["policy"], &policy) != nil {
		return nil, errors.New("invalid_context_policy")
	}
	result := p.context.Update(policy)
	if result.State == kimi.NativeStateConflict && result.Reason == "invalid_policy" {
		return nil, errors.New("invalid_context_policy")
	}
	if result.State == kimi.NativeStateConflict {
		return core.Document{"context": core.Document{"policy": policy, "status": result.State, "reason": result.Reason}}, nil
	}
	_, rev, err := s.Store.Read()
	if err != nil {
		return nil, err
	}
	d, _, err := s.Store.Update(rev, func(d core.Document) error {
		raw, _ := json.Marshal(policy)
		var value any
		_ = json.Unmarshal(raw, &value)
		d["context"] = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return core.Document{"context": p.contextView(d)}, nil
}
