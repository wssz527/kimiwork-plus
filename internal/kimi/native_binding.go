package kimi

import "context"

// The panel renders three layers and never collapses them:
//
//	configured  what the user saved
//	policy      what is in the file (or the ownership state)
//	runtime     whether the kernel actually adopted it
//
// The released UI's own vocabulary is reproduced here, because a caller that
// substituted `file-applied` for "adopted" would tell the user a policy was live
// when only a file changed. `Observed` is the one value that may raise a policy
// above `file-applied`, and it is only set when a real tool definition carried
// the pool structure — which this build cannot observe without a live kernel
// session, so it stays false unless a caller supplies evidence.

// SubagentConfigured is the saved target in the shape the panel reads.
//
// The panel accepts `model` or `modelAlias` and prefers `model`; it reads pool
// rows as `{id, description}`. This type emits both aliases so an existing page
// keeps working without a translation step in the UI.
type SubagentConfigured struct {
	Mode       string                    `json:"mode"`
	Model      string                    `json:"model,omitempty"`
	ModelAlias string                    `json:"modelAlias,omitempty"`
	Effort     string                    `json:"effort,omitempty"`
	Models     []SubagentConfiguredModel `json:"models,omitempty"`
}

// SubagentConfiguredModel is one pool row in the panel's shape.
type SubagentConfiguredModel struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// SubagentBinding is the three-layer answer the panel renders.
type SubagentBinding struct {
	Configured SubagentConfigured `json:"configured"`
	Policy     SubagentStatus     `json:"policy"`
	Runtime    SubagentRuntime    `json:"runtime"`
}

// SubagentRuntime is the adoption layer.
type SubagentRuntime struct {
	State    string            `json:"state"`
	Reason   string            `json:"reason,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Observed *SubagentObserved `json:"observed,omitempty"`
}

// SubagentObserved is the one evidence that may raise the state to "loaded".
type SubagentObserved struct {
	State string `json:"state"`
}

// Runtime state values, matching the released panel's vocabulary.
const (
	RuntimeFileApplied    = "file-applied"
	RuntimeWaitingConfig  = "waiting-config"
	RuntimeConflict       = "conflict"
	RuntimeInherit        = "inherit"
	RuntimeRestored       = "restored"
	RuntimeUnavailable    = "unavailable"
	RuntimeObservedSchema = "observed-schema"
	RuntimeLoaded         = "loaded"
)

// ConfiguredFromPolicy renders a saved policy in the panel's shape.
func ConfiguredFromPolicy(policy SubagentPolicy) SubagentConfigured {
	configured := SubagentConfigured{Mode: policy.Mode}
	switch policy.Mode {
	case SubagentModeForce:
		configured.Model = policy.ModelAlias
		configured.ModelAlias = policy.ModelAlias
		configured.Effort = policy.Effort
	case SubagentModePool:
		configured.Model = policy.ModelAlias
		configured.ModelAlias = policy.ModelAlias
		configured.Models = make([]SubagentConfiguredModel, 0, len(policy.Models))
		for _, row := range policy.Models {
			configured.Models = append(configured.Models, SubagentConfiguredModel{
				ID: row.ModelAlias, Description: row.Description})
		}
	}
	return configured
}

// Binding composes the three layers.
//
// `observed` is the caller's evidence that the kernel's tool definition carries
// the pool structure. This build cannot produce it without a live session, so a
// caller that has none passes nil and the runtime stays at `file-applied`.
func (c *SubagentConfig) Binding(policy SubagentPolicy, observed bool) SubagentBinding {
	binding := SubagentBinding{Configured: ConfiguredFromPolicy(policy), Policy: c.Inspect()}
	binding.Runtime = runtimeFromStatus(binding.Policy, observed)
	return binding
}

// runtimeFromStatus maps a file status onto the adoption layer. The mapping is
// deliberately conservative: a written file is `file-applied`, never `loaded`.
func runtimeFromStatus(status SubagentStatus, observed bool) SubagentRuntime {
	runtime := SubagentRuntime{Mode: status.Mode, Reason: status.Reason}
	switch status.State {
	case NativeStateFileApplied:
		runtime.State = RuntimeFileApplied
		if observed {
			// Only real evidence raises this, and it still does not claim the
			// kernel picked the model — only that the tool structure updated.
			runtime.State = RuntimeLoaded
			runtime.Observed = &SubagentObserved{State: RuntimeObservedSchema}
		}
	case NativeStateWaiting:
		runtime.State = RuntimeWaitingConfig
	case NativeStateConflict:
		runtime.State = RuntimeConflict
	case NativeStateInherit, SubagentModeBlocked:
		runtime.State = RuntimeInherit
	case NativeStateRestored:
		runtime.State = RuntimeRestored
	default:
		runtime.State = RuntimeUnavailable
	}
	return runtime
}

// ---------------------------------------------------------------------------
// context and services: the panel's shape

// ContextBinding is the two-layer answer for the context policy: the saved
// request and what the file holds.
//
// This is an intermediate structure for `internal/bridge/policies.go` to map
// onto the panel document, not the panel's own contract. The released page reads
// `ctx.policy` as the request policy and `ctx.status` as a state string
// (`growth.js` renders it directly), so the bridge builds that shape; the file
// state here is a full status record because the caller needs its reason and
// layout too. There is no runtime layer, because the kernel reads these keys at
// start-up and a written value is the whole claim.
type ContextBinding struct {
	Policy ContextPolicy `json:"policy"`
	State  ContextStatus `json:"state"`
}

// Binding composes the saved request with the file state.
func (c *ContextPolicyConfig) Binding(policy ContextPolicy) ContextBinding {
	return ContextBinding{Policy: policy, State: c.State()}
}

// ServicesBinding is the saved intent plus the per-service outcome. The panel
// renders each service separately, so the two are kept apart rather than
// flattened into one state.
//
// Like ContextBinding this is an intermediate structure: the panel reads
// `search.services[name]{enabled,endpoint,keySet,status}`, which the bridge
// derives from this, because the key itself must never leave this process.
type ServicesBinding struct {
	Intent ServicesInput  `json:"intent"`
	State  ServicesStatus `json:"state"`
}

// Binding composes the saved intent with the file state.
func (c *ServicesConfig) Binding() ServicesBinding {
	return ServicesBinding{Intent: c.Intent(), State: c.State()}
}

// MCPBinding is the redacted server list plus the probe availability. `Probe`
// reports whether this build can run a probe at all, so the panel can disable the
// button instead of showing a failure after a click.
type MCPBinding struct {
	Servers []MCPServerView `json:"servers"`
	Probe   MCPProbeStatus  `json:"probe"`
	State   string          `json:"state"`
	Reason  string          `json:"reason,omitempty"`
}

// MCPProbeStatus says whether a probe is possible here.
type MCPProbeStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Binding lists the servers and reports probe availability.
func (m *MCPManager) Binding(ctx context.Context) MCPBinding {
	status := m.List(ctx)
	binding := MCPBinding{Servers: status.Servers, State: status.State, Reason: status.Reason}
	if m.probeAvailable() {
		binding.Probe = MCPProbeStatus{Available: true}
		return binding
	}
	binding.Probe = MCPProbeStatus{Available: false, Reason: MCPTestUnavailableReason}
	return binding
}

// probeAvailable asks the transport once. A transport that answers
// ErrMCPTestUnavailable is a file store; anything else may be able to probe.
func (m *MCPManager) probeAvailable() bool {
	if probe, ok := m.transport.(interface{ ProbeAvailable() bool }); ok {
		return probe.ProbeAvailable()
	}
	return true
}

// ProbeAvailable reports false: a file store cannot run a probe.
func (s *MCPFileStore) ProbeAvailable() bool { return false }
