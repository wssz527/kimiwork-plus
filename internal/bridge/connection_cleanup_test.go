package bridge

import (
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func stagedRefs(t *testing.T, d core.Document) []string {
	t.Helper()
	refs := []string{}
	for _, row := range core.Rows(d["connections"]) {
		if ref := core.String(core.Object(row)["apiKeyRef"]); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// prepareStaged stages one connection credential the way the panel save does
// and returns the document that is ready to commit together with the refs it
// created.
func prepareStaged(t *testing.T, s *Service, store *core.Store) (core.Document, string, []string) {
	t.Helper()
	_, rev, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	d, rev, err := store.Prepare(rev, func(d core.Document) error {
		changes, err := core.MergeConnections(d, []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic", "protocols": []any{"chat"}}})
		if err != nil {
			return err
		}
		if err = core.Validate(d); err != nil {
			return err
		}
		staged, err = s.stageCredentials(d, changes)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 {
		t.Fatalf("staged refs = %+v", staged)
	}
	return d, rev, staged
}

type conflictingWriteKeys struct {
	testKeys
	path     string
	document core.Document
}

func (k *conflictingWriteKeys) Set(ref, value string) error {
	k.testKeys[ref] = value
	return core.AtomicJSON(k.path, k.document)
}

func TestConfigureDiscardsStagedCredentialsAfterExternalConflict(t *testing.T) {
	for _, endpoint := range []string{"/api/connections", "/api/workspace"} {
		t.Run(endpoint, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			store, err := core.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			external, rev, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			external["subagents"] = core.Document{"mode": "custom", "models": []any{}}
			keys := &conflictingWriteKeys{testKeys: testKeys{"existing": "retained"}, path: filepath.Join(dir, "gateway.json"), document: external}
			service := &Service{Store: store, StateDir: dir, Keys: CacheKeys(keys)}
			_, err = service.configure(endpoint, core.Document{"revision": rev, "models": []any{}, "connections": []any{
				core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic", "protocols": []any{"chat"}},
			}})
			if !errors.Is(err, core.ErrConflict) {
				t.Fatalf("external conflict not reported: %v", err)
			}
			if len(keys.testKeys) != 1 || keys.testKeys["existing"] != "retained" {
				t.Fatalf("failed configure retained staged credentials: %v", keys.testKeys)
			}
		})
	}
}

func TestFailedCommitDiscardsUnreferencedStagedCredentials(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{})
	s := &Service{Store: store, StateDir: dir, Keys: keys}
	d, rev, staged := prepareStaged(t, s, store)
	// A concurrent save moves the revision on, so this document's commit fails
	// before the recovery journal is written.
	other, _, err := store.Prepare(rev, func(d core.Document) error {
		d["subagents"] = core.Document{"mode": "custom", "models": []any{}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Commit(other, rev); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Commit(d, rev); err == nil {
		t.Fatal("the stale commit succeeded")
	}
	s.discardStagedCredentials(staged)
	if value, err := keys.Get(staged[0]); err != nil || value != "" {
		t.Fatalf("an unreferenced staged credential survived the failed commit: %q %v", value, err)
	}
}

func TestFailedCommitKeepsStagedCredentialsTheJournalApplies(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{})
	s := &Service{Store: store, StateDir: dir, Keys: keys}
	d, rev, staged := prepareStaged(t, s, store)
	if err := core.AtomicJSON(filepath.Join(dir, "gateway.json.journal"), core.Document{"before": rev, "after": d}); err != nil {
		t.Fatal(err)
	}
	s.discardStagedCredentials(staged)
	if value, err := keys.Get(staged[0]); err != nil || value != "synthetic" {
		t.Fatalf("a credential the recovery journal applies was deleted: %q %v", value, err)
	}
	reopened, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := reopened.Read()
	if err != nil {
		t.Fatal(err)
	}
	refs := stagedRefs(t, applied)
	if len(refs) != 1 || refs[0] != staged[0] {
		t.Fatalf("the applied journal lost the staged reference: %+v", refs)
	}
	value, err := credentialFor(core.Object(core.Rows(applied["connections"])[0]), keys)
	if err != nil || value != "synthetic" {
		t.Fatalf("the applied configuration cannot read its credential: %q %v", value, err)
	}
}

func TestCommittedConfigurationKeepsItsCredentialWhenApplyFails(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := CacheKeys(testKeys{})
	s := &Service{Store: store, StateDir: dir, Keys: keys, OnConfig: func(core.Document) error { return errors.New("apply_failed") }}
	_, rev, _ := store.Read()
	_, err = s.configure("/api/connections", core.Document{"revision": rev, "connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic", "protocols": []any{"chat"}}}})
	if err == nil || err.Error() != "apply_failed" {
		t.Fatalf("apply failure not reported: %v", err)
	}
	saved, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	refs := stagedRefs(t, saved)
	if len(refs) != 1 {
		t.Fatalf("the committed configuration lost its reference: %+v", refs)
	}
	value, err := keys.Get(refs[0])
	if err != nil || value != "synthetic" {
		t.Fatalf("a committed credential was deleted after apply failed: %q %v", value, err)
	}
}

func TestFailedStageLeavesNoCredentialOrReference(t *testing.T) {
	dir := t.TempDir() + "/state"
	store, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := &alwaysFailingWriteKeys{}
	s := &Service{Store: store, StateDir: dir, Keys: keys}
	_, rev, _ := store.Read()
	_, err = s.configure("/api/workspace", core.Document{"revision": rev, "models": []any{}, "connections": []any{core.Document{"id": "local", "name": "Local", "baseURL": "http://127.0.0.1:9999/v1", "apiKey": "synthetic", "protocols": []any{"chat"}}}})
	if err == nil || err.Error() != "denied" {
		t.Fatalf("the failing store staged a credential: %v", err)
	}
	if len(keys.testKeys) != 0 {
		t.Fatalf("a failed stage retained a credential: %+v", keys.testKeys)
	}
	saved, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(stagedRefs(t, saved)) != 0 {
		t.Fatal("a failed stage committed a reference")
	}
}

func TestConnectionDeleteCascadeIsAtomicAndCleansCredentials(t *testing.T) {
	const path = "/api/connections/delete"
	s := newConfigPanelTestService(t, testKeys{"retired-ref": "retired-test-key", "keep-ref": "kept-test-key"})
	_, rev, _ := s.Store.Read()
	kept := core.Document{"id": "keep", "name": "Kept", "baseURL": "https://kept.example/v1", "apiKeyEnv": "KEEP_TEST_KEY", "apiKeyRef": "keep-ref"}
	before, rev, err := s.Store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{"id": "retire", "name": "Retired", "baseURL": "https://retired.example/v1", "apiKeyRef": "retired-ref"}, kept,
		}
		d["models"] = []any{
			core.Document{"id": "retired-model", "connectionId": "retire", "protocol": "responses", "contextWindow": 32000, "tools": true},
			core.Document{"id": "kept-model", "connectionId": "keep", "protocol": "responses", "contextWindow": 32000, "failover": []any{core.Document{"connectionId": "retire"}}},
		}
		d["subagents"] = core.Document{"mode": "force", "model": "retired-model"}
		d["k3CompatibleModelId"] = "retired-model"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var applied, scheduled int
	s.OnConfig = func(d core.Document) error {
		applied++
		if stored, _, err := s.Store.Read(); err != nil || core.Revision(d) != core.Revision(stored) {
			t.Fatalf("OnConfig called before commit: %v", err)
		}
		return nil
	}
	s.ScheduleAutoSync = func(gotPath string, prev, next core.Document) {
		scheduled++
		if applied != 1 || gotPath != path || core.Revision(prev) != core.Revision(before) || len(core.Rows(next["models"])) != 1 {
			t.Fatalf("incorrect auto-sync invocation for committed delete: %s %v %v", gotPath, prev, next)
		}
	}
	for _, tc := range []struct {
		body   core.Document
		status int
		code   string
	}{
		{core.Document{"revision": "stale", "connectionId": "retire", "cascade": true}, http.StatusConflict, "configuration_conflict"},
		{core.Document{"revision": rev, "connectionId": ""}, http.StatusBadRequest, "invalid_connections"},
		{core.Document{"revision": rev, "connectionId": "bad/id"}, http.StatusBadRequest, "invalid_connections"},
		{core.Document{"revision": rev, "connectionId": "missing"}, http.StatusConflict, "connection_unavailable"},
		{core.Document{"revision": rev, "connectionId": "retire"}, http.StatusConflict, "connection_in_use"},
	} {
		out := callConfigPanel(t, s, http.MethodPost, path, tc.body, tc.status)
		if core.Object(out["error"])["code"] != tc.code {
			t.Fatal(out)
		}
	}
	if _, unchanged, err := s.Store.Read(); err != nil || unchanged != rev || applied != 0 || scheduled != 0 {
		t.Fatalf("rejected delete had side effects: %v", err)
	}
	out := callConfigPanel(t, s, http.MethodPost, path, core.Document{"revision": rev, "connectionId": "retire", "cascade": true}, http.StatusOK)
	removed := core.Object(out["removed"])
	if out["ok"] != true || removed["connectionId"] != "retire" || removed["subagentsReset"] != true ||
		!reflect.DeepEqual(removed["models"], []any{"retired-model"}) ||
		!reflect.DeepEqual(removed["failoverStripped"], []any{"kept-model"}) || removed["apiKeyRef"] != nil {
		t.Fatalf("unexpected delete receipt: %v", removed)
	}
	d, afterRev, err := s.Store.Read()
	if err != nil || core.Object(out["state"])["revision"] != afterRev || applied != 1 || scheduled != 1 ||
		!reflect.DeepEqual(core.Rows(d["connections"]), []any{map[string]any(kept)}) ||
		d["k3CompatibleModelId"] != nil || core.Object(d["subagents"])["mode"] != "inherit" {
		t.Fatalf("delete not applied in one save: %v %v", d, err)
	}
	if key, _ := s.Keys.Get("retired-ref"); key != "" {
		t.Fatal("removed connection credential was not cleaned")
	}
	if key, _ := s.Keys.Get("keep-ref"); key != "kept-test-key" {
		t.Fatal("surviving connection credential was lost")
	}
}

func TestConnectionDeleteKeepsSharedCredential(t *testing.T) {
	const ref = "shared-test-ref"
	s := newConfigPanelTestService(t, testKeys{ref: "shared-test-key"})
	_, rev, _ := s.Store.Read()
	_, rev, err := s.Store.Update(rev, func(d core.Document) error {
		d["connections"] = []any{
			core.Document{"id": "retire", "name": "Retired", "baseURL": "https://retired.example/v1", "apiKeyRef": ref},
			core.Document{"id": "keep", "name": "Kept", "baseURL": "https://kept.example/v1", "apiKeyRef": ref},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := callConfigPanel(t, s, http.MethodPost, "/api/connections/delete", core.Document{"revision": rev, "connectionId": "retire"}, http.StatusOK)
	if removed := core.Object(out["removed"]); !reflect.DeepEqual(removed["models"], []any{}) || !reflect.DeepEqual(removed["failoverStripped"], []any{}) {
		t.Fatalf("an unused connection reported removed models: %v", removed)
	}
	if key, _ := s.Keys.Get(ref); key != "shared-test-key" {
		t.Fatal("delete removed a key that another connection uses")
	}
}
