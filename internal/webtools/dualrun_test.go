package webtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/webtools"
)

// This file is the dual run: the same fixtures are pushed through the legacy
// 0.12.1 adapter (testdata/legacy-run.mjs) and through this package, and the
// answers are compared field by field. A difference is a real behavioural
// difference — the Go side is not graded against expectations written next to it.
//
// Regenerate the legacy side with:
//
//	bun internal/webtools/testdata/legacy-run.mjs \
//	    internal/webtools/testdata/cases.json \
//	    internal/webtools/testdata/legacy-observations.json

const (
	dualCasesFile  = "testdata/cases.json"
	dualLegacyFile = "testdata/legacy-observations.json"
)

type dualFixture struct {
	Search []struct {
		Label string         `json:"label"`
		Body  map[string]any `json:"body"`
	} `json:"search"`
	SearchErrors []struct {
		Label  string `json:"label"`
		Status int    `json:"status"`
	} `json:"searchErrors"`
	Fetch []struct {
		Label string         `json:"label"`
		URL   string         `json:"url"`
		Body  map[string]any `json:"body"`
	} `json:"fetch"`
	URLGate  []string `json:"urlGate"`
	Requests []struct {
		Label       string         `json:"label"`
		Kind        string         `json:"kind"`
		Request     map[string]any `json:"request"`
		SearchDepth string         `json:"searchDepth"`
	} `json:"requests"`
}

type dualLegacy struct {
	Search []struct {
		Label  string          `json:"label"`
		Answer json.RawMessage `json:"answer"`
		Code   string          `json:"code"`
		Status int             `json:"status"`
	} `json:"search"`
	SearchErrors []struct {
		Label        string `json:"label"`
		Code         string `json:"code"`
		Status       int    `json:"status"`
		VendorStatus int    `json:"vendorStatus"`
	} `json:"searchErrors"`
	Fetch []struct {
		Label  string  `json:"label"`
		Page   *string `json:"page"`
		Code   string  `json:"code"`
		Status int     `json:"status"`
	} `json:"fetch"`
	URLGate []struct {
		Input    string `json:"input"`
		Accepted bool   `json:"accepted"`
	} `json:"urlGate"`
	Requests []struct {
		Label    string `json:"label"`
		Accepted bool   `json:"accepted"`
		Code     string `json:"code"`
		Status   int    `json:"status"`
	} `json:"requests"`
}

func loadDual(t *testing.T) (dualFixture, dualLegacy) {
	t.Helper()
	raw, err := os.ReadFile(dualCasesFile)
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}
	var fixture dualFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode cases: %v", err)
	}
	rawLegacy, err := os.ReadFile(dualLegacyFile)
	if err != nil {
		t.Fatalf("read legacy observations: %v (run the bun command in this file's comment)", err)
	}
	var legacy dualLegacy
	if err := json.Unmarshal(rawLegacy, &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	return fixture, legacy
}

// mockVendor answers one case's body from loopback.
func mockVendor(t *testing.T, status int, body map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode vendor body: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestDualRunMatchesLegacyWebAdapter(t *testing.T) {
	if _, err := os.Stat(dualLegacyFile); err != nil {
		t.Skip("legacy observations not generated; run the bun command in this file's comment")
	}
	fixture, legacy := loadDual(t)
	if len(fixture.Search) != len(legacy.Search) {
		t.Fatalf("search case count %d want %d", len(fixture.Search), len(legacy.Search))
	}

	for index, testCase := range fixture.Search {
		testCase := testCase
		want := legacy.Search[index]
		t.Run("search/"+testCase.Label, func(t *testing.T) {
			if want.Label != testCase.Label {
				t.Fatalf("legacy order changed: %q at %d", want.Label, index)
			}
			endpoint := mockVendor(t, 200, testCase.Body)
			answer, err := webtools.Search(context.Background(),
				webtools.SearchRequest{TextQuery: "how does it work"},
				webtools.Options{APIKey: vendorKey, Endpoint: endpoint, Timeout: 5e9})
			if want.Code != "" {
				// The legacy side refused; this package must refuse with the same
				// code and the same native status.
				var typed *webtools.Error
				if !errors.As(err, &typed) {
					t.Fatalf("accepted; the legacy side reported %s", want.Code)
				}
				if typed.Code != want.Code || typed.Status != want.Status {
					t.Fatalf("refusal %s/%d want %s/%d", typed.Code, typed.Status, want.Code, want.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused with %v; the legacy side accepted", err)
			}
			encoded, err := json.Marshal(answer)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var got, expected any
			if json.Unmarshal(encoded, &got) != nil || json.Unmarshal(want.Answer, &expected) != nil {
				t.Fatalf("decode both sides")
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("answer differs\n got %s\nwant %s", encoded, want.Answer)
			}
		})
	}

	for index, testCase := range fixture.SearchErrors {
		testCase := testCase
		want := legacy.SearchErrors[index]
		t.Run("status/"+testCase.Label, func(t *testing.T) {
			endpoint := mockVendor(t, testCase.Status, map[string]any{"detail": "vendor said no"})
			_, err := webtools.Search(context.Background(),
				webtools.SearchRequest{TextQuery: "x"},
				webtools.Options{APIKey: vendorKey, Endpoint: endpoint, Timeout: 5e9})
			var typed *webtools.Error
			if !errors.As(err, &typed) {
				t.Fatalf("no typed error: %v", err)
			}
			if typed.Code != want.Code || typed.Status != want.Status || typed.VendorStatus != want.VendorStatus {
				t.Fatalf("got %s/%d/%d want %s/%d/%d", typed.Code, typed.Status, typed.VendorStatus,
					want.Code, want.Status, want.VendorStatus)
			}
		})
	}

	for index, testCase := range fixture.Fetch {
		testCase := testCase
		want := legacy.Fetch[index]
		t.Run("fetch/"+testCase.Label, func(t *testing.T) {
			endpoint := mockVendor(t, 200, testCase.Body)
			page, err := webtools.Fetch(context.Background(),
				webtools.FetchRequest{URL: testCase.URL},
				webtools.Options{APIKey: vendorKey, Endpoint: endpoint, Timeout: 5e9})
			if want.Code != "" {
				var typed *webtools.Error
				if !errors.As(err, &typed) {
					t.Fatalf("accepted; the legacy side reported %s", want.Code)
				}
				if typed.Code != want.Code || typed.Status != want.Status {
					t.Fatalf("refusal %s/%d want %s/%d", typed.Code, typed.Status, want.Code, want.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused with %v", err)
			}
			if want.Page == nil || page != *want.Page {
				t.Fatalf("page %q want %v", page, want.Page)
			}
		})
	}

	for index, raw := range fixture.URLGate {
		want := legacy.URLGate[index]
		_, accepted := webtools.NormalizeServiceURL(raw)
		if accepted != want.Accepted {
			t.Fatalf("urlGate %q: accepted=%v want %v", raw, accepted, want.Accepted)
		}
	}

	for index, testCase := range fixture.Requests {
		testCase := testCase
		want := legacy.Requests[index]
		t.Run("request/"+testCase.Label, func(t *testing.T) {
			// Port 1 is closed, so an accepted request reaches the transport and
			// fails there; the point of this group is the pre-flight refusal.
			options := webtools.Options{APIKey: vendorKey, Endpoint: "http://127.0.0.1:1",
				SearchDepth: testCase.SearchDepth, Timeout: 5e9}
			var err error
			if testCase.Kind == "search" {
				_, err = webtools.Search(context.Background(),
					webtools.SearchRequest{TextQuery: stringField(testCase.Request, "text_query")}, options)
			} else {
				_, err = webtools.Fetch(context.Background(),
					webtools.FetchRequest{URL: stringField(testCase.Request, "url")}, options)
			}
			if want.Accepted {
				if err == nil {
					t.Fatalf("the legacy side accepted and this one did not fail either")
				}
				return
			}
			var typed *webtools.Error
			if !errors.As(err, &typed) {
				t.Fatalf("accepted; the legacy side reported %s", want.Code)
			}
			if typed.Code != want.Code || typed.Status != want.Status {
				t.Fatalf("refusal %s/%d want %s/%d", typed.Code, typed.Status, want.Code, want.Status)
			}
		})
	}
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func TestLegacyWebAdapterObservationsAreReproducible(t *testing.T) {
	if os.Getenv("DUAL_RUN_REGEN") != "1" {
		t.Skip("set DUAL_RUN_REGEN=1 to re-run the legacy adapter")
	}
	bun := findWebtoolsBun()
	if bun == "" {
		t.Skip("no bun executable found")
	}
	output := filepath.Join(t.TempDir(), "legacy.json")
	command := exec.Command(bun, "run", "testdata/legacy-run.mjs", dualCasesFile, output)
	command.Dir = "."
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("legacy run failed: %v\n%s", err, combined)
	}
	fresh, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	committed, err := os.ReadFile(dualLegacyFile)
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}
	if string(fresh) != string(committed) {
		t.Fatalf("the committed observations no longer match a fresh run")
	}
}

func findWebtoolsBun() string {
	for _, candidate := range []string{
		os.Getenv("BUN"),
		filepath.Join("..", "..", "..", "..", ".bun", "bun-1.4.4"),
		"bun",
	} {
		if candidate == "" {
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved
		}
	}
	return ""
}
