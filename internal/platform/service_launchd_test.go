package platform

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A rendering must honor ServiceOptions.Home on both platforms. macOS used to
// ignore it, so a test could inspect the Windows path but the launchd definition
// was always rendered against the real home.
func TestDescribeServiceHonorsTheHomeOverride(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	options := ServiceOptions{
		Binary:   filepath.Join(home, "bin", "gateway-daemon"),
		StateDir: state,
		Port:     "43117",
		BuildID:  "build-a",
		Home:     home,
	}
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			description, err := describeServiceFor(goos, options, false)
			if err != nil {
				t.Fatal(err)
			}
			want := definitionPathFor(goos, home)
			if description.DefinitionPath != want {
				t.Fatalf("definition path = %q, want %q", description.DefinitionPath, want)
			}
			if strings.Contains(description.DefinitionPath, homeDirectory()) && homeDirectory() != home {
				t.Fatalf("the rendering escaped the home override: %q", description.DefinitionPath)
			}
		})
	}
}

// The default log must never land in ~/Library/LaunchAgents: that directory holds
// definitions, and a log there is not read by launchd and not removed with the
// plist.
func TestLaunchdDefaultLogPathsAreOutsideLaunchAgents(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	launchAgents := filepath.Join(home, "Library", "LaunchAgents")
	for _, test := range []struct {
		name     string
		stateDir string
		want     string
	}{
		{name: "state dir", stateDir: state, want: filepath.Join(state, "launchd-daemon.log")},
		{name: "home logs", stateDir: "",
			want: filepath.Join(home, "Library", "Logs", "KimiWorkPlus", ServiceLabel+".log")},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := ServiceOptions{
				Binary:   filepath.Join(home, "bin", "gateway-daemon"),
				StateDir: test.stateDir,
				Home:     home,
			}
			description, err := describeServiceFor("darwin", options, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
				path := plistString(description.Content, key)
				if path != test.want {
					t.Fatalf("%s = %q, want %q", key, path, test.want)
				}
				if strings.HasPrefix(path, launchAgents+string(filepath.Separator)) {
					t.Fatalf("%s was written inside LaunchAgents: %q", key, path)
				}
			}
		})
	}
}

// An explicit LogPath still wins, so a caller that manages the file itself is not
// overridden by the default.
func TestLaunchdExplicitLogPathWins(t *testing.T) {
	home := t.TempDir()
	explicit := filepath.Join(home, "logs", "custom.log")
	options := ServiceOptions{
		Binary:   filepath.Join(home, "bin", "gateway-daemon"),
		StateDir: filepath.Join(home, "state"),
		LogPath:  explicit,
		Home:     home,
	}
	description, err := describeServiceFor("darwin", options, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if path := plistString(description.Content, key); path != explicit {
			t.Fatalf("%s = %q, want %q", key, path, explicit)
		}
	}
}

// Older releases wrote the daemon log into LaunchAgents. Uninstall must take
// those stale files with it, including the preview-era label.
func TestUninstallRemovesLegacyLaunchAgentLogs(t *testing.T) {
	host := newFakeHost()
	manager, options := managerFor(t, "darwin", host)
	definition := definitionPathFor("darwin", options.Home)
	host.files[definition] = []byte("plist")
	for _, path := range legacyLaunchAgentLogPaths(options.Home) {
		host.files[path] = []byte("stale log\n")
	}
	if _, err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range legacyLaunchAgentLogPaths(options.Home) {
		if _, still := host.files[path]; still {
			t.Fatalf("legacy log %s survived Uninstall", path)
		}
	}
	if _, still := host.files[definition]; still {
		t.Fatal("the plist survived Uninstall")
	}
}

// Migrating a preview-era service removes the preview log that release wrote
// beside its plist, so the old label leaves no files behind.
func TestMigrateLegacyPreviewServiceRemovesThePreviewLog(t *testing.T) {
	host := newFakeHost()
	_, options := managerFor(t, "darwin", host)
	legacyDef := legacyDefinitionPathFor("darwin", options.Home)
	host.files[legacyDef] = []byte("<plist><key>Label</key><string>" + LegacyPreviewServiceLabel +
		"</string><key>RunAtLoad</key><true/></plist>")
	previewLog := filepath.Join(options.Home, "Library", "LaunchAgents", LegacyPreviewServiceLabel+".log")
	host.files[previewLog] = []byte("preview log\n")
	migrated, enabled, err := MigrateLegacyPreviewService(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !migrated || !enabled {
		t.Fatalf("migrated=%v enabled=%v", migrated, enabled)
	}
	if _, still := host.files[previewLog]; still {
		t.Fatalf("legacy preview log %s survived migration", previewLog)
	}
	if _, still := host.files[legacyDef]; still {
		t.Fatal("legacy preview definition survived migration")
	}
	if _, exists := host.files[definitionPathFor("darwin", options.Home)]; !exists {
		t.Fatal("the formal definition was not written")
	}
}

// legacyLaunchAgentLogPaths is the only list of files release cleanup may touch:
// both the formal and the preview-era label, and nothing else.
func TestLegacyLaunchAgentLogPathsAreTheTwoKnownLabels(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "someone")
	paths := legacyLaunchAgentLogPaths(home)
	if len(paths) != 2 {
		t.Fatalf("paths = %#v", paths)
	}
	want := map[string]bool{
		filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".log"):              true,
		filepath.Join(home, "Library", "LaunchAgents", LegacyPreviewServiceLabel+".log"): true,
	}
	for _, path := range paths {
		if !want[path] {
			t.Fatalf("unexpected cleanup target: %s", path)
		}
	}
}
