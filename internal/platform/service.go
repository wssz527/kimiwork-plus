package platform

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// ServiceDescription is a user-level background service definition, rendered as
// text the user installs themselves.
//
// This package never registers, loads, starts or enables anything. It produces
// the definition and the exact commands, so the panel can show them and the user
// decides. Auto-enabling a login item on first run is the kind of change this
// product must not make on its own.
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
}

// DescribeService renders the platform's user-level service definition. It is
// pure: no file is written and no command is run.
func DescribeService(options ServiceOptions) (ServiceDescription, error) {
	if !filepath.IsAbs(options.Binary) {
		return ServiceDescription{}, fmt.Errorf("service_binary_not_absolute")
	}
	if options.StateDir != "" && !filepath.IsAbs(options.StateDir) {
		return ServiceDescription{}, fmt.Errorf("service_state_dir_not_absolute")
	}
	switch runtime.GOOS {
	case "darwin":
		return describeLaunchd(options), nil
	case "windows":
		return describeScheduledTask(options), nil
	}
	return ServiceDescription{}, ErrUnsupported
}

func serviceArguments(options ServiceOptions) []string {
	args := []string{}
	if options.StateDir != "" {
		args = append(args, "--state-dir", options.StateDir)
	}
	return args
}

func describeLaunchd(options ServiceOptions) ServiceDescription {
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

	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + ServiceLabel + `</string>
` + program.String() + `    <key>RunAtLoad</key>
    <false/>
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
	return ServiceDescription{
		Kind: "launchd", Label: ServiceLabel, DefinitionPath: definition, Content: content,
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
		Notes: []string{
			"RunAtLoad and KeepAlive are false: nothing starts until you ask it to.",
			"bootout stops the service; it never touches the official Kimi application.",
			"The definition is written by you, not by the application.",
		},
	}
}

func describeScheduledTask(options ServiceOptions) ServiceDescription {
	commandLine := quoteWindows(options.Binary)
	for _, argument := range serviceArguments(options) {
		commandLine += " " + quoteWindows(argument)
	}
	return ServiceDescription{
		Kind: "schtasks", Label: ServiceLabel,
		DefinitionPath: "",
		Install: [][]string{
			{"schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", ServiceLabel, "/TR", commandLine},
		},
		Uninstall: [][]string{
			{"schtasks", "/End", "/TN", ServiceLabel},
			{"schtasks", "/Delete", "/F", "/TN", ServiceLabel},
		},
		Status: [][]string{
			{"schtasks", "/Query", "/TN", ServiceLabel, "/XML"},
		},
		Notes: []string{
			"ONLOGON with /RL LIMITED runs the daemon as the signed-in user without elevation.",
			"Creating the task is an explicit step; nothing is registered at install time.",
			"Deleting the task removes only this preview's entry.",
		},
	}
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
