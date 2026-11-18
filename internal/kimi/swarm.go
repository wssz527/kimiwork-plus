package kimi

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Swarm mode is a per-conversation setting the desktop renderer owns. The panel
// drives the official control-plane RPCs (app.swarmMode.get / app.swarmMode.set)
// rather than editing conversation-models.json, so the page's revision counter
// and its app.swarmMode.changed broadcast stay authoritative.

const (
	SwarmStateOn      = "on"
	SwarmStateOff     = "off"
	SwarmStateUnknown = "unknown"
)

// The official code travels separately in UpstreamCode, so a build whose
// vocabulary changes can never fake one of these reasons.
const (
	SwarmErrUnavailable             = "work_swarm_unavailable"
	SwarmErrUnsupported             = "work_swarm_unsupported"
	SwarmErrPageUnavailable         = "work_swarm_page_unavailable"
	SwarmErrConversationUnavailable = "work_swarm_conversation_unavailable"
	SwarmErrContractChanged         = "work_swarm_contract_changed"
	SwarmErrRPCFailed               = "work_swarm_rpc_failed"
	SwarmErrNotConfirmed            = "work_swarm_not_confirmed"
	SwarmErrConversationChanged     = "work_swarm_conversation_changed"
)

// SwarmSnapshot is one conversation's swarm mode as the page reports it. It
// carries no conversation content, account identity or credential.
type SwarmSnapshot struct {
	State           string `json:"state"`
	ConversationKey string `json:"conversationKey,omitempty"`
	Enabled         bool   `json:"enabled"`
	// A changed epoch invalidates every revision cached under the previous one.
	Epoch    string `json:"epoch,omitempty"`
	Revision int    `json:"revision"`
	// Confirmed is set only by a write whose readback agreed; a read never sets it.
	Confirmed    bool   `json:"confirmed"`
	Reason       string `json:"reason,omitempty"`
	UpstreamCode string `json:"upstreamCode,omitempty"`
}

// swarmSurface is separate from PageActions so that contract, and every fake
// built on it, stays unchanged.
type swarmSurface interface {
	swarmConversationKey(ctx context.Context) (string, error)
	swarmGet(ctx context.Context, conversationKey string) (SwarmSnapshot, error)
	swarmSet(ctx context.Context, conversationKey string, enabled bool) (SwarmSnapshot, error)
}

// swarmModeMu is not the lifecycle lock: a Stop during a page call ends the pipe
// and surfaces as an error instead of serialising behind that call.
var swarmModeMu sync.Mutex

// SwarmMode reads the active conversation's swarm mode and writes nothing.
func (c *Controller) SwarmMode(ctx context.Context) (SwarmSnapshot, error) {
	swarmModeMu.Lock()
	defer swarmModeMu.Unlock()

	surface, err := c.swarmSurface()
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: err.Error()}, err
	}
	key, err := surface.swarmConversationKey(ctx)
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: err.Error()}, err
	}
	return surface.swarmGet(ctx, key)
}

// SetSwarmMode writes swarm mode for the active conversation and confirms it by
// reading back through the same key. `enabled` is explicit in both directions,
// so a caller that lost track of the value cannot flip it by accident.
//
// The RPC is scoped to a key, so a write still succeeds after the user leaves
// that conversation, and a readback keyed by that write always answers with it.
// Confirmed therefore also requires the page to still show the same conversation
// at the moment of confirmation, and the same epoch the write was stamped with.
func (c *Controller) SetSwarmMode(ctx context.Context, expectedKey string, enabled bool) (SwarmSnapshot, error) {
	swarmModeMu.Lock()
	defer swarmModeMu.Unlock()

	surface, err := c.swarmSurface()
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: err.Error()}, err
	}
	key, err := surface.swarmConversationKey(ctx)
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: err.Error()}, err
	}
	if key != expectedKey {
		return swarmUnconfirmed(SwarmSnapshot{State: SwarmStateUnknown}, SwarmErrConversationChanged)
	}
	written, err := surface.swarmSet(ctx, expectedKey, enabled)
	if err != nil {
		return swarmAfterFailedWrite(ctx, surface, key, written), err
	}
	readback, err := surface.swarmGet(ctx, key)
	if err != nil {
		return readback, err
	}
	if current, keyErr := surface.swarmConversationKey(ctx); keyErr != nil || current != key {
		return swarmUnconfirmed(readback, SwarmErrConversationChanged)
	}
	if readback.ConversationKey != key {
		return swarmUnconfirmed(readback, SwarmErrConversationChanged)
	}
	// An empty written epoch means the page had not stamped the conversation;
	// only a real stamp can be compared.
	if readback.Enabled != enabled || (written.Epoch != "" && readback.Epoch != written.Epoch) {
		return swarmUnconfirmed(readback, SwarmErrNotConfirmed)
	}
	readback.Confirmed = true
	return readback, nil
}

func swarmUnconfirmed(snapshot SwarmSnapshot, reason string) (SwarmSnapshot, error) {
	snapshot.Reason = reason
	snapshot.Confirmed = false
	return snapshot, errors.New(reason)
}

// swarmAfterFailedWrite reports what the conversation holds after a write that
// failed, keeping the write's own reason and upstream code.
func swarmAfterFailedWrite(ctx context.Context, surface swarmSurface, key string, written SwarmSnapshot) SwarmSnapshot {
	written.Confirmed = false
	if written.Reason == "" {
		written.Reason = SwarmErrRPCFailed
	}
	readback, err := surface.swarmGet(ctx, key)
	if err != nil || readback.ConversationKey != key {
		return written
	}
	readback.Reason = written.Reason
	readback.UpstreamCode = written.UpstreamCode
	readback.Confirmed = false
	return readback
}

func (c *Controller) swarmSurface() (swarmSurface, error) {
	c.mu.Lock()
	actions := c.actions
	child := c.child
	c.mu.Unlock()
	if actions == nil {
		return nil, errors.New(SwarmErrUnavailable)
	}
	if child != nil {
		select {
		case <-child.Exit():
			return nil, errors.New(SwarmErrUnavailable)
		default:
		}
	}
	surface, ok := actions.(swarmSurface)
	if !ok {
		return nil, errors.New(SwarmErrUnsupported)
	}
	return surface, nil
}

// swarmTarget is the Work view from the same allowlist every page action uses.
func (a *cdpActions) swarmTarget(ctx context.Context) (actionTarget, error) {
	targets, err := a.targets(ctx)
	if err != nil {
		return actionTarget{}, errors.New(SwarmErrUnavailable)
	}
	target, ok := findPath(targets, "/kimi-agent.html")
	if !ok {
		return actionTarget{}, errors.New(SwarmErrPageUnavailable)
	}
	return target, nil
}

func (a *cdpActions) swarmConversationKey(ctx context.Context) (string, error) {
	target, err := a.swarmTarget(ctx)
	if err != nil {
		return "", err
	}
	value, err := a.evaluate(ctx, target.sessionID, swarmConversationKeyExpression)
	if err != nil {
		return "", errors.New(SwarmErrPageUnavailable)
	}
	key, _ := value.(string)
	if !ValidSwarmConversationKey(key) {
		return "", errors.New(SwarmErrConversationUnavailable)
	}
	return key, nil
}

func (a *cdpActions) swarmGet(ctx context.Context, conversationKey string) (SwarmSnapshot, error) {
	return a.swarmCall(ctx, swarmMethodGet, conversationKey, nil)
}

func (a *cdpActions) swarmSet(ctx context.Context, conversationKey string, enabled bool) (SwarmSnapshot, error) {
	return a.swarmCall(ctx, swarmMethodSet, conversationKey, &enabled)
}

func (a *cdpActions) swarmCall(ctx context.Context, method, conversationKey string, enabled *bool) (SwarmSnapshot, error) {
	if !ValidSwarmConversationKey(conversationKey) {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrConversationUnavailable},
			errors.New(SwarmErrConversationUnavailable)
	}
	target, err := a.swarmTarget(ctx)
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: err.Error()}, err
	}
	value, err := a.evaluate(ctx, target.sessionID, swarmRPCExpression(method, conversationKey, enabled))
	if err != nil {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrPageUnavailable},
			errors.New(SwarmErrPageUnavailable)
	}
	return parseSwarmAnswer(value, conversationKey)
}

const (
	swarmMethodGet = "app.swarmMode.get"
	swarmMethodSet = "app.swarmMode.set"
)

// swarmConversationKeyExpression reads the active row's key. Nothing else on the
// page exposes it, so a page without these rows answers null and the caller
// reports the conversation as unavailable instead of guessing one.
const swarmConversationKeyExpression = `(function(){
try{
var doc=globalThis.document;
if(!doc||typeof doc.querySelector!=="function")return null;
var el=doc.querySelector('[data-testid="sidebar-conversation"][data-conversation-key].active')??doc.querySelector('.conversation-row.active[data-conversation-key]');
if(!el||typeof el.getAttribute!=="function")return null;
var key=el.getAttribute("data-conversation-key");
if(typeof key!=="string")return null;
var trimmed=key.trim();
return trimmed.length>0?trimmed:null;
}catch(error){return null;}
})()`

// swarmRPCExpression unwraps the envelope inside the page and returns only
// whitelisted fields: the official error message and the rest of `data` never
// leave the renderer. Params carry exactly the keys the official validator
// accepts, because it compares the key set for equality.
func swarmRPCExpression(method, conversationKey string, enabled *bool) string {
	params := `{conversationKey:` + quoteJS(conversationKey)
	if enabled != nil {
		params += `,enabled:` + strconv.FormatBool(*enabled)
	}
	params += `}`
	return `(async()=>{` +
		`var cp=globalThis.controlPlane;` +
		`if(!cp||typeof cp.rpc!=="function")return{status:"no-control-plane"};` +
		`var envelope;` +
		`try{envelope=await cp.rpc({method:` + quoteJS(method) + `,params:` + params + `});}` +
		`catch(error){return{status:"rpc-threw",code:(error&&typeof error.code==="string")?error.code:""};}` +
		`if(!envelope||envelope.ok!==true){` +
		`var err=envelope&&envelope.error;` +
		`var data=err&&err.data;` +
		`return{status:"rpc-failed",` +
		`code:(err&&err.code!==undefined)?String(err.code):"",` +
		`reasonCode:(data&&typeof data.reasonCode==="string")?data.reasonCode:""};}` +
		`var result=envelope.result;` +
		`return{status:"ok",` +
		`conversationKey:(result&&typeof result.conversationKey==="string")?result.conversationKey:"",` +
		`enabled:(result&&typeof result.enabled==="boolean")?result.enabled:null,` +
		`epoch:(result&&typeof result.epoch==="string")?result.epoch:"",` +
		`revision:(result&&typeof result.revision==="number"&&Number.isSafeInteger(result.revision))?result.revision:-1};` +
		`})()`
}

// parseSwarmAnswer requires the whole official snapshot shape: a missing field
// must never be read as "disabled".
func parseSwarmAnswer(value any, wantKey string) (SwarmSnapshot, error) {
	answer, ok := value.(map[string]any)
	if !ok {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrContractChanged},
			errors.New(SwarmErrContractChanged)
	}
	switch stringField(answer, "status") {
	case "no-control-plane":
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrContractChanged},
			errors.New(SwarmErrContractChanged)
	case "rpc-threw", "rpc-failed":
		code := stringField(answer, "code")
		if code == "" {
			code = stringField(answer, "reasonCode")
		}
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrRPCFailed,
			ConversationKey: wantKey, UpstreamCode: safeSwarmCode(code)}, errors.New(SwarmErrRPCFailed)
	case "ok":
	default:
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrContractChanged},
			errors.New(SwarmErrContractChanged)
	}

	key, _ := answer["conversationKey"].(string)
	enabled, enabledOK := answer["enabled"].(bool)
	epoch, epochOK := answer["epoch"].(string)
	revision, revisionOK := answer["revision"].(float64)
	// The epoch must be present but may be empty: a page that has not stamped
	// this conversation reports an empty one, which only means no revision can
	// be cached against an epoch.
	if !enabledOK || !revisionOK || !ValidSwarmConversationKey(key) || key != wantKey || !epochOK ||
		!swarmRevisionOK(revision) {
		return SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrContractChanged,
			ConversationKey: key}, errors.New(SwarmErrContractChanged)
	}
	snapshot := SwarmSnapshot{ConversationKey: key, Enabled: enabled, Epoch: epoch,
		Revision: int(revision), State: SwarmStateOff}
	if enabled {
		snapshot.State = SwarmStateOn
	}
	return snapshot, nil
}

func swarmRevisionOK(value float64) bool {
	return value >= 0 && value <= 9007199254740991 && math.Trunc(value) == value
}

// ValidSwarmConversationKey accepts what the official validator accepts, trimmed and
// non-empty, plus a ceiling because the key is embedded in a page expression.
func ValidSwarmConversationKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.TrimSpace(key) != key {
		return false
	}
	for _, character := range key {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func safeSwarmCode(value string) string {
	var out strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_', character == '.', character == ':':
			out.WriteRune(character)
		}
		if out.Len() >= 64 {
			break
		}
	}
	return out.String()
}
