package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const conversationFixtureAlias = "byok-custom/claude"
const conversationFixtureKey = "conv-'quoted'; SELECT 1"
const conversationFixtureSelection = "{\n  \"unrelated\": {\"modelKey\": \"byok-other\"},\n  \"conv-'quoted'; SELECT 1\": {\"modelKey\": \"official\", \"keep\": true}\n}\n"
const conversationFixtureBindings = "{ \"unrelated\": \"byok-other\" }\n"

func conversationTestNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node with node:sqlite unavailable")
	}
	if err := exec.Command(node, "-e", `require('node:sqlite')`).Run(); err != nil {
		t.Skip("Node with node:sqlite unavailable")
	}
	return node
}

func conversationTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func conversationTestRead(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func conversationTestSQL(t *testing.T, store conversationStore, script string) string {
	t.Helper()
	path := filepath.Join(store.home, "agents", "main", "sessions", "hosted-logical", "conversations.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(store.nodePath, "-e", `const db = new (require('node:sqlite').DatabaseSync)(process.argv[1]);`+script, path)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture SQL: %v: %s", err, out)
	}
	return string(out)
}

func conversationFixture(t *testing.T) (conversationStore, string, string) {
	t.Helper()
	node := conversationTestNode(t)
	profile := t.TempDir()
	home := filepath.Join(profile, "daimon-share", "daimon")
	store := conversationStore{profile: profile, home: home, nodePath: node}
	conversationTestSQL(t, store, `
		db.exec("CREATE TABLE schema_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT");
		db.prepare("INSERT INTO schema_metadata VALUES ('schema_version', '1')").run();
		db.exec("CREATE TABLE conversations (agent_id TEXT NOT NULL, conversation_key TEXT NOT NULL, kernel_records_path TEXT, extra_json TEXT, title TEXT, first_user_text TEXT, workspace_path TEXT, updated_at_ms INTEGER NOT NULL, PRIMARY KEY (agent_id, conversation_key)) STRICT");
		const insert = db.prepare('INSERT INTO conversations VALUES (?, ?, ?, ?, ?, ?, ?, ?)');
		insert.run('main', `+"`"+conversationFixtureKey+"`"+`, '/records/1', '{"runtimePreferences":{"modelAlias":"kimi-k3","thinkingLevel":"high","other":true},"unrelated":9007199254740993}', '', ' An earlier   conversation ', '/test', 20);
		insert.run('main', 'second', '/records/2', '{"runtimePreferences":{"modelAlias":"byok-kth-resp/prior","thinkingLevel":"high"}}', 'Second', '', '', 10);
	`)
	dir := filepath.Join(profile, "kimi-agent")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	selection := filepath.Join(dir, "conversation-kimi-work-models.json")
	bindings := filepath.Join(dir, "byok-applied-model-bindings.json")
	conversationTestWrite(t, selection, conversationFixtureSelection)
	conversationTestWrite(t, bindings, conversationFixtureBindings)
	setConversationTestModels(t, store)
	return store, selection, bindings
}

func setConversationTestModels(t *testing.T, store conversationStore) {
	t.Helper()
	conversationTestWrite(t, filepath.Join(store.home, NativeWorkConfig), `{"credentials":{"kimiWeb":{"userId":"test-account"}}}`)
	account := v3Account("http://127.0.0.1:9910/v1", "fixture")
	active := account["active"].(map[string]any)
	account["sources"] = append(account["sources"].([]any), map[string]any{"kind": "custom", "id": "source-custom"})
	active["providers"] = append(active["providers"].([]any), map[string]any{
		"id": "anthropic-test", "sourceId": "source-custom", "protocol": "anthropic"})
	model := map[string]any{"alias": conversationFixtureAlias, "sourceId": "source-custom", "providerId": "anthropic-test",
		"model": "claude", "displayName": "Claude test", "supportEfforts": []string{"low", "high"}, "defaultEffort": "low"}
	blocked := map[string]any{"alias": "byok-custom/blocked", "sourceId": "source-custom", "providerId": "anthropic-test", "model": "blocked"}
	active["models"] = append(active["models"].([]any), model, blocked)
	account["blockedAliases"] = map[string]string{"byok-custom/blocked": "quarantined"}
	account["pending"] = map[string]any{"models": []any{map[string]any{"alias": "byok-custom/pending", "sourceId": "source-custom", "providerId": "anthropic-test", "model": "pending"}}}
	content, err := json.Marshal(map[string]any{"accounts": map[string]any{sha256Hex("test-account"): account,
		sha256Hex("another-account"): v3Account("http://127.0.0.1:9911/v1", "other")}})
	if err != nil {
		t.Fatal(err)
	}
	conversationTestWrite(t, filepath.Join(store.home, NativeWorkStore), string(content))
}

func TestConversationRebindBatchUpdatesSQLiteAndElectronFiles(t *testing.T) {
	store, selection, bindings := conversationFixture(t)
	controller := &Controller{settings: ControllerSettings{UserDataDir: store.profile, ProfileMode: ProfileModeIsolated,
		Installation: Installation{Application: "fixture"}, ConversationNodePath: store.nodePath}}
	previous := listOfficialProcesses
	listOfficialProcesses = func(context.Context, string) ([]int, bool, string) { return nil, true, "" }
	t.Cleanup(func() { listOfficialProcesses = previous })
	ctx := context.Background()
	list := controller.WorkConversations(ctx)
	if list.Reason != "" || list.Application != NotRunning || len(list.Conversations) != 2 || len(list.Models) != 3 {
		t.Fatalf("list = %+v", list)
	}
	first := list.Conversations[0]
	if first.ConversationKey != conversationFixtureKey || first.IsBYOK || first.Title != "An earlier conversation" || first.WorkspacePath != "/test" || !list.Conversations[1].IsBYOK {
		t.Fatalf("rows = %+v", list.Conversations)
	}
	request := ConversationRebindRequest{ConversationKeys: []string{conversationFixtureKey, "second"}, TargetModelAlias: conversationFixtureAlias, ThinkingLevel: "medium"}
	if _, err := controller.RebindWorkConversations(ctx, request); err == nil || err.Error() != "invalid_conversation_thinking_level" {
		t.Fatalf("invalid level accepted: %v", err)
	}
	request.ThinkingLevel = ""
	result, err := controller.RebindWorkConversations(ctx, request)
	if err != nil || result.ThinkingLevel != "low" || !result.DirectAnthropicHistoryWarning || result.KimiRestarted {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if content := conversationTestRead(t, selection); !strings.Contains(content, `"thinkingLevel":"low"`) || !strings.Contains(content, `"keep":true`) || !strings.Contains(content, `"unrelated"`) {
		t.Fatalf("selection = %s", content)
	}
	if content := conversationTestRead(t, bindings); strings.Count(content, conversationFixtureAlias) != 2 || !strings.Contains(content, "byok-other") {
		t.Fatalf("bindings = %s", content)
	}
	if extra := conversationTestSQL(t, store, `console.log(db.prepare('SELECT extra_json FROM conversations ORDER BY updated_at_ms DESC LIMIT 1').get().extra_json)`); !strings.Contains(extra, `"other":true`) || !strings.Contains(extra, `9007199254740993`) {
		t.Fatalf("extra = %s", extra)
	}
	for _, row := range controller.WorkConversations(ctx).Conversations {
		if row.ModelAlias != conversationFixtureAlias || !row.IsBYOK || row.ThinkingLevel != "low" {
			t.Fatalf("reloaded row = %+v", row)
		}
	}
	request.TargetModelAlias = "byok-kth-resp/gpt-probe-model"
	if _, err := controller.RebindWorkConversations(ctx, request); err != nil {
		t.Fatal(err)
	}
	if extra := conversationTestSQL(t, store, `console.log(db.prepare('SELECT extra_json FROM conversations LIMIT 1').get().extra_json)`); strings.Contains(extra, `"thinkingLevel"`) {
		t.Fatalf("thinkingLevel was not cleared: %s", extra)
	}
}

func TestConversationRebindRollsBackSQLiteAndBothFilesOnCommitFailure(t *testing.T) {
	for _, missingSelection := range []bool{false, true} {
		name := "existing selection"
		if missingSelection {
			name = "missing selection"
		}
		t.Run(name, func(t *testing.T) {
			store, selection, bindings := conversationFixture(t)
			if missingSelection {
				if err := os.Remove(selection); err != nil {
					t.Fatal(err)
				}
			}
			conversationTestSQL(t, store, `db.exec(`+"`"+`
				CREATE TABLE parent(id INTEGER PRIMARY KEY) STRICT;
				CREATE TABLE child(id INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED) STRICT;
				CREATE TRIGGER deferred_failure AFTER UPDATE ON conversations BEGIN INSERT INTO child VALUES (42); END;
			`+"`"+`)`)
			_, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "rebind", ConversationKeys: []string{conversationFixtureKey, "second"}, ModelAlias: conversationFixtureAlias})
			if err == nil {
				t.Fatal("commit unexpectedly succeeded")
			}
			if missingSelection {
				if _, err := os.Stat(selection); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("originally absent selection: %v", err)
				}
			} else if content := conversationTestRead(t, selection); content != conversationFixtureSelection {
				t.Fatalf("selection not restored byte for byte: %s", content)
			}
			if content := conversationTestRead(t, bindings); content != conversationFixtureBindings {
				t.Fatalf("bindings not restored byte for byte: %s", content)
			}
			rows, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 100})
			if err != nil || rows.Conversations[0].ModelAlias != "kimi-k3" || rows.Conversations[1].ModelAlias != "byok-kth-resp/prior" {
				t.Fatalf("database not restored: %+v %v", rows, err)
			}
			entries, err := os.ReadDir(filepath.Dir(selection))
			if err != nil || slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return strings.HasPrefix(entry.Name(), ".kwp-") }) {
				t.Fatalf("temporary file remained: %v %v", entries, err)
			}
		})
	}
}

func TestConversationRebindChecksProcessThenRestarts(t *testing.T) {
	store, _, _ := conversationFixture(t)
	controller := &Controller{settings: ControllerSettings{UserDataDir: store.profile, ProfileMode: ProfileModeIsolated,
		Installation: Installation{Application: "fixture"}, ConversationNodePath: store.nodePath}}
	previousList, previousIdentity, previousTargets, previousQuit, previousStart := listOfficialProcesses, officialProcessIdentity, selectQuitTargets, requestOfficialQuit, restartConversationKimi
	t.Cleanup(func() {
		listOfficialProcesses, officialProcessIdentity, selectQuitTargets, requestOfficialQuit, restartConversationKimi = previousList, previousIdentity, previousTargets, previousQuit, previousStart
	})
	running, known, failStop, failConfirmation := true, true, false, false
	var actions []string
	listOfficialProcesses = func(ctx context.Context, _ string) ([]int, bool, string) {
		if running {
			return []int{101}, known, ""
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 4*time.Second || time.Until(deadline) > IdentityProbeTimeout {
			t.Fatalf("post-stop probe reused the stop deadline: %v", deadline)
		}
		return nil, known && !failConfirmation, ""
	}
	officialProcessIdentity = func(context.Context, int) ProcessIdentity {
		if running {
			return ProcessIdentity{Kind: "fixture", Value: "pid-101"}
		}
		return ProcessIdentity{Kind: IdentityNotFound}
	}
	selectQuitTargets = func(pids []int) []int { return pids }
	requestOfficialQuit = func(context.Context, int) error {
		if failStop {
			return errors.New("fixture_quit_failed")
		}
		actions = append(actions, "quit")
		running = false
		return nil
	}
	restartConversationKimi = func(_ *Controller, ctx context.Context) error {
		actions = append(actions, "start")
		rows, err := store.run(ctx, conversationSQLiteRequest{Operation: "list", Limit: 100})
		if err != nil || rows.Conversations[0].ModelAlias != conversationFixtureAlias {
			t.Fatalf("restart before write: %+v %v", rows, err)
		}
		return nil
	}
	ctx := context.Background()
	request := ConversationRebindRequest{ConversationKeys: []string{conversationFixtureKey}, TargetModelAlias: conversationFixtureAlias}
	if _, err := controller.RebindWorkConversations(ctx, request); err == nil || err.Error() != "conversation_kimi_restart_required" || len(actions) > 0 {
		t.Fatalf("unconfirmed running write: %v %v", err, actions)
	}
	request.RestartKimi = true
	known = false
	if _, err := controller.RebindWorkConversations(ctx, request); err == nil || err.Error() != "conversation_kimi_state_unknown" {
		t.Fatalf("unknown process state was accepted: %v", err)
	}
	known, failStop = true, true
	if _, err := controller.RebindWorkConversations(ctx, request); err == nil || err.Error() != "conversation_kimi_stop_failed" {
		t.Fatalf("failed stop was accepted: %v", err)
	}
	failStop, failConfirmation = false, true
	stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if result, err := controller.RebindWorkConversations(stopCtx, request); !errors.Is(err, errConversationStopFailed) || result.KimiRestarted || !slices.Equal(actions, []string{"quit"}) {
		t.Fatalf("unconfirmed stop: result = %+v, error = %v, actions = %v", result, err, actions)
	}
	previousExec := execCommandContext
	t.Cleanup(func() { execCommandContext = previousExec })
	execCommandContext = func(ctx context.Context, node string, _ ...string) *exec.Cmd {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 28*time.Second || time.Until(deadline) > conversationWriteTimeout {
			t.Fatalf("write reused the stop deadline: %v", deadline)
		}
		return exec.CommandContext(ctx, node, "-e", `console.log('{"error":"conversation_rollback_failed"}')`)
	}
	running, failConfirmation, actions = true, false, nil
	if result, err := controller.RebindWorkConversations(stopCtx, request); !errors.Is(err, errConversationRollbackFailed) || result.KimiRestarted || !slices.Equal(actions, []string{"quit"}) {
		t.Fatalf("failed rollback: result = %+v, error = %v, actions = %v", result, err, actions)
	}
	execCommandContext = previousExec
	running, actions = true, nil
	if result, err := controller.RebindWorkConversations(ctx, request); err != nil || !result.KimiRestarted || !slices.Equal(actions, []string{"quit", "start"}) {
		t.Fatalf("result = %+v, error = %v, actions = %v", result, err, actions)
	}
}

func TestConversationListMissingSQLiteReturnsAnEmptyList(t *testing.T) {
	controller := &Controller{settings: ControllerSettings{UserDataDir: t.TempDir(), ProfileMode: ProfileModeIsolated}}
	got := controller.WorkConversations(context.Background())
	if got.Reason != "conversation_database_missing" || got.Conversations == nil || len(got.Conversations) != 0 {
		t.Fatalf("list = %+v", got)
	}
}

func TestConversationModelsSelectsCurrentLegacyProfile(t *testing.T) {
	home := t.TempDir()
	store := conversationStore{home: home}
	conversationTestWrite(t, filepath.Join(home, NativeWorkConfig), `{"credentials":{"kimiWeb":{"userId":"test-account"}}}`)
	profile := func(alias, kind, protocol string) string {
		return `{"kind":"` + kind + `","models":[{"alias":"` + alias + `","providerId":"p","model":"model","maxContextSize":512,"protocol":"` + protocol + `"}]}`
	}
	conversationTestWrite(t, filepath.Join(home, NativeWorkStore), `{"profiles":{"`+sha256Hex("test-account")+`":{"byok-kth":`+
		profile("byok-kth-resp/ok", "kth", "openai_responses")+`,"custom":`+profile("byok-custom/ok", "custom", "anthropic")+`},"`+
		sha256Hex("other")+`":{"byok-kth":`+profile("byok-kth/other", "kth", "kimi")+`}}}`)
	models := store.models()
	if len(models) != 2 || models[0].Alias != "byok-custom/ok" || !models[0].DirectAnthropicHistoryWarning || models[1].SourceKind != "kth" {
		t.Fatalf("models = %+v", models)
	}
}

func TestConversationNodeCandidateOrder(t *testing.T) {
	for _, tc := range []struct {
		platform, application, archive string
		paths                          []string
	}{
		{"darwin", "Kimi.app/Contents/MacOS/Kimi", "Kimi.app/Contents/Resources/app.asar", []string{
			"Kimi.app/Contents/Resources/resources/runtime/tool-node/bin/node", "Kimi.app/Contents/Resources/resources/runtime/node",
			"Kimi.app/Contents/Resources/resources/tool-node/bin/node", "Kimi.app/Contents/Resources/resources/tool-node/node"}},
		{"windows", "Kimi/Kimi.exe", "Kimi/resources/app.asar", []string{
			"Kimi/resources/resources/runtime/tool-node/node.exe", "Kimi/resources/resources/runtime/tool-node/bin/node.exe",
			"Kimi/resources/resources/runtime/node.exe", "Kimi/resources/resources/tool-node/node.exe",
			"Kimi/resources/resources/tool-node/bin/node.exe", "Kimi/Kimi.exe"}},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			for _, archive := range []string{"", filepath.FromSlash(tc.archive)} {
				install := Installation{Application: filepath.FromSlash(tc.application), Archive: archive}
				var paths []string
				for _, candidate := range bundledConversationNodes(install, tc.platform) {
					paths = append(paths, filepath.ToSlash(candidate.path))
					if candidate.electron != (tc.platform == "windows" && candidate.path == install.Application) {
						t.Errorf("unexpected Electron mode for %+v", candidate)
					}
				}
				if !slices.Equal(paths, tc.paths) {
					t.Errorf("archive %q: paths = %v, want %v", archive, paths, tc.paths)
				}
			}
		})
	}
}

func TestConversationNodeEnvironment(t *testing.T) {
	inherited := []string{"PATH=testpath", "NODE_OPTIONS=--import=wrong", "kimi_desktop_tool_node=wrong", "Electron_No_Asar=0", "ELECTRON_RUN_AS_NODE=0"}
	for _, electron := range []bool{false, true} {
		want := []string{"PATH=testpath", "ELECTRON_NO_ASAR=1"}
		if electron {
			want = append(want, "ELECTRON_RUN_AS_NODE=1")
		}
		if got := (conversationNodeCandidate{electron: electron}).environment(inherited); !slices.Equal(got, want) {
			t.Fatalf("electron %t: environment = %v", electron, got)
		}
	}
}

func TestConversationRunnerFallsBackFromUnusableCandidates(t *testing.T) {
	store, _, _ := conversationFixture(t)
	node := store.nodePath
	store.nodePath = ""
	store.install = Installation{Archive: filepath.Join(t.TempDir(), "app.asar")}
	candidates := bundledConversationNodes(store.install, runtime.GOOS)
	for _, candidate := range candidates[:2] {
		if err := os.MkdirAll(filepath.Dir(candidate.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(candidate.path, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NODE_OPTIONS", "--invalid-node-option")
	t.Setenv("KIMI_DESKTOP_TOOL_NODE", "wrong")
	t.Setenv("ELECTRON_NO_ASAR", "0")
	t.Setenv("ELECTRON_RUN_AS_NODE", "0")
	previous := execCommandContext
	t.Cleanup(func() { execCommandContext = previous })
	for _, tc := range []struct{ name, first, wantError string }{
		{"start error", "", ""},
		{"crash", `process.exit(1)`, ""},
		{"stdout pollution", `console.log('noise')`, ""},
		{"missing sqlite", `console.log('{"error":"conversation_node_unavailable"}')`, ""},
		{"database error and nonzero exit", `console.log('{"error":"conversation_database_unavailable"}'); process.exit(1)`, "conversation_database_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tried []string
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				tried = append(tried, name)
				if name == candidates[0].path {
					if tc.first == "" {
						return exec.CommandContext(ctx, filepath.Join(store.profile, "absent"))
					}
					return exec.CommandContext(ctx, node, "-e", tc.first)
				}
				const envCheck = `if (process.env.NODE_OPTIONS || process.env.KIMI_DESKTOP_TOOL_NODE || process.env.ELECTRON_RUN_AS_NODE === '0' || process.env.ELECTRON_NO_ASAR !== '1') { console.log('{"error":"bad_environment"}'); process.exit(1); }`
				return exec.CommandContext(ctx, node, "-e", envCheck+args[1])
			}
			got, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 1})
			if tc.wantError != "" {
				if err == nil || err.Error() != tc.wantError || len(tried) != 1 {
					t.Fatalf("database result = %+v, %v, candidates %v", got, err, tried)
				}
			} else if err != nil || len(got.Conversations) != 1 || !slices.Equal(tried, []string{candidates[0].path, candidates[1].path}) {
				t.Fatalf("fallback result = %+v, %v, candidates %v", got, err, tried)
			}
		})
	}
	execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, node, "-e", `console.log('{}')`)
	}
	if _, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 1}); err == nil || err.Error() != "conversation_node_unavailable" {
		t.Fatalf("unusable runtimes reported as %v", err)
	}
}

func TestConversationRebindRunnerDeadline(t *testing.T) {
	store, _, _ := conversationFixture(t)
	previous := execCommandContext
	t.Cleanup(func() { execCommandContext = previous })
	execCommandContext = func(ctx context.Context, node string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, node, "-e", `setInterval(() => {}, 1000)`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := store.run(ctx, conversationSQLiteRequest{Operation: "rebind"})
	if !errors.Is(err, errConversationRollbackFailed) || !errors.Is(ctx.Err(), context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Fatalf("deadline = %v, rebind = %v, duration = %v", ctx.Err(), err, time.Since(started))
	}
}

func TestConversationRebindRequiresEffortWithoutDefault(t *testing.T) {
	store := conversationStore{home: t.TempDir()}
	conversationTestWrite(t, filepath.Join(store.home, NativeWorkConfig), `{"credentials":{"kimiWeb":{"userId":"test-account"}}}`)
	conversationTestWrite(t, filepath.Join(store.home, NativeWorkStore), `{"profiles":{"`+sha256Hex("test-account")+`":{"byok-kth":{"kind":"kth","models":[{"alias":"byok-kth/model","providerId":"p","model":"model","maxContextSize":512,"protocol":"kimi","supportEfforts":["zeta","max","medium","high","off","alpha","low","xhigh","high"],"defaultEffort":"invalid"}]}}}}`)
	want := []string{"off", "low", "medium", "high", "xhigh", "max", "alpha", "zeta"}
	if models := store.models(); len(models) != 1 || !slices.Equal(models[0].SupportEfforts, want) || models[0].DefaultEffort != "" {
		t.Fatalf("models = %+v", models)
	}
	request := ConversationRebindRequest{TargetModelAlias: "byok-kth/model"}
	if _, _, err := store.target(request); err == nil || err.Error() != "invalid_conversation_thinking_level" {
		t.Fatalf("empty level accepted: %v", err)
	}
	request.ThinkingLevel = " HIGH "
	if _, level, err := store.target(request); err != nil || level != "high" {
		t.Fatalf("explicit level = %s, %v", level, err)
	}
}

func TestConversationRebindBatchLimitExceedsListLimit(t *testing.T) {
	request := ConversationRebindRequest{TargetModelAlias: "byok-kth/model"}
	for i := range 500 {
		request.ConversationKeys = append(request.ConversationKeys, "conv-"+strconv.Itoa(i))
	}
	if err := validateConversationRebind(request); err != nil {
		t.Fatalf("batch of 500 refused: %v", err)
	}
	request.ConversationKeys = append(request.ConversationKeys, "one-too-many")
	if err := validateConversationRebind(request); err == nil {
		t.Fatal("batch of 501 accepted")
	}
}

func TestConversationSQLiteReadOnlyReplacementAndCleanup(t *testing.T) {
	node := conversationTestNode(t)
	dir := t.TempDir()
	// Run the exact helper with real files and a deterministic rename failure.
	script := conversationSQLiteScript[:strings.LastIndex(conversationSQLiteScript, "\ntry {")] + `
		const assert = require('node:assert/strict');
		const dir = process.argv[1];
		const target = path.join(dir, 'read-only.json');
		fs.writeFileSync(target, 'before');
		fs.chmodSync(target, 0o400);
		const rename = fs.renameSync;
		fs.renameSync = (source, dest) => {
			assert.equal(fs.statSync(source).mode & 0o200, process.platform === 'win32' ? 0o200 : 0);
			throw new Error('forced rename failure');
		};
		assert.throws(() => replaceFile(target, 'after', 0o400), /forced rename failure/);
		assert.deepEqual(fs.readdirSync(dir), ['read-only.json']);
		assert.equal(fs.readFileSync(target, 'utf8'), 'before');
		fs.renameSync = rename;
		try {
			replaceFile(target, 'after', 0o400);
			assert.equal(fs.readFileSync(target, 'utf8'), 'after');
			if (process.platform !== 'win32') assert.equal(fs.statSync(target).mode & 0o777, 0o400);
		} catch (error) {
			if (process.platform !== 'win32' || !['EPERM', 'EACCES'].includes(error.code)) throw error;
			assert.equal(fs.readFileSync(target, 'utf8'), 'before');
		}
		assert.deepEqual(fs.readdirSync(dir), ['read-only.json']);
		fs.chmodSync(target, 0o600);
	`
	if output, err := exec.Command(node, "-e", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("readonly files: %v\n%s", err, output)
	}
}

func TestConversationAliasMigrationPreservesSupportedEffortAndBatches(t *testing.T) {
	store, selection, bindings := conversationFixture(t)
	const previous = "byok-kth-kimi/group/kimi-example"
	const target = "byok-kth-kimi/kimi-k3-kwp-example"
	conversationTestSQL(t, store, `
		const insert = db.prepare('INSERT INTO conversations VALUES (?, ?, ?, ?, ?, ?, ?, ?)');
		const preferences = (modelAlias, thinkingLevel) => JSON.stringify({runtimePreferences: {modelAlias, thinkingLevel}});
		insert.run('main', 'earlier', '/records/e', preferences('byok-kth-kimi/group/kimi-example', 'max'), '', '', '', 1);
		insert.run('main', 'later', '/records/l', preferences('byok-kth-kimi/group/kimi-example', 'high'), '', '', '', 2);
		insert.run('main', 'malformed', '/records/m', 'malformed', '', '', '', 3);
	`)
	request := conversationSQLiteRequest{Operation: "rebind", PreviousAlias: previous, ModelAlias: target,
		SupportEfforts: []string{"low", "high"}, ThinkingLevel: "low", Limit: 1}
	for i, key := range []string{"later", "earlier"} {
		got, err := store.run(context.Background(), request)
		if err != nil || !slices.Equal(got.ConversationKeys, []string{key}) || got.HasMore != (i == 0) {
			t.Fatalf("batch %d = %+v, %v", i, got, err)
		}
	}
	got, err := store.run(context.Background(), request)
	if err != nil || got.HasMore || len(got.ConversationKeys) != 0 {
		t.Fatalf("repeated migration = %+v, %v", got, err)
	}
	if contents := conversationTestRead(t, selection); !strings.Contains(contents, `"later":{"modelKey":"`+target+`","thinkingLevel":"high"}`) ||
		!strings.Contains(contents, `"earlier":{"modelKey":"`+target+`","thinkingLevel":"low"}`) {
		t.Fatalf("selection = %s", contents)
	}
	if contents := conversationTestRead(t, bindings); strings.Count(contents, target) != 2 || strings.Contains(contents, previous) {
		t.Fatalf("bindings = %s", contents)
	}
	got, err = store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 10})
	if err != nil || len(got.Conversations) != 5 || got.Conversations[0].ModelAlias != "kimi-k3" ||
		got.Conversations[3].ModelAlias != target || got.Conversations[3].ThinkingLevel != "high" ||
		got.Conversations[4].ModelAlias != target || got.Conversations[4].ThinkingLevel != "low" {
		t.Fatalf("reloaded conversations = %+v, %v", got, err)
	}
	// Explicit rebind must apply the requested thinkingLevel even if supportEfforts is supplied.
	if _, err := store.run(context.Background(), conversationSQLiteRequest{
		Operation: "rebind", ConversationKeys: []string{"later"}, ModelAlias: target,
		SupportEfforts: []string{"low", "high"}, ThinkingLevel: "low",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 10})
	if err != nil || got.Conversations[3].ThinkingLevel != "low" {
		t.Fatalf("explicit rebind inherited old thinkingLevel: %+v, %v", got, err)
	}
}

func TestConversationVisibilityScopeSchemaVersionAndNullPreferences(t *testing.T) {
	store, _, _ := conversationFixture(t)
	conversationTestSQL(t, store, `
		const insert = db.prepare('INSERT INTO conversations VALUES (?, ?, ?, ?, ?, ?, ?, ?)');
		insert.run('subagent', `+"`"+conversationFixtureKey+"`"+`, '/records/s', '{"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Subagent duplicate', '', '', 99);
		insert.run('subagent', 'sub-only', '/records/s2', '{"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Subagent only', '', '', 98);
		insert.run('main', 'temp-conv', '/records/t', '{"temporaryConversation":true,"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Temp', '', '', 97);
		insert.run('main', 'onboarding-pending', '/records/o1', '{"onboardingImport":{},"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Onboarding pending', '', '', 96);
		insert.run('main', 'onboarding-no-records', null, '{"onboardingImport":{"completedAt":"2026-11-24T00:00:00Z"},"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Onboarding no records', '', '', 95);
		insert.run('main', 'fork-preparing', '/records/f1', '{"forkCreation":{"state":"preparing"},"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Fork preparing', '', '', 94);
		insert.run('main', 'onboarding-done', '/records/o2', '{"onboardingImport":{"completedAt":"2026-11-24T00:00:00Z"},"runtimePreferences":null}', 'Onboarding done', '', '', 93);
		insert.run('main', 'fork-ready', '/records/f2', '{"forkCreation":{"state":"ready"},"runtimePreferences":{"modelAlias":"kimi-k3"}}', 'Fork ready', '', '', 92);
	`)
	list, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, row := range list.Conversations {
		keys = append(keys, row.ConversationKey)
	}
	want := []string{"onboarding-done", "fork-ready", conversationFixtureKey, "second"}
	if !slices.Equal(keys, want) {
		t.Fatalf("visible keys = %v, want %v", keys, want)
	}
	for _, hidden := range []string{"sub-only", "temp-conv", "onboarding-pending", "onboarding-no-records", "fork-preparing"} {
		if _, err := store.run(context.Background(), conversationSQLiteRequest{
			Operation: "rebind", ConversationKeys: []string{hidden}, ModelAlias: conversationFixtureAlias, ThinkingLevel: "low",
		}); err == nil || err.Error() != "conversation_not_found" {
			t.Fatalf("hidden conversation %s rebind = %v", hidden, err)
		}
	}
	// Duplicate key in another agent must not fail main rebind, and runtimePreferences:null must be accepted.
	if _, err := store.run(context.Background(), conversationSQLiteRequest{
		Operation: "rebind", ConversationKeys: []string{conversationFixtureKey, "onboarding-done", "fork-ready"},
		ModelAlias: conversationFixtureAlias, ThinkingLevel: "low",
	}); err != nil {
		t.Fatalf("rebind visible rows failed: %v", err)
	}
	subAlias := strings.TrimSpace(conversationTestSQL(t, store, `console.log(JSON.parse(db.prepare("SELECT extra_json FROM conversations WHERE agent_id = 'subagent' AND conversation_key = ?").get(`+"`"+conversationFixtureKey+"`"+`).extra_json).runtimePreferences.modelAlias)`))
	if subAlias != "kimi-k3" {
		t.Fatalf("subagent row was unexpectedly modified: %s", subAlias)
	}
	doneAlias := strings.TrimSpace(conversationTestSQL(t, store, `console.log(JSON.parse(db.prepare("SELECT extra_json FROM conversations WHERE agent_id = 'main' AND conversation_key = 'onboarding-done'").get().extra_json).runtimePreferences.modelAlias)`))
	if doneAlias != conversationFixtureAlias {
		t.Fatalf("runtimePreferences:null row not updated: %s", doneAlias)
	}
	// Unsupported schema_version must be refused.
	conversationTestSQL(t, store, `db.prepare("UPDATE schema_metadata SET value = '2' WHERE key = 'schema_version'").run();`)
	if _, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "list", Limit: 10}); err == nil || err.Error() != "conversation_database_schema_unsupported" {
		t.Fatalf("schema v2 = %v", err)
	}
}

func TestConversationIntentJournalCrashRecoveryAndCancellationErrors(t *testing.T) {
	t.Run("crash after commit converges JSON on WorkConversations when Kimi stopped", func(t *testing.T) {
		store, selection, bindings := conversationFixture(t)
		// Simulate DB committed but SIGKILL happened before JSON files were replaced.
		conversationTestSQL(t, store, `
			db.prepare("UPDATE conversations SET extra_json = json_set(extra_json, '$.runtimePreferences.modelAlias', ?, '$.runtimePreferences.thinkingLevel', ?) WHERE agent_id = 'main' AND conversation_key = ?")
				.run('`+conversationFixtureAlias+`', 'low', `+"`"+conversationFixtureKey+"`"+`);
		`)
		journal := map[string]any{
			"version":     1,
			"hadOfficial": true,
			"hasMore":     false,
			"entries": []map[string]any{{
				"key":           conversationFixtureKey,
				"oldAlias":      "kimi-k3",
				"oldLevel":      "high",
				"modelAlias":    conversationFixtureAlias,
				"thinkingLevel": "low",
				"byok":          true,
			}},
		}
		raw, _ := json.Marshal(journal)
		conversationTestWrite(t, store.journalPath(), string(raw))

		// While Kimi is running, WorkConversations must NOT touch Electron JSON files.
		controller := &Controller{settings: ControllerSettings{UserDataDir: store.profile, ProfileMode: ProfileModeIsolated,
			Installation: Installation{Application: "fixture"}, ConversationNodePath: store.nodePath}}
		prevList := listOfficialProcesses
		t.Cleanup(func() { listOfficialProcesses = prevList })
		running := true
		listOfficialProcesses = func(context.Context, string) ([]int, bool, string) {
			if running {
				return []int{42}, true, ""
			}
			return nil, true, ""
		}
		_ = controller.WorkConversations(context.Background())
		if !store.hasPendingJournal() || conversationTestRead(t, selection) != conversationFixtureSelection {
			t.Fatal("WorkConversations mutated JSON while Kimi was running")
		}

		// Once Kimi is stopped, WorkConversations recovers the committed journal and syncs both JSON files.
		running = false
		list := controller.WorkConversations(context.Background())
		if list.Reason != "" || store.hasPendingJournal() {
			t.Fatalf("journal not recovered: reason=%s pending=%t", list.Reason, store.hasPendingJournal())
		}
		if content := conversationTestRead(t, selection); !strings.Contains(content, conversationFixtureAlias) || !strings.Contains(content, `"keep":true`) {
			t.Fatalf("selection not converged: %s", content)
		}
		if content := conversationTestRead(t, bindings); !strings.Contains(content, conversationFixtureAlias) || !strings.Contains(content, "byok-other") {
			t.Fatalf("bindings not converged: %s", content)
		}
	})

	t.Run("crash before commit removes journal and preserves JSON", func(t *testing.T) {
		store, selection, bindings := conversationFixture(t)
		journal := map[string]any{
			"version":     1,
			"hadOfficial": true,
			"hasMore":     false,
			"entries": []map[string]any{{
				"key":           conversationFixtureKey,
				"oldAlias":      "kimi-k3",
				"oldLevel":      "high",
				"modelAlias":    conversationFixtureAlias,
				"thinkingLevel": "low",
				"byok":          true,
			}},
		}
		raw, _ := json.Marshal(journal)
		conversationTestWrite(t, store.journalPath(), string(raw))

		got, err := store.run(context.Background(), conversationSQLiteRequest{Operation: "recover"})
		if err != nil || got.Recovered != "rolled_back" || store.hasPendingJournal() {
			t.Fatalf("recover = %+v, err = %v, pending = %t", got, err, store.hasPendingJournal())
		}
		if conversationTestRead(t, selection) != conversationFixtureSelection || conversationTestRead(t, bindings) != conversationFixtureBindings {
			t.Fatal("JSON files were modified during rollback recovery")
		}
	})

	t.Run("timeout after journal write recovers committed and rolled-back outcomes", func(t *testing.T) {
		store, selection, bindings := conversationFixture(t)
		previous := execCommandContext
		t.Cleanup(func() { execCommandContext = previous })
		// First invocation simulates a hung child after journal write + DB commit; recovery must converge JSON and succeed.
		firstCall := true
		execCommandContext = func(ctx context.Context, node string, args ...string) *exec.Cmd {
			if firstCall {
				firstCall = false
				journal, _ := json.Marshal(map[string]any{
					"version":     1,
					"hadOfficial": true,
					"hasMore":     false,
					"entries": []map[string]any{{
						"key":           conversationFixtureKey,
						"oldAlias":      "kimi-k3",
						"oldLevel":      "high",
						"modelAlias":    conversationFixtureAlias,
						"thinkingLevel": "low",
						"byok":          true,
					}},
				})
				conversationTestSQL(t, store, `
					db.prepare("UPDATE conversations SET extra_json = json_set(extra_json, '$.runtimePreferences.modelAlias', ?, '$.runtimePreferences.thinkingLevel', ?) WHERE agent_id = 'main' AND conversation_key = ?")
						.run('`+conversationFixtureAlias+`', 'low', `+"`"+conversationFixtureKey+"`"+`);
				`)
				conversationTestWrite(t, store.journalPath(), string(journal))
				return exec.CommandContext(ctx, node, "-e", `setInterval(() => {}, 1000)`)
			}
			return exec.CommandContext(ctx, node, args...)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		got, err := store.run(ctx, conversationSQLiteRequest{Operation: "rebind", ConversationKeys: []string{conversationFixtureKey}, ModelAlias: conversationFixtureAlias, ThinkingLevel: "low"})
		if err != nil || !slices.Equal(got.ConversationKeys, []string{conversationFixtureKey}) || store.hasPendingJournal() {
			t.Fatalf("post-timeout commit recovery = %+v, err = %v, pending = %t", got, err, store.hasPendingJournal())
		}
		if !strings.Contains(conversationTestRead(t, selection), conversationFixtureAlias) || !strings.Contains(conversationTestRead(t, bindings), conversationFixtureAlias) {
			t.Fatal("post-timeout commit recovery did not converge both JSON files")
		}

		// Second test simulates hung child after journal write BEFORE DB commit; recovery must clean journal and report cancelled.
		store2, selection2, bindings2 := conversationFixture(t)
		firstCall = true
		execCommandContext = func(ctx context.Context, node string, args ...string) *exec.Cmd {
			if firstCall {
				firstCall = false
				journal, _ := json.Marshal(map[string]any{
					"version":     1,
					"hadOfficial": true,
					"hasMore":     false,
					"entries": []map[string]any{{
						"key":           conversationFixtureKey,
						"oldAlias":      "kimi-k3",
						"oldLevel":      "high",
						"modelAlias":    conversationFixtureAlias,
						"thinkingLevel": "low",
						"byok":          true,
					}},
				})
				conversationTestWrite(t, store2.journalPath(), string(journal))
				return exec.CommandContext(ctx, node, "-e", `setInterval(() => {}, 1000)`)
			}
			return exec.CommandContext(ctx, node, args...)
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel2()
		if _, err := store2.run(ctx2, conversationSQLiteRequest{Operation: "rebind", ConversationKeys: []string{conversationFixtureKey}, ModelAlias: conversationFixtureAlias, ThinkingLevel: "low"}); err == nil || err.Error() != "conversation_rebind_cancelled" || store2.hasPendingJournal() {
			t.Fatalf("post-timeout rollback recovery = %v, pending = %t", err, store2.hasPendingJournal())
		}
		if conversationTestRead(t, selection2) != conversationFixtureSelection || conversationTestRead(t, bindings2) != conversationFixtureBindings {
			t.Fatal("post-timeout rollback recovery modified JSON files")
		}
	})

	t.Run("pre-cancelled context returns accurate error code", func(t *testing.T) {
		store, _, _ := conversationFixture(t)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.run(cancelled, conversationSQLiteRequest{Operation: "rebind"}); err == nil || err.Error() != "conversation_rebind_cancelled" {
			t.Fatalf("cancelled rebind = %v", err)
		}
		if _, err := store.run(cancelled, conversationSQLiteRequest{Operation: "list", Limit: 10}); err == nil || err.Error() != "conversation_database_unavailable" {
			t.Fatalf("cancelled list = %v", err)
		}
	})
}
