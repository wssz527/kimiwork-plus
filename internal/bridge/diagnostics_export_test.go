package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func TestDiagnosticsExportDropsSensitiveFields(t *testing.T) {
	log := newDiagnosticsLog(t.TempDir())
	n := &Native{diagnostics: log}
	n.logOperationStarted("operation-1", "connect-kimi")
	n.logOperationFinished("operation-1", "connect-kimi", diagnosticStageImportDirect, "prepared", "", 1500, kimi.ImportResult{
		OK:                  true,
		State:               kimi.ImportStateApplied,
		Models:              2,
		Applied:             true,
		RouteCleanupPending: true,
		SourceID:            "source-secret",
		Readback: &kimi.ImportReadback{
			ProfileDigest: "profile-secret",
			URL:           "https://secret.example/v1",
			CredentialRef: "credential-secret",
		},
	})

	state := core.Document{
		"kimiProcess": core.Document{"running": true, "state": "checked"},
		"integration": core.Document{
			"state": "applied", "reason": "", "imported": true, "applied": true, "adopted": true,
			"mode": "direct", "routeCleanupPending": false,
			"aliases":        map[string]string{"secret-alias": "alias-secret"},
			"directSourceId": "source-secret",
			"readback":       core.Document{"url": "https://secret.example/v1", "credentialRef": "credential-secret"},
			"desktop":        core.Document{"state": "running", "managed": true},
			"nativeImport":   core.Document{"ok": true, "state": "applied", "sourceId": "source-secret", "models": 2},
		},
		"profile":       kimi.ProfileStatus{Mode: kimi.ProfileModeOfficial, Implemented: true, ProfileDigest: "digest-secret"},
		"compatibility": kimi.CompatibilityReport{Status: "ok", CheckedAt: 77, Checks: kimi.CompatibilityChecks{Catalog: kimi.CatalogCompatCheck{Status: "ok", ModelCount: 2}}},
		"accounts":      core.Document{"state": "ready", "accounts": []AccountSummary{{ID: "user@example.com", Name: "Real Name"}}},
		"operation": core.Document{
			"id": "operation-1", "action": "prepare-native", "state": "prepared", "stage": diagnosticStageImportDirect,
			"startedAt": int64(1000), "finishedAt": int64(2500),
			"result": kimi.ImportResult{OK: true, State: kimi.ImportStateApplied, SourceID: "source-secret", Readback: &kimi.ImportReadback{URL: "https://secret.example/v1", CredentialRef: "credential-secret"}},
		},
		"someFutureState": "token-secret",
	}
	config := core.Document{
		"integrationMode": "direct",
		"models":          []any{core.Document{"id": "alpha"}},
		"connections":     []any{core.Document{"id": "local", "baseURL": "https://secret.example/v1"}},
	}

	exported := diagnosticsExportDocument(diagnosticsStateSummary(state, config), log.history())
	blob, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"source-secret", "secret.example", "credential-secret", "profile-secret",
		"digest-secret", "user@example.com", "Real Name", "token-secret", "alias-secret",
	} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("export leaked %q: %s", secret, blob)
		}
	}

	if exported["schemaVersion"] != diagnosticsSchemaVersion {
		t.Fatalf("schemaVersion %v", exported["schemaVersion"])
	}
	app := core.Object(exported["app"])
	if core.String(app["version"]) != Version || core.String(app["buildId"]) != BuildID || core.String(app["platform"]) == "" {
		t.Fatalf("app %+v", app)
	}
	redaction := core.Object(exported["redaction"])
	excluded, _ := redaction["excluded"].([]string)
	if core.String(redaction["note"]) == "" || len(excluded) == 0 {
		t.Fatalf("redaction note %+v", redaction)
	}
	summary := core.Object(exported["summary"])
	if summary["modelCount"] != 1 || summary["connectionCount"] != 1 || summary["integrationMode"] != "direct" {
		t.Fatalf("summary counts %+v", summary)
	}
	if receipt := core.Object(summary["nativeImport"]); receipt["ok"] != true || receipt["state"] != "applied" || receipt["models"] != 2 {
		t.Fatalf("native import receipt %+v", receipt)
	}
	if _, ok := core.Object(summary["nativeImport"])["sourceId"]; ok {
		t.Fatal("native import receipt kept the source id")
	}
	if operation := core.Object(summary["currentOperation"]); operation["action"] != "prepare-native" || operation["stage"] != diagnosticStageImportDirect {
		t.Fatalf("current operation %+v", operation)
	}
	events := exported["events"].([]diagnosticEvent)
	if len(events) != 2 || events[1].Import == nil || events[1].Import.State != kimi.ImportStateApplied {
		t.Fatalf("events %+v", events)
	}
	if operation := core.Object(summary["currentOperation"]); operation["import"] == nil {
		t.Fatal("current operation lost its sanitized import")
	}
}

func TestDiagnosticsExportSanitizesDirtyHistory(t *testing.T) {
	root := t.TempDir()
	writeRawHistory(t, root+"/diagnostics", dirtyHistory())
	exported := diagnosticsExportDocument(core.Document{"version": Version}, newDiagnosticsLog(root).history())
	blob, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"source-secret", "secret.example", "credential-secret", "token-abc",
		"evil.example", "etc/passwd", "/tmp/secret",
	} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("export kept %q from the history file: %s", secret, blob)
		}
	}
	events := exported["events"].([]diagnosticEvent)
	if len(events) != 3 || events[0].Version != "1.1.5" || events[0].BuildID != "a26ef4a" {
		t.Fatalf("export changed the recorded build identity: %+v", events)
	}
}

func TestDiagnosticsSummaryHandlesTriStateProcessAndProfileMaps(t *testing.T) {
	summary := diagnosticsStateSummary(core.Document{
		"kimiProcess": core.Document{"running": "yes", "state": "checked"},
		"profile":     map[string]any{"mode": "official", "implemented": true, "canReuseLogin": false, "profileDigest": "digest-secret"},
	}, nil)
	if process := core.Object(summary["process"]); process["running"] != nil {
		t.Fatalf("non-boolean running was published: %+v", process)
	}
	profile := core.Object(summary["profile"])
	if profile["mode"] != "official" || profile["implemented"] != true || profile["canReuseLogin"] != false || profile["reason"] != "" {
		t.Fatalf("profile map %+v", profile)
	}
	if _, ok := profile["profileDigest"]; ok {
		t.Fatal("profile digest was published")
	}

	unknown := diagnosticsStateSummary(core.Document{"kimiProcess": core.Document{"running": nil, "state": "unknown"}}, nil)
	if process := core.Object(unknown["process"]); process["running"] != nil {
		t.Fatalf("nil running was published as a value: %+v", process)
	}
	stopped := diagnosticsStateSummary(core.Document{"kimiProcess": core.Document{"running": false, "state": "checked"}}, nil)
	if process := core.Object(stopped["process"]); process["running"] != false {
		t.Fatalf("boolean running was dropped: %+v", process)
	}
}

func TestDiagnosticsCodeListKeepsReasonsButNotPaths(t *testing.T) {
	cases := map[string]string{
		"missing:getUserProfile,getByokProfile_or_byokSources.list": "missing:getUserProfile,getByokProfile_or_byokSources.list",
		"model_readback_api_missing,model_import_api_missing":       "model_readback_api_missing,model_import_api_missing",
		"missing:modelImportAPI":                                    "missing:modelImportAPI",
		"store_format_unknown":                                      "store_format_unknown",
		"":                                                          "",
		"missing:/etc/passwd":                                       "redacted",
		"https://secret.example/v1":                                 "redacted",
		"../../etc/passwd":                                          "redacted",
		"token-abc,/etc/passwd":                                     "redacted",
		"a.b":                                                       "a.b",
		".hidden":                                                   "redacted",
		"trailing.":                                                 "redacted",
	}
	for input, want := range cases {
		if got := diagnosticCodeList(input); got != want {
			t.Fatalf("diagnosticCodeList(%q) = %q, want %q", input, got, want)
		}
	}

	report := kimi.CompatibilityReport{
		Status: "degraded",
		AffectedFeatures: []kimi.AffectedFeature{
			{Feature: "model-sync", Reason: "model_readback_api_missing,https://secret.example/v1", WriteBlocked: true},
			{Feature: "account-switch", Reason: "account_profile_api_missing", WriteBlocked: true},
		},
		Checks: kimi.CompatibilityChecks{AccountAPI: kimi.AccountAPICompatCheck{Status: "fail", Reason: "missing:getUserProfile,byokSources.list"}},
	}
	compat := diagnosticsCompatibility(report)
	features := compat["affectedFeatures"].([]core.Document)
	if features[0]["reason"] != "redacted" {
		t.Fatalf("feature reason allowed a URL: %+v", features[0])
	}
	if features[1]["reason"] != "account_profile_api_missing" {
		t.Fatalf("feature reason %+v", features[1])
	}
	if reason := core.Object(core.Object(compat["checks"])["accountApi"])["reason"]; reason != "missing:getUserProfile,byokSources.list" {
		t.Fatalf("account api reason %v", reason)
	}
}

func TestDiagnosticsExportRouteUsesTheNativeExtraHandler(t *testing.T) {
	exportDir := t.TempDir()
	s := &Service{StateDir: t.TempDir(), ExportDir: exportDir}
	called := 0
	s.Extra = func(_ context.Context, path string, _ core.Document) (any, error) {
		called++
		if path != "/api/diagnostics/export" {
			return nil, errors.New("unexpected_path")
		}
		return core.Document{"schemaVersion": diagnosticsSchemaVersion}, nil
	}
	out, err := s.action(context.Background(), "GET", "/api/diagnostics/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || out.(core.Document)["schemaVersion"] != diagnosticsSchemaVersion {
		t.Fatalf("export was not routed to the native handler: %+v", out)
	}
	postOut, err := s.action(context.Background(), "POST", "/api/diagnostics/export", core.Document{})
	if err != nil {
		t.Fatalf("POST diagnostics export failed: %v", err)
	}
	doc := postOut.(core.Document)
	if doc["ok"] != true || !strings.HasPrefix(core.String(doc["path"]), exportDir) {
		t.Fatalf("unexpected POST diagnostics export response: %+v", doc)
	}
	if _, err := s.action(context.Background(), "PUT", "/api/diagnostics/export", core.Document{}); err == nil {
		t.Fatal("PUT to the export path was accepted")
	}

	s.Extra = nil
	if _, err := s.action(context.Background(), "GET", "/api/diagnostics/export", nil); err == nil || err.Error() != "native_preview_unavailable" {
		t.Fatalf("export without a native handler: %v", err)
	}
	if _, err := s.action(context.Background(), "POST", "/api/diagnostics/export", core.Document{}); err == nil || err.Error() != "native_preview_unavailable" {
		t.Fatalf("POST export without a native handler: %v", err)
	}
}
