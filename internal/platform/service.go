package platform

import (
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
	// Kind is "launchd" or "schtasks".
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

// ServiceLabel is the preview identity. It is deliberately not the released
// build's label: the two services must be able to coexist.
const ServiceLabel = "dev.kimi-work-plus.preview.daemon"

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
		return describeScheduledTask(options, loginStart), nil
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

// describeScheduledTask renders the Windows task commands.
//
// schtasks has no "create disabled" form, so the task is created ONLOGON and then
// disabled in the same plan. The rendering shows that pair, which is what makes
// "registered but not enabled" true on Windows as well.
func describeScheduledTask(options ServiceOptions, loginStart bool) ServiceDescription {
	commandLine := taskCommandLine(options)
	create := []string{"schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED",
		"/TN", ServiceLabel, "/TR", commandLine}
	install := [][]string{create}
	if !loginStart {
		install = append(install, []string{"schtasks", "/Change", "/TN", ServiceLabel, "/DISABLE"})
	}
	notes := []string{
		"ONLOGON with /RL LIMITED runs the daemon as the signed-in user without elevation.",
		"Deleting the task removes only this preview's entry.",
	}
	if loginStart {
		notes = append(notes, "The task is active because you enabled login start.")
	} else {
		notes = append(notes, "The task is created disabled: nothing starts at login until you enable it.")
	}
	return ServiceDescription{
		Kind: "schtasks", Label: ServiceLabel, DefinitionPath: "",
		Install: install,
		Uninstall: [][]string{
			{"schtasks", "/End", "/TN", ServiceLabel},
			{"schtasks", "/Delete", "/F", "/TN", ServiceLabel},
		},
		Status: [][]string{
			{"schtasks", "/Query", "/TN", ServiceLabel, "/XML"},
		},
		Notes:      notes,
		LoginStart: loginStart,
	}
}

func boolTag(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// quoteWindows quotes one argument for the Windows task scheduler. The value is
// wrapped in double quotes and internal quotes are doubled, which is what
// schtasks stores and what CreateProcess parses back.
func quoteWindows(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}
