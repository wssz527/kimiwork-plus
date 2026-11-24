package protocol_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

var compatibleModelIDPattern = regexp.MustCompile(`^kimi-k3-kwp-[0-9a-f]{64}$`)

// --- identity mapping ------------------------------------------------------

// The generated id is what Kimi's own parser classifies: the final segment
// starts with "kimi-" (a Chat model) and with "kimi-k3" (offered for existing
// K3 conversations). The digest fixes the length, so nested and very long
// saved ids stay importable.
func TestKimiCompatibleModelID(t *testing.T) {
	const id = "group/kimi-example"
	generated := protocol.KimiCompatibleModelID(id)
	if !compatibleModelIDPattern.MatchString(generated) {
		t.Fatalf("generated id %q", generated)
	}
	if generated != protocol.KimiCompatibleModelID(id) {
		t.Fatal("generated id is not deterministic")
	}
	if protocol.ModelFamily(generated) != protocol.ProtocolChat {
		t.Fatalf("Kimi classifies %q as %s", generated, protocol.ModelFamily(generated))
	}

	saved := []string{
		"kimi-alpha",
		"group/kimi-example",
		"vendor/subgroup/kimi-nested",
		strings.Repeat("x", 512),
		"kimi-k3-kwp-not-a-digest",
		"byok-kth-kimi/kimi-real",
	}
	seen := map[string]bool{}
	for _, logical := range saved {
		value := protocol.KimiCompatibleModelID(logical)
		if len(value) != len("kimi-k3-kwp-")+64 || !compatibleModelIDPattern.MatchString(value) {
			t.Fatalf("id for %q = %q", logical, value)
		}
		last := value[strings.LastIndex(value, "/")+1:]
		if !strings.HasPrefix(last, "kimi-k3") {
			t.Fatalf("final segment of %q = %q", value, last)
		}
		if value == logical {
			t.Fatalf("generated id equals the saved id %q", logical)
		}
		if seen[value] {
			t.Fatalf("duplicate generated id for %q", logical)
		}
		seen[value] = true
	}
}

// --- catalog ---------------------------------------------------------------

type catalogModel struct {
	Name string `json:"name"`
}

type nativeCatalog struct {
	Kimi struct {
		Type   string                  `json:"type"`
		API    string                  `json:"api"`
		Models map[string]catalogModel `json:"models"`
	} `json:"kimi"`
	Responses struct {
		Type   string                  `json:"type"`
		API    string                  `json:"api"`
		Models map[string]catalogModel `json:"models"`
	} `json:"responses"`
}

func decodeNativeCatalog(t *testing.T, body []byte) nativeCatalog {
	t.Helper()
	var catalog nativeCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("catalog JSON: %v\n%s", err, body)
	}
	return catalog
}

func TestNativeCatalogCompatibleOffersEveryRowFromOneKimiGroup(t *testing.T) {
	display := "DeepSeek V4.1 Flash · Magpie"
	models := []protocol.Model{
		chatModel("kimi-alpha", func(m *protocol.Model) { m.Name = &display }),
		responsesModel("vendor/responses"),
		chatModel("vendor/kimi-beta"),
	}
	const api = "http://127.0.0.1:1234/v1"

	plain := decodeNativeCatalog(t, protocol.NativeCatalog(models, api))
	if _, ok := plain.Kimi.Models["kimi-alpha"]; !ok {
		t.Fatalf("plain catalog keys = %v", plain.Kimi.Models)
	}
	if plain.Kimi.Models["kimi-alpha"].Name != display {
		t.Fatalf("plain display name = %q", plain.Kimi.Models["kimi-alpha"].Name)
	}
	if _, ok := plain.Responses.Models["vendor/responses"]; !ok {
		t.Fatalf("plain responses group = %v", plain.Responses.Models)
	}

	body := protocol.NativeCatalogCompatible(models, api)
	compatible := decodeNativeCatalog(t, body)
	alphaKey := protocol.KimiCompatibleModelID("kimi-alpha")
	betaKey := protocol.KimiCompatibleModelID("vendor/kimi-beta")
	responsesKey := protocol.KimiCompatibleModelID("vendor/responses")
	if len(compatible.Kimi.Models) != len(models) {
		t.Fatalf("compatible kimi group = %v", compatible.Kimi.Models)
	}
	if compatible.Kimi.Type != "kimi" {
		t.Fatalf("compatible group type = %q", compatible.Kimi.Type)
	}
	// The Responses group is gone: every row is served from the one `kimi` group,
	// so Kimi never has to resolve a model family from the name.
	if len(compatible.Responses.Models) != 0 {
		t.Fatalf("compatible catalog still has a responses group: %v", compatible.Responses.Models)
	}
	if got := compatible.Kimi.Models[alphaKey].Name; got != display {
		t.Fatalf("display name for a named model = %q", got)
	}
	// A model without a display name falls back to its saved id, never to the
	// generated one, so the menu stays readable.
	if got := compatible.Kimi.Models[betaKey].Name; got != "vendor/kimi-beta" {
		t.Fatalf("display fallback = %q", got)
	}
	if got := compatible.Kimi.Models[responsesKey].Name; got != "vendor/responses" {
		t.Fatalf("responses display fallback = %q", got)
	}
	if _, ok := compatible.Kimi.Models["kimi-alpha"]; ok {
		t.Fatalf("the saved id is still advertised under the opt-in: %v", compatible.Kimi.Models)
	}
	if compatible.Kimi.API != api {
		t.Fatalf("api changed: %+v", compatible)
	}
	// The rows keep the order the user saved them in.
	alphaAt := strings.Index(string(body), `"`+alphaKey+`"`)
	responsesAt := strings.Index(string(body), `"`+responsesKey+`"`)
	betaAt := strings.Index(string(body), `"`+betaKey+`"`)
	if alphaAt < 0 || responsesAt < 0 || betaAt < 0 || !(alphaAt < responsesAt && responsesAt < betaAt) {
		t.Fatalf("catalog order changed: %d %d %d", alphaAt, responsesAt, betaAt)
	}

	// The OpenAI list is a different contract and never carries generated ids.
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(protocol.OpenAIModels(models), &list); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, row := range list.Data {
		ids = append(ids, row.ID)
	}
	if !reflect.DeepEqual(ids, []string{"kimi-alpha", "vendor/responses", "vendor/kimi-beta"}) {
		t.Fatalf("OpenAI ids = %v", ids)
	}
}

// --- gateway ---------------------------------------------------------------

func fetchCatalog(t *testing.T, url string) ([]byte, nativeCatalog) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, url+"/models/api.json", nil)
	request.Header.Set("Authorization", "Bearer "+gatewayKey)
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer reply.Body.Close()
	raw, _ := io.ReadAll(reply.Body)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("catalog status %d: %s", reply.StatusCode, raw)
	}
	return raw, decodeNativeCatalog(t, raw)
}

func chatReply(w http.ResponseWriter, _ *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
}

func waitForUsageEntry(t *testing.T, mu *sync.Mutex, entries *[]protocol.UsageEntry) protocol.UsageEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		for _, entry := range *entries {
			if entry.Status != "started" {
				mu.Unlock()
				return entry
			}
		}
		mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no terminal usage entry was written")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A generated id must reach the same row as the saved id, with the real
// upstream id and the logical id preserved in the record and the budget gate.
func TestGatewayRoutesCompatibleChatIDs(t *testing.T) {
	fake := newUpstream(t, chatReply)
	upstreamID := "vendor-real-model"
	chat := chatModel("kimi-alpha", func(m *protocol.Model) { m.UpstreamModelID = &upstreamID })
	gate := &fakeGate{}
	var mu sync.Mutex
	var entries []protocol.UsageEntry
	sink := protocol.UsageSinkFunc(func(entry protocol.UsageEntry) error {
		mu.Lock()
		entries = append(entries, entry)
		mu.Unlock()
		return nil
	})
	gateway, url := startGateway(t, fake.server.URL, []protocol.Model{chat, responsesModel("vendor/responses")},
		func(c *protocol.Config) {
			c.KimiK3Compatibility = true
			c.Budget = gate
			c.UsageSink = sink
		})
	compatible := protocol.KimiCompatibleModelID("kimi-alpha")

	_, catalog := fetchCatalog(t, url)
	if _, ok := catalog.Kimi.Models[compatible]; !ok {
		t.Fatalf("catalog does not offer %q: %+v", compatible, catalog.Kimi.Models)
	}
	if got := catalog.Kimi.Models[compatible].Name; got != "kimi-alpha" {
		t.Fatalf("display name = %q", got)
	}

	reply, body := post(t, url, "/chat/completions",
		map[string]any{"model": compatible, "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("generated id: %d %s", reply.StatusCode, body)
	}
	captured := fake.captured()
	if len(captured) != 1 || !strings.Contains(captured[0].Body, `"model":"`+upstreamID+`"`) {
		t.Fatalf("upstream saw %+v", captured)
	}
	record := waitForRecentRecord(t, gateway)
	if record.Model != "kimi-alpha" {
		t.Fatalf("record model = %q", record.Model)
	}
	if record.UpstreamModelID == nil || *record.UpstreamModelID != upstreamID {
		t.Fatalf("record upstream = %v", record.UpstreamModelID)
	}
	if len(gate.requests) != 1 || gate.requests[0].Model != "kimi-alpha" {
		t.Fatalf("budget admissions = %+v", gate.requests)
	}
	if len(gate.settled) != 1 || gate.settled[0].Model != "kimi-alpha" {
		t.Fatalf("budget settlements = %+v", gate.settled)
	}
	// The ledger receipt carries the real upstream id and the logical id as the
	// alias, so usage is never attributed to the generated name.
	terminal := waitForUsageEntry(t, &mu, &entries)
	if terminal.ModelID != upstreamID || terminal.ModelAlias != "kimi-alpha" {
		t.Fatalf("usage entry = %+v", terminal)
	}

	// The provider prefixes the client already uses are stripped before the
	// generated id is resolved.
	for _, prefixed := range []string{
		"byok-kth-kimi/" + compatible,
		"byok-3d8250d1-cbbc-4f1f-991e-be98b216f34f/" + compatible,
	} {
		reply, body = post(t, url, "/chat/completions",
			map[string]any{"model": prefixed, "messages": []any{}}, nil)
		if reply.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", prefixed, reply.StatusCode, body)
		}
	}
	// The saved id keeps working unchanged.
	reply, body = post(t, url, "/chat/completions",
		map[string]any{"model": "kimi-alpha", "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("saved id: %d %s", reply.StatusCode, body)
	}
	if len(fake.captured()) != 4 {
		t.Fatalf("upstream saw %d requests", len(fake.captured()))
	}
}

// Strict protocol matching and unknown-name handling are unchanged: a generated
// Chat id on the Responses route is a mismatch, and an unbound generated id is
// not a model.
func TestCompatibleIDsPreserveProtocolMatching(t *testing.T) {
	fake := newUpstream(t, chatReply)
	_, url := startGateway(t, fake.server.URL, []protocol.Model{chatModel("kimi-alpha"), responsesModel("vendor/responses")},
		func(c *protocol.Config) { c.KimiK3Compatibility = true })
	compatible := protocol.KimiCompatibleModelID("kimi-alpha")

	reply, body := post(t, url, "/responses", map[string]any{"model": compatible, "input": "hi"}, nil)
	if reply.StatusCode != http.StatusConflict || errorCode(t, body) != "model_protocol_mismatch" {
		t.Fatalf("generated id on responses: %d %s", reply.StatusCode, errorCode(t, body))
	}
	reply, body = post(t, url, "/chat/completions",
		map[string]any{"model": protocol.KimiCompatibleModelID("kimi-not-enabled"), "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusConflict || errorCode(t, body) != "model_not_enabled_for_kimi" {
		t.Fatalf("unbound generated id: %d %s", reply.StatusCode, errorCode(t, body))
	}
	if len(fake.captured()) != 0 {
		t.Fatalf("refused requests reached the upstream")
	}
}

// Turning the opt-in off must stop advertising the generated ids but keep them
// routable, because conversations bound while it was on are only migrated later.
func TestCompatibleIDsStayRoutableAfterDisable(t *testing.T) {
	fake := newUpstream(t, chatReply)
	models := []protocol.Model{chatModel("kimi-alpha")}
	compatible := protocol.KimiCompatibleModelID("kimi-alpha")

	_, enabledURL := startGateway(t, fake.server.URL, models,
		func(c *protocol.Config) { c.KimiK3Compatibility = true })
	disabledGateway, disabledURL := startGateway(t, fake.server.URL, models, nil)

	_, catalog := fetchCatalog(t, enabledURL)
	if _, ok := catalog.Kimi.Models[compatible]; !ok {
		t.Fatal("enabled catalog lacks the generated id")
	}
	_, catalog = fetchCatalog(t, disabledURL)
	if _, ok := catalog.Kimi.Models[compatible]; ok {
		t.Fatalf("disabled catalog still offers the generated id: %v", catalog.Kimi.Models)
	}
	if _, ok := catalog.Kimi.Models["kimi-alpha"]; !ok {
		t.Fatalf("disabled catalog lost the saved id: %v", catalog.Kimi.Models)
	}

	reply, body := post(t, disabledURL, "/chat/completions",
		map[string]any{"model": compatible, "messages": []any{}}, nil)
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("generated id after disable: %d %s", reply.StatusCode, body)
	}
	record := waitForRecentRecord(t, disabledGateway)
	if record.Model != "kimi-alpha" {
		t.Fatalf("record model = %q", record.Model)
	}
}
