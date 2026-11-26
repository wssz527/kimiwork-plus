package main

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/bridge"
)

func TestWindowNavScriptsCoverAllTargets(t *testing.T) {
	var targets []string
	for target := range windowNavScripts {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	if !slices.Equal(targets, []string{"accounts", "conversations", "updates"}) {
		t.Fatalf("unexpected nav targets: %v", targets)
	}
	for _, needle := range []string{"__kwpOpenAccounts", "nav-accounts", "nav-maintenance", "settings-tab-accounts"} {
		if !strings.Contains(openAccountsJS, needle) {
			t.Fatalf("accounts script is missing %q: %s", needle, openAccountsJS)
		}
	}
	if strings.Index(openAccountsJS, "nav-accounts") >= strings.Index(openAccountsJS, "nav-maintenance") {
		t.Fatal("account navigation must try the main page first")
	}
	for _, needle := range []string{"__kwpOpenConversations", "nav-conversations", "nav-maintenance", "conversations-panel", "scrollIntoView"} {
		if !strings.Contains(openConversationsJS, needle) {
			t.Fatalf("conversations script is missing %q: %s", needle, openConversationsJS)
		}
	}
	if strings.Index(openConversationsJS, "nav-conversations") >= strings.Index(openConversationsJS, "nav-maintenance") {
		t.Fatal("conversation navigation must try the top-level entry first")
	}
	for _, needle := range []string{"__kwpOpenUpdates", "nav-maintenance", "settings-tab-updates", "settings-tab-connection", "update-panel", "main.scrollTop", "getBoundingClientRect"} {
		if !strings.Contains(openUpdatesJS, needle) {
			t.Fatalf("updates script is missing %q: %s", needle, openUpdatesJS)
		}
	}
	if strings.Index(openUpdatesJS, "settings-tab-updates") >= strings.Index(openUpdatesJS, "settings-tab-connection") {
		t.Fatal("update navigation must try the dedicated updates tab first")
	}
	if strings.Contains(openUpdatesJS, "scrollIntoView") {
		t.Fatalf("updates script must scroll the main container instead of scrollIntoView: %s", openUpdatesJS)
	}
}

func TestWindowNavControllerQueuesLatestTarget(t *testing.T) {
	var scripts []string
	nav := newWindowNavController(
		true,
		"http://127.0.0.1:43001",
		"key-initial",
		nil,
		func(js string) { scripts = append(scripts, js) },
	)

	nav.RequestOpenAccounts()
	nav.RequestOpenConversations()
	nav.RequestOpenUpdates()
	if len(scripts) != 0 {
		t.Fatalf("nav scripts ran before the first navigation finished: %d", len(scripts))
	}
	nav.OnNavigationFinished()
	if len(scripts) != 1 || scripts[0] != openUpdatesJS {
		t.Fatalf("only the latest queued target must run, got %d scripts", len(scripts))
	}

	nav.RequestOpenAccounts()
	nav.RequestOpenConversations()
	nav.RequestOpenUpdates()
	if !slices.Equal(scripts[1:], []string{openAccountsJS, openConversationsJS, openUpdatesJS}) {
		t.Fatalf("loaded navigation must run each target immediately: %v", scripts)
	}

	nav.RequestNavigate("missing")
	if len(scripts) != 4 {
		t.Fatalf("unknown targets must not run a script, got %d", len(scripts))
	}
}

func TestWindowNavControllerSurvivesEndpointReloads(t *testing.T) {
	var urls, scripts []string
	nav := newWindowNavController(
		true,
		"http://127.0.0.1:43001",
		"key-initial",
		func(u string) { urls = append(urls, u) },
		func(js string) { scripts = append(scripts, js) },
	)
	nav.OnNavigationFinished()

	nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:43001", Key: "key-rotated", PID: 2})
	if len(urls) != 1 || urls[0] != "http://127.0.0.1:43001/#key=key-rotated" {
		t.Fatalf("key rotation should reload the same origin: %v", urls)
	}
	nav.RequestOpenUpdates()
	if len(scripts) != 1 || scripts[0] != openUpdatesJS {
		t.Fatalf("same-origin key rotation must not defer navigation requests: %d", len(scripts))
	}

	nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:43002", Key: "key-newport", PID: 3})
	if len(urls) != 2 || urls[1] != "http://127.0.0.1:43002/#key=key-newport" {
		t.Fatalf("cross-origin endpoint change should navigate: %v", urls)
	}
	nav.RequestOpenConversations()
	if len(scripts) != 1 {
		t.Fatalf("cross-origin reload must defer navigation requests until finish: %d", len(scripts))
	}
	nav.OnNavigationFinished()
	if len(scripts) != 2 || scripts[1] != openConversationsJS {
		t.Fatalf("deferred conversations navigation did not run after reload: %v", scripts)
	}

	nav.RequestOpenAccounts()
	if len(scripts) != 3 || scripts[2] != openAccountsJS {
		t.Fatalf("accounts navigation must resume after reload: %v", scripts)
	}
}

func TestWindowNavControllerWithoutWaitRunsImmediately(t *testing.T) {
	var scripts []string
	nav := newWindowNavController(
		false,
		"http://127.0.0.1:43001",
		"key",
		nil,
		func(js string) { scripts = append(scripts, js) },
	)
	nav.RequestOpenConversations()
	if len(scripts) != 1 || scripts[0] != openConversationsJS {
		t.Fatalf("controllers that do not wait for navigation must run immediately: %d", len(scripts))
	}
}

func TestWindowNavControllerMixedTargetsConcurrent(t *testing.T) {
	nav := newWindowNavController(
		true,
		"http://127.0.0.1:43001",
		"key",
		nil,
		func(string) {},
	)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(3)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				nav.RequestOpenAccounts()
				return
			}
			nav.RequestOpenConversations()
		}(i)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				nav.RequestOpenUpdates()
				return
			}
			nav.ApplyEndpoint(bridge.Endpoint{URL: "http://127.0.0.1:43002", Key: "key-b"})
		}(i)
		go func() { defer wg.Done(); nav.OnNavigationFinished() }()
	}
	wg.Wait()
}
