package platform

import "testing"

// service.go builds exactly these three shapes. Anything else must be refused
// here rather than passed to an external program or half-applied.
func TestWindowsRegistryCommandAcceptsOnlyThePlannedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want registryCommand
	}{
		{"add with data",
			[]string{"add", windowsRunKey, "/v", ServiceLabel, "/t", "REG_SZ", "/d", `"C:\daemon.exe" --start-background`, "/f"},
			registryCommand{verb: "add", subkey: `Software\Microsoft\Windows\CurrentVersion\Run`,
				valueName: ServiceLabel, data: `"C:\daemon.exe" --start-background`}},
		{"delete",
			[]string{"delete", windowsRunKey, "/v", ServiceLabel, "/f"},
			registryCommand{verb: "delete", subkey: `Software\Microsoft\Windows\CurrentVersion\Run`, valueName: ServiceLabel}},
		{"query",
			[]string{"query", windowsRunKey, "/v", ServiceLabel},
			registryCommand{verb: "query", subkey: `Software\Microsoft\Windows\CurrentVersion\Run`, valueName: ServiceLabel}},
	} {
		got, ok := parseWindowsRegistryCommand(tc.args)
		if !ok {
			t.Fatalf("%s: refused %v", tc.name, tc.args)
		}
		if got != tc.want {
			t.Fatalf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"machine hive", []string{"add", `HKLM\Software\Run`, "/v", ServiceLabel, "/t", "REG_SZ", "/d", "x", "/f"}},
		{"hive prefix inside a key name", []string{"add", `Software\HKCU\Run`, "/v", ServiceLabel, "/t", "REG_SZ", "/d", "x", "/f"}},
		{"empty subkey", []string{"add", `HKCU\`, "/v", ServiceLabel, "/t", "REG_SZ", "/d", "x", "/f"}},
		{"non-string type", []string{"add", windowsRunKey, "/v", ServiceLabel, "/t", "REG_DWORD", "/d", "1", "/f"}},
		{"add without data", []string{"add", windowsRunKey, "/v", ServiceLabel, "/t", "REG_SZ", "/f"}},
		{"query carrying data", []string{"query", windowsRunKey, "/v", ServiceLabel, "/d", "x"}},
		{"missing value name", []string{"query", windowsRunKey}},
		{"unknown verb", []string{"import", windowsRunKey, "/v", ServiceLabel, "/f"}},
		{"unknown switch", []string{"add", windowsRunKey, "/v", ServiceLabel, "/t", "REG_SZ", "/d", "x", "/s"}},
	} {
		if got, ok := parseWindowsRegistryCommand(tc.args); ok {
			t.Fatalf("%s: accepted %v as %+v", tc.name, tc.args, got)
		}
	}
}

// service.go must keep producing shapes this package implements. A change there
// that this parser refuses would silently reopen the console it exists to avoid.
func TestPlannedRegistryStepsParse(t *testing.T) {
	options := ServiceOptions{Binary: `C:\Program Files\Kimi Work+ Preview\gateway-daemon.exe`, Home: `C:\Users\Alice`}
	for _, action := range []string{actionRegister, actionEnable, actionDisable, actionStatus, actionUninstall} {
		steps, err := windowsPlan(action, options, `C:\Users\Alice\service.json`, "")
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		for _, item := range steps {
			if item.Command == nil || item.Command.Name != registryProgram {
				continue
			}
			if _, ok := parseWindowsRegistryCommand(item.Command.Args); !ok {
				t.Fatalf("%s plans a registry step this package does not implement: %v", action, item.Command.Args)
			}
		}
	}
}

// A value written with another string type must keep answering the type it was
// stored as: reg.exe printed REG_EXPAND_SZ, and the Run-value parser treats that
// as "not enabled".
func TestRegistryQueryTypeNames(t *testing.T) {
	for _, tc := range []struct {
		valueType uint32
		want      string
	}{
		{1, "REG_SZ"},
		{2, "REG_EXPAND_SZ"},
		{4, ""},
		{3, ""},
	} {
		if got := registryTypeName(tc.valueType); got != tc.want {
			t.Fatalf("registryTypeName(%d) = %q, want %q", tc.valueType, got, tc.want)
		}
	}
	output := "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Run\r\n    " +
		ServiceLabel + "    REG_EXPAND_SZ    " + `"%LOCALAPPDATA%\daemon.exe"` + "\r\n"
	if parseRegistryRunValue(output) {
		t.Fatal("an expandable string was read as the enabled Run value")
	}
}
