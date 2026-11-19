package bridge

import (
	"path/filepath"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/webtools"
)

func TestWebCredentialCommitAndReopen(t *testing.T) {
	dir := t.TempDir()
	keys := CacheKeys(testKeys{})
	policies, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := &Native{service: &Service{StateDir: dir, Keys: keys}, policies: policies}
	if err := n.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer n.web.Close()
	result, err := n.webAction(core.Document{"action": "save", "provider": "tavily", "apiKey": "synthetic-tavily-key"})
	if err != nil {
		t.Fatal(err)
	}
	if !core.Object(result)["state"].(webtools.State).KeySet {
		t.Fatal("saved key is not readable")
	}
	ref := currentWebCredentialRef(filepath.Join(dir, "web-tools-settings.json"))
	actual, err := keys.store.Get(ref)
	if ref == "" || err != nil || actual != "synthetic-tavily-key" {
		t.Fatal("committed credential reference was not persisted")
	}

	reopenedPolicies, err := newIsolatedTestPolicies(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened := &Native{service: &Service{StateDir: dir, Keys: CacheKeys(keys.store)}, policies: reopenedPolicies}
	if err := reopened.initEnhancements(); err != nil {
		t.Fatal(err)
	}
	defer reopened.web.Close()
	if reopened.webKeys.ref != ref || !reopened.web.State(nil).KeySet {
		t.Fatal("reopening did not restore the committed key reference")
	}
}

func TestWebCredentialRollbackKeepsOldReference(t *testing.T) {
	store := testKeys{"tavily-old": "old"}
	key := webKey{keys: store, ref: "tavily-old"}
	if err := key.Set("new"); err != nil {
		t.Fatal(err)
	}
	staged := key.ref
	key.rollback()
	if key.ref != "tavily-old" || store[key.ref] != "old" {
		t.Fatal("rollback discarded the old reference")
	}
	if _, exists := store[staged]; exists {
		t.Fatal("rollback left a new orphan credential")
	}
}
