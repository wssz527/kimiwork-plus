package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// swarmFakePage answers Runtime.evaluate for the expressions swarm mode sends and
// records them, so the selectors, RPC names and params are part of the contract.
type swarmFakePage struct {
	mu sync.Mutex

	targets []map[string]any
	key     any
	get     any
	set     any

	expressions []string
	sessions    []string
}

func (p *swarmFakePage) answer(expression, sessionID string) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expressions = append(p.expressions, expression)
	p.sessions = append(p.sessions, sessionID)
	switch {
	case strings.Contains(expression, "sidebar-conversation"):
		return p.key
	case strings.Contains(expression, swarmMethodSet):
		return p.set
	case strings.Contains(expression, swarmMethodGet):
		return p.get
	}
	return nil
}

func (p *swarmFakePage) called(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, expression := range p.expressions {
		if strings.Contains(expression, method) {
			total++
		}
	}
	return total
}

func (p *swarmFakePage) lastSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) == 0 {
		return ""
	}
	return p.sessions[len(p.sessions)-1]
}

func newSwarmPipe(t *testing.T, page *swarmFakePage) *Pipe {
	t.Helper()
	toHostReader, toHostWriter := io.Pipe()
	toPageReader, toPageWriter := io.Pipe()
	pipe := NewPipe(toHostReader, toPageWriter)

	handle := func(frame string) {
		var request struct {
			ID        int            `json:"id"`
			Method    string         `json:"method"`
			Params    map[string]any `json:"params"`
			SessionID string         `json:"sessionId"`
		}
		if json.Unmarshal([]byte(frame), &request) != nil {
			return
		}
		result := map[string]any{}
		switch request.Method {
		case "Target.getTargets":
			result["targetInfos"] = page.targets
		case "Target.attachToTarget":
			if target, ok := request.Params["targetId"].(string); ok {
				result["sessionId"] = "session-" + target
			}
		case "Runtime.evaluate":
			expression, _ := request.Params["expression"].(string)
			result["result"] = map[string]any{"type": "object", "value": page.answer(expression, request.SessionID)}
		}
		response, _ := json.Marshal(map[string]any{"id": request.ID, "result": result, "sessionId": request.SessionID})
		_, _ = toHostWriter.Write(append(response, 0))
	}
	go func() {
		buffer := make([]byte, 0, 64<<10)
		chunk := make([]byte, 4096)
		for {
			n, err := toPageReader.Read(chunk)
			buffer = append(buffer, chunk[:n]...)
			for {
				at := strings.IndexByte(string(buffer), 0)
				if at < 0 {
					break
				}
				handle(string(buffer[:at]))
				buffer = buffer[at+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { pipe.Dispose() })
	return pipe
}

func workTargets() []map[string]any {
	return []map[string]any{
		{"targetId": "work", "type": "page", "url": "app://localhost/kimi-agent.html"},
		{"targetId": "main", "type": "page", "url": "app://localhost/desktopclaw.html"},
	}
}

func swarmOK(key string, enabled bool, revision float64) map[string]any {
	return swarmOKEpoch(key, enabled, revision, "epoch-1")
}

func swarmOKEpoch(key string, enabled bool, revision float64, epoch string) map[string]any {
	return map[string]any{"status": "ok", "conversationKey": key, "enabled": enabled,
		"epoch": epoch, "revision": revision}
}

func TestSwarmRPCExpressionSendsExactlyTheOfficialParams(t *testing.T) {
	get := swarmRPCExpression(swarmMethodGet, "conv-1", nil)
	if !strings.Contains(get, `method:"app.swarmMode.get"`) {
		t.Fatalf("get expression: %s", get)
	}
	if !strings.Contains(get, `params:{conversationKey:"conv-1"}`) {
		t.Fatalf("get params must be the key alone: %s", get)
	}

	enabled := true
	set := swarmRPCExpression(swarmMethodSet, "conv-1", &enabled)
	if !strings.Contains(set, `params:{conversationKey:"conv-1",enabled:true}`) {
		t.Fatalf("set params: %s", set)
	}

	off := false
	if !strings.Contains(swarmRPCExpression(swarmMethodSet, "conv-1", &off), `enabled:false`) {
		t.Fatal("an explicit false must be sent as false, not omitted")
	}

	hostile := swarmRPCExpression(swarmMethodGet, `a");alert(1);//`, nil)
	if strings.Contains(hostile, `conversationKey:"a");alert(1);//"`) {
		t.Fatalf("key was not escaped: %s", hostile)
	}
	if !strings.Contains(hostile, `conversationKey:"a\");alert(1);//"`) {
		t.Fatalf("escaped key: %s", hostile)
	}
	if strings.Contains(get, "message") {
		t.Fatalf("expression must not read the official message: %s", get)
	}
}

func TestSwarmConversationKeyComesFromTheWorkSidebar(t *testing.T) {
	page := &swarmFakePage{targets: workTargets(), key: "conv-42"}
	actions := NewCDPActions(newSwarmPipe(t, page), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key, err := actions.swarmConversationKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if key != "conv-42" {
		t.Fatalf("key: %q", key)
	}
	if page.lastSession() != "session-work" {
		t.Fatalf("the key was read from %q, not the Work view", page.lastSession())
	}
}

func TestSwarmWithoutAnActiveConversationIsUnavailable(t *testing.T) {
	for name, page := range map[string]*swarmFakePage{
		"no row":     {targets: workTargets(), key: nil},
		"blank key":  {targets: workTargets(), key: "   "},
		"wrong type": {targets: workTargets(), key: 7.0},
	} {
		t.Run(name, func(t *testing.T) {
			actions := NewCDPActions(newSwarmPipe(t, page), t.TempDir(), t.TempDir()).(*cdpActions)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key, err := actions.swarmConversationKey(ctx)
			if err == nil || err.Error() != SwarmErrConversationUnavailable || key != "" {
				t.Fatalf("key=%q err=%v", key, err)
			}
			if page.called(swarmMethodGet) != 0 || page.called(swarmMethodSet) != 0 {
				t.Fatal("an RPC ran without a conversation key")
			}
		})
	}
}

func TestSwarmTargetIsOnlyTheWorkView(t *testing.T) {
	page := &swarmFakePage{targets: []map[string]any{
		{"targetId": "main", "type": "page", "url": "app://localhost/desktopclaw.html"},
		{"targetId": "chat", "type": "page", "url": "https://www.kimi.com/chat/abc"},
	}, key: "conv-1"}
	actions := NewCDPActions(newSwarmPipe(t, page), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := actions.swarmGet(ctx, "conv-1"); err == nil || err.Error() != SwarmErrPageUnavailable {
		t.Fatalf("err: %v", err)
	}
	if page.called(swarmMethodGet) != 0 {
		t.Fatal("an RPC ran against a page that is not the Work view")
	}
}

func TestSwarmGetUnwrapsTheEnvelope(t *testing.T) {
	page := &swarmFakePage{targets: workTargets(), key: "conv-1", get: swarmOK("conv-1", true, 3)}
	actions := NewCDPActions(newSwarmPipe(t, page), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snapshot, err := actions.swarmGet(ctx, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != SwarmStateOn || !snapshot.Enabled || snapshot.Revision != 3 ||
		snapshot.Epoch != "epoch-1" || snapshot.ConversationKey != "conv-1" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if snapshot.Confirmed {
		t.Fatal("a read was reported as confirmed")
	}
}

func TestSwarmGetReportsTheOfficialCodeWithoutTheMessage(t *testing.T) {
	page := &swarmFakePage{targets: workTargets(), key: "conv-1", get: map[string]any{
		"status": "rpc-failed", "code": "-32000", "reasonCode": "swarm_gate"}}
	actions := NewCDPActions(newSwarmPipe(t, page), t.TempDir(), t.TempDir()).(*cdpActions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snapshot, err := actions.swarmGet(ctx, "conv-1")
	if err == nil || err.Error() != SwarmErrRPCFailed {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Reason != SwarmErrRPCFailed || snapshot.State != SwarmStateUnknown {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if snapshot.UpstreamCode != "-32000" {
		t.Fatalf("upstream code: %q", snapshot.UpstreamCode)
	}
}

func TestSwarmReasonCodeIsUsedWhenTheRPCCodeIsEmpty(t *testing.T) {
	got, err := parseSwarmAnswer(map[string]any{"status": "rpc-failed", "code": "",
		"reasonCode": "membership_required"}, "conv-1")
	if err == nil || got.UpstreamCode != "membership_required" {
		t.Fatalf("got: %+v err: %v", got, err)
	}
}

func TestSwarmSnapshotShapeMismatchIsContractChanged(t *testing.T) {
	cases := map[string]any{
		"no control plane": map[string]any{"status": "no-control-plane"},
		"unknown status":   map[string]any{"status": "something-new"},
		"missing enabled":  map[string]any{"status": "ok", "conversationKey": "conv-1", "epoch": "e", "revision": 1.0},
		"enabled not bool": map[string]any{"status": "ok", "conversationKey": "conv-1", "enabled": nil, "epoch": "e", "revision": 1.0},
		"epoch not a string": map[string]any{"status": "ok", "conversationKey": "conv-1", "enabled": true,
			"revision": 1.0},
		"negative revision": map[string]any{"status": "ok", "conversationKey": "conv-1", "enabled": true,
			"epoch": "e", "revision": -1.0},
		"fractional revision": map[string]any{"status": "ok", "conversationKey": "conv-1", "enabled": true,
			"epoch": "e", "revision": 1.5},
		"other conversation": map[string]any{"status": "ok", "conversationKey": "conv-2", "enabled": true,
			"epoch": "e", "revision": 1.0},
		"not an object": "ok",
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot, err := parseSwarmAnswer(answer, "conv-1")
			if err == nil || err.Error() != SwarmErrContractChanged {
				t.Fatalf("err: %v", err)
			}
			if snapshot.Reason != SwarmErrContractChanged {
				t.Fatalf("snapshot: %+v", snapshot)
			}
		})
	}
}

func TestSwarmSnapshotWithAnUnstampedEpochIsStillAPlainAnswer(t *testing.T) {
	snapshot, err := parseSwarmAnswer(swarmOKEpoch("conv-1", true, 0, ""), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != SwarmStateOn || !snapshot.Enabled || snapshot.Epoch != "" || snapshot.Revision != 0 {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

// swarmSurfaceFake sits in Controller.actions beside the page-level fake.
type swarmSurfaceFake struct {
	*fakeActions

	key    string
	keyErr error
	// keyAfterSet is what the page shows once a write has run, modelling the user
	// switching conversations mid-call.
	keyAfterSet string

	getResult SwarmSnapshot
	getErr    error

	setResult SwarmSnapshot
	setErr    error

	sets      []bool
	writesKey []string
	getKeys   []string
	keyReads  int
	// afterSet, when set, answers the readback that follows a successful write.
	afterSet *SwarmSnapshot
}

func (f *swarmSurfaceFake) swarmConversationKey(context.Context) (string, error) {
	f.keyReads++
	if f.keyErr != nil {
		return "", f.keyErr
	}
	if len(f.sets) > 0 && f.keyAfterSet != "" {
		return f.keyAfterSet, nil
	}
	return f.key, nil
}

func (f *swarmSurfaceFake) swarmGet(_ context.Context, key string) (SwarmSnapshot, error) {
	f.getKeys = append(f.getKeys, key)
	if f.afterSet != nil && len(f.sets) > 0 {
		return *f.afterSet, nil
	}
	return f.getResult, f.getErr
}

func (f *swarmSurfaceFake) swarmSet(_ context.Context, key string, enabled bool) (SwarmSnapshot, error) {
	f.sets = append(f.sets, enabled)
	f.writesKey = append(f.writesKey, key)
	return f.setResult, f.setErr
}

func swarmController(t *testing.T, actions PageActions) *Controller {
	t.Helper()
	controller, err := NewController(ControllerSettings{StateDir: t.TempDir(), UserDataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	controller.actions = actions
	return controller
}

func TestControllerSwarmModeReadsWithoutWriting(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		getResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 2}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SwarmMode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Enabled || snapshot.State != SwarmStateOn || snapshot.Confirmed {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if len(actions.sets) != 0 {
		t.Fatal("a read wrote swarm mode")
	}
}

func TestControllerSetSwarmModeDoesNotWriteAfterConversationChanges(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-2"}
	controller := swarmController(t, actions)
	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrConversationChanged || snapshot.Confirmed || len(actions.writesKey) != 0 {
		t.Fatalf("snapshot=%+v err=%v writes=%v", snapshot, err, actions.writesKey)
	}
}

func TestControllerSetSwarmModeConfirmsOnlyAfterMatchingReadback(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		getResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 3},
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 3}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Confirmed || !snapshot.Enabled || snapshot.State != SwarmStateOn {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if len(actions.sets) != 1 || !actions.sets[0] {
		t.Fatalf("sets: %+v", actions.sets)
	}
	if len(actions.writesKey) != 1 || actions.writesKey[0] != "conv-1" {
		t.Fatalf("write keys: %+v", actions.writesKey)
	}
	if actions.keyReads != 2 {
		t.Fatalf("the active conversation was checked %d times, want one before and one before confirming", actions.keyReads)
	}
	for _, key := range actions.getKeys {
		if key != "conv-1" {
			t.Fatalf("readback used %q", key)
		}
	}
}

// A set is scoped to its key, so the RPC keeps answering for that key after the
// user leaves the conversation. The page's own active row is the only proof the
// user is still looking at what was just written.
func TestControllerSetSwarmModeRefusesWhenTheActiveConversationMovedOn(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		keyAfterSet: "conv-2",
		getResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 4},
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 4}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrConversationChanged {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Confirmed || snapshot.Reason != SwarmErrConversationChanged {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	// The write did happen for the conversation it was scoped to, and the
	// readback still names it; neither is accepted as confirmation.
	if len(actions.writesKey) != 1 || actions.writesKey[0] != "conv-1" {
		t.Fatalf("write keys: %+v", actions.writesKey)
	}
}

func TestControllerSetSwarmModeRefusesAMismatchedReadback(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		getResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 3},
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "e", Revision: 3},
		afterSet: &SwarmSnapshot{State: SwarmStateOff, ConversationKey: "conv-1", Enabled: false,
			Epoch: "e", Revision: 3}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrNotConfirmed {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Confirmed || snapshot.Enabled || snapshot.Reason != SwarmErrNotConfirmed {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

// A new epoch means the page reset its cache for every conversation, so a
// revision read under it cannot be the one the write produced.
func TestControllerSetSwarmModeRefusesAChangedEpoch(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "epoch-before", Revision: 3},
		afterSet: &SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "epoch-after", Revision: 3}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrNotConfirmed {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Confirmed || snapshot.Reason != SwarmErrNotConfirmed || snapshot.Epoch != "epoch-after" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

// A write the page had not stamped yet carries no epoch to compare, so only the
// mode and the conversation decide.
func TestControllerSetSwarmModeAcceptsAnUnstampedWrite(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true, Revision: 1},
		afterSet: &SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true,
			Epoch: "epoch-stamped", Revision: 1}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Confirmed {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

func TestControllerSetSwarmModeReportsAConversationChange(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		setResult: SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-1", Enabled: true},
		afterSet: &SwarmSnapshot{State: SwarmStateOn, ConversationKey: "conv-2", Enabled: true,
			Epoch: "e", Revision: 1}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrConversationChanged {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Confirmed || snapshot.Reason != SwarmErrConversationChanged {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

func TestControllerSetSwarmModeKeepsTheFailureReasonAndTheRealState(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true}, key: "conv-1",
		setResult: SwarmSnapshot{State: SwarmStateUnknown, Reason: SwarmErrRPCFailed,
			ConversationKey: "conv-1", UpstreamCode: "swarm_gate"},
		setErr: context.DeadlineExceeded,
		getResult: SwarmSnapshot{State: SwarmStateOff, ConversationKey: "conv-1", Enabled: false,
			Epoch: "e", Revision: 0}}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if snapshot.Confirmed || snapshot.Enabled || snapshot.Reason != SwarmErrRPCFailed ||
		snapshot.UpstreamCode != "swarm_gate" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

func TestControllerSwarmWithoutAManagedPageIsUnavailable(t *testing.T) {
	controller := swarmController(t, nil)
	if _, err := controller.SwarmMode(context.Background()); err == nil ||
		err.Error() != SwarmErrUnavailable {
		t.Fatalf("err: %v", err)
	}
	if _, err := controller.SetSwarmMode(context.Background(), "conv-1", true); err == nil ||
		err.Error() != SwarmErrUnavailable {
		t.Fatalf("err: %v", err)
	}
}

func TestControllerSwarmReportsAnUnsupportedSurface(t *testing.T) {
	controller := swarmController(t, &fakeActions{connected: true})
	snapshot, err := controller.SwarmMode(context.Background())
	if err == nil || err.Error() != SwarmErrUnsupported {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Reason != SwarmErrUnsupported || snapshot.State != SwarmStateUnknown {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

func TestControllerSwarmSurfacesAConversationThatCannotBeRead(t *testing.T) {
	actions := &swarmSurfaceFake{fakeActions: &fakeActions{connected: true},
		keyErr: errors.New(SwarmErrConversationUnavailable)}
	controller := swarmController(t, actions)

	snapshot, err := controller.SetSwarmMode(context.Background(), "conv-1", true)
	if err == nil || err.Error() != SwarmErrConversationUnavailable {
		t.Fatalf("err: %v", err)
	}
	if snapshot.Reason != SwarmErrConversationUnavailable || snapshot.Confirmed {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if len(actions.sets) != 0 {
		t.Fatal("a write ran without a conversation key")
	}
}

func TestSwarmKeyValidation(t *testing.T) {
	if ValidSwarmConversationKey("") || ValidSwarmConversationKey(" padded") || ValidSwarmConversationKey("trailing ") ||
		ValidSwarmConversationKey(strings.Repeat("k", 1025)) || ValidSwarmConversationKey("bad\x00key") {
		t.Fatal("an invalid key was accepted")
	}
	if !ValidSwarmConversationKey("conversation-2f1e") {
		t.Fatal("a valid key was refused")
	}
}

func TestSafeSwarmCodeDropsEverythingButTheCode(t *testing.T) {
	if got := safeSwarmCode("swarm_gate"); got != "swarm_gate" {
		t.Fatalf("code: %q", got)
	}
	got := safeSwarmCode("membership required for Moderato; see https://x")
	if strings.ContainsAny(got, " ;/") {
		t.Fatalf("code carried message text: %q", got)
	}
	if got != "membershiprequiredforModeratoseehttps:x" {
		t.Fatalf("code: %q", got)
	}
	if safeSwarmCode("中文") != "" {
		t.Fatal("non-ascii text was carried")
	}
}
