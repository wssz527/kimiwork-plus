package platform

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ServiceDescription is a user-level background service definition, rendered as
// text and as the exact commands, so a panel can show what would change.
//
// DescribeService is pure: it writes no file and runs nothing. Manager is what
// actually registers and operates the service, and every Manager action is an
// explicit user choice. Nothing here auto-enables a login item.
type ServiceDescription struct {
	// Kind is "launchd" on macOS and "registry" on Windows.
	Kind string `json:"kind"`
	// Label is the reverse-DNS service identifier.
	Label string `json:"label"`
	// DefinitionPath is where the definition belongs.
	DefinitionPath string `json:"definitionPath"`
	// Content is the definition file body. Empty when the platform uses a
	// command-line task instead of a file.
	Content string `json:"content,omitempty"`
	// Install, Uninstall and Status are the commands a user runs, in order.
	Install   [][]string `json:"install"`
	Uninstall [][]string `json:"uninstall"`
	Status    [][]string `json:"status"`
	// Notes are shown next to the commands, so the user knows what they change.
	Notes []string `json:"notes,omitempty"`
	// LoginStart reports whether this rendering starts the service at login.
	LoginStart bool `json:"loginStart"`
}

// ServiceLabel is the formal background daemon service identifier.
const ServiceLabel = "dev.kimi-work-plus.daemon"

// LegacyPreviewServiceLabel is the service identifier used by preview builds prior to 1.0.0.
const LegacyPreviewServiceLabel = "dev.kimi-work-plus.preview.daemon"

// ServiceOptions describes the binary the service should run.
type ServiceOptions struct {
	// Binary is the absolute path of the daemon executable.
	Binary string
	// StateDir is passed as --state-dir.
	StateDir string
	// LogPath receives stdout and stderr.
	LogPath string
	// Port is passed as --port when set. An empty value keeps the daemon's own
	// default, which is "0" and means a free port each start.
	Port string
	// BuildID is recorded in the definition so a supervisor can tell which build
	// registered it. It is never passed to the daemon: the daemon reports its own
	// identity, and a definition cannot be trusted to describe it.
	BuildID string
	// Home overrides the home directory a definition path is rendered against. It
	// exists so a rendering can be inspected without touching the real one.
	Home string
}

// DescribeService renders the platform's user-level service definition with
// login start off. It is pure: no file is written and no command is run.
func DescribeService(options ServiceOptions) (ServiceDescription, error) {
	if !filepath.IsAbs(options.Binary) {
		return ServiceDescription{}, fmt.Errorf("service_binary_not_absolute")
	}
	if options.StateDir != "" && !filepath.IsAbs(options.StateDir) {
		return ServiceDescription{}, fmt.Errorf("service_state_dir_not_absolute")
	}
	return describeServiceFor(runtime.GOOS, options, false)
}

// describeServiceFor renders one platform's definition.
func describeServiceFor(goos string, options ServiceOptions, loginStart bool) (ServiceDescription, error) {
	switch goos {
	case "darwin":
		return describeLaunchd(options, loginStart), nil
	case "windows":
		return describeWindowsRegistry(options, loginStart), nil
	}
	return ServiceDescription{}, ErrUnsupported
}

func serviceArguments(options ServiceOptions) []string {
	args := []string{}
	if options.StateDir != "" {
		args = append(args, "--state-dir", options.StateDir)
	}
	if options.Port != "" {
		args = append(args, "--port", options.Port)
	}
	return args
}

// describeLaunchd renders a LaunchAgent.
//
// RunAtLoad is the login-start switch. KeepAlive stays false in both renderings:
// a crashed daemon must not be restarted in a loop by launchd, because the panel
// is what decides to start it again, and a restart loop would hide the failure
// instead of reporting it.
func describeLaunchd(options ServiceOptions, loginStart bool) ServiceDescription {
	definition := filepath.Join(homeDirectory(), "Library", "LaunchAgents", ServiceLabel+".plist")
	logPath := options.LogPath
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(definition), ServiceLabel+".log")
	}
	var program strings.Builder
	program.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	program.WriteString("      <string>" + xmlEscape(options.Binary) + "</string>\n")
	for _, argument := range serviceArguments(options) {
		program.WriteString("      <string>" + xmlEscape(argument) + "</string>\n")
	}
	program.WriteString("    </array>\n")
	build := ""
	if options.BuildID != "" {
		// The build identity is a comment-level record, not a daemon argument:
		// it tells a supervisor which build wrote the definition.
		build = "    <key>BuildID</key>\n    <string>" + xmlEscape(options.BuildID) + "</string>\n"
	}

	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + ServiceLabel + `</string>
` + build + program.String() + `    <key>RunAtLoad</key>
    <` + boolTag(loginStart) + `/>
    <key>KeepAlive</key>
    <false/>
    <key>ProcessType</key>
    <string>Interactive</string>
    <key>StandardOutPath</key>
    <string>` + xmlEscape(logPath) + `</string>
    <key>StandardErrorPath</key>
    <string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`
	target := "gui/" + currentUID() + "/" + ServiceLabel
	notes := []string{
		"KeepAlive is false: a daemon that exits is reported, not restarted in a loop.",
		"bootout stops the service; it never touches the official Kimi application.",
	}
	if loginStart {
		notes = append(notes, "RunAtLoad is true because you enabled login start.")
	} else {
		notes = append(notes, "RunAtLoad is false: nothing starts until you ask it to.")
	}
	return ServiceDescription{
		Kind: "launchd", Label: ServiceLabel, DefinitionPath: definition, Content: content,
		LoginStart: loginStart,
		Install: [][]string{
			{"cp", "<rendered-plist>", definition},
			{"/bin/launchctl", "bootstrap", "gui/" + currentUID(), definition},
		},
		Uninstall: [][]string{
			{"/bin/launchctl", "bootout", target},
			{"rm", "-f", definition},
		},
		Status: [][]string{
			{"/bin/launchctl", "print", target},
		},
		Notes: notes,
	}
}

// describeWindowsRegistry renders the user-level Windows service.
//
// A per-user Run key needs no elevation and no Task Scheduler access: a medium
// integrity desktop process cannot create an ONLOGON task with schtasks at all, it
// is refused with ERROR_ACCESS_DENIED. The Run key is the mechanism Windows itself
// documents for a program the signed-in user starts at login, and it is what the
// Explorer shell launches.
//
// The definition file is written next to the preview's own configuration. It
// records what the registration was for, and it is what Status reads the build from
// without trusting the daemon's own report.
func describeWindowsRegistry(options ServiceOptions, loginStart bool) ServiceDescription {
	definition := definitionPathFor("windows", windowsHome(options))
	commandLine := windowsCommandLine(options)
	install := [][]string{
		{"<write-definition>", definition},
		append([]string{registryProgram}, registryDeleteArgs...),
	}
	notes := []string{
		"A per-user Run key needs no elevation: a medium integrity process cannot create an ONLOGON task at all.",
		"Deleting the value removes only this preview's entry; nothing else in the Run key is touched.",
	}
	if loginStart {
		install = [][]string{
			{"<write-definition>", definition},
			append(append([]string{registryProgram}, registryAddArgs...), "/d", commandLine, "/f"),
		}
		notes = append(notes, "The Run value exists because you enabled login start.")
	} else {
		notes = append(notes, "The Run value is absent after this change: nothing starts at login until you enable it.")
	}
	return ServiceDescription{
		Kind: "registry", Label: ServiceLabel, DefinitionPath: definition,
		Content: windowsDefinitionContent(options, loginStart),
		Install: install,
		Uninstall: [][]string{
			windowsTerminateCommand(options),
			append([]string{registryProgram}, registryDeleteArgs...),
			{"<remove-definition>", definition},
		},
		Status: [][]string{
			append([]string{registryProgram}, registryQueryArgs...),
		},
		Notes:      notes,
		LoginStart: loginStart,
	}
}

// windowsHome is the home directory a definition path is rendered against.
func windowsHome(options ServiceOptions) string {
	if options.Home != "" {
		return options.Home
	}
	return homeDirectory()
}

// windowsDefinitionContent is the JSON record written next to the preview's own
// configuration. It is deliberately not the login-start mechanism: the Run value
// is, and this file only says what was registered and by which build.
func windowsDefinitionContent(options ServiceOptions, loginStart bool) string {
	record := definitionRecord{BuildID: options.BuildID, Binary: options.Binary,
		StateDir: options.StateDir, Port: options.Port, LogPath: options.LogPath,
		CommandLine: windowsCommandLine(options), LoginStart: loginStart, Label: ServiceLabel}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded) + "\n"
}

// windowsTerminateCommand is the forced stop shown to a user. The normal stop is
// the daemon's own authenticated drain; this exists for a drain that failed and
// for an uninstall, and it never names the official application.
func windowsTerminateCommand(options ServiceOptions) []string {
	args := []string{"--terminate-daemon"}
	if options.StateDir != "" {
		args = append(args, "--state-dir", options.StateDir)
	}
	return append([]string{options.Binary}, args...)
}

// windowsCommandLine is what the Run value holds: the daemon started in the
// background, quoted the way CommandLineToArgvW parses it. Explorer passes the
// value to CreateProcess unchanged, so an unquoted path with a space — the preview
// is "Kimi Work+ Preview.app" on macOS and a folder with the same name on Windows —
// would be split into the wrong executable.
func windowsCommandLine(options ServiceOptions) string {
	arguments := append([]string{"--start-background"}, serviceArguments(options)...)
	if options.LogPath != "" {
		arguments = append(arguments, "--log-path", options.LogPath)
	}
	commandLine := quoteWindows(options.Binary)
	for _, argument := range arguments {
		commandLine += " " + quoteWindows(argument)
	}
	return commandLine
}

// windowsRunKey is the per-user start-up key. HKCU needs no elevation: the value
// belongs to the signed-in user, and the shell that reads it runs as that user.
const windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// registryProgram is the command-line program for every registry step. The
// argument lists below exclude it, so a plan step names it exactly once.
const registryProgram = "reg"

var (
	registryAddArgs          = []string{"add", windowsRunKey, "/v", ServiceLabel, "/t", "REG_SZ"}
	registryDeleteArgs       = []string{"delete", windowsRunKey, "/v", ServiceLabel, "/f"}
	registryQueryArgs        = []string{"query", windowsRunKey, "/v", ServiceLabel}
	legacyRegistryDeleteArgs = []string{"delete", windowsRunKey, "/v", LegacyPreviewServiceLabel, "/f"}
	legacyRegistryQueryArgs  = []string{"query", windowsRunKey, "/v", LegacyPreviewServiceLabel}
)

func boolTag(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// quoteWindows quotes one argument for a Windows command line, following the
// CommandLineToArgvW rules both Explorer and CreateProcess apply:
//
//   - the value is wrapped in double quotes;
//   - a run of backslashes followed by a quote is doubled and the quote escaped;
//   - a run of backslashes immediately before the closing quote is doubled.
//
// The preview's own path contains a space and a "+", which is exactly the case an
// unquoted value breaks.
func quoteWindows(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	backslashes := 0
	for _, character := range value {
		switch character {
		case '\\':
			backslashes++
		case '"':
			// Backslashes before a quote are escaped, and the quote itself is
			// escaped as \" on its way to the parser.
			out.WriteString(strings.Repeat(`\`, backslashes*2+1))
			out.WriteByte('"')
			backslashes = 0
		default:
			out.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
			out.WriteRune(character)
		}
	}
	// Backslashes at the end would otherwise escape the closing quote.
	out.WriteString(strings.Repeat(`\`, backslashes*2))
	out.WriteByte('"')
	return out.String()
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}
