package byokdns

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func ResolverConfiguration(port int, resolverPath string) (string, error) {
	if port < 1024 || port > 65535 {
		return "", ErrInvalidPort
	}
	return "# Magpie Kimi Work native BYOK\nnameserver 127.0.0.1\nport " + strconv.Itoa(port) + "\ntimeout 1\n", nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func ResolverScript(action, source, target string, flush bool) string {
	if action != "prepare" && action != "remove-dns" {
		return ""
	}
	destination := shellQuote(target)
	input := shellQuote(source)
	directory := shellQuote(filepath.Dir(target))
	script := "set -eu\ntest ! -L " + directory + "\ntest ! -L " + destination + "\n"
	if action == "prepare" {
		script += "if test -e " + destination + "; then /usr/bin/cmp -s " + input + " " + destination + " || { /usr/bin/printf %s resolver_conflict >&2; exit 73; }; else /bin/mkdir -p " + directory + "; /usr/bin/install -m 644 " + input + " " + destination + "; fi\n"
	} else {
		script += "if test -e " + destination + "; then /usr/bin/cmp -s " + input + " " + destination + " || { /usr/bin/printf %s resolver_conflict >&2; exit 73; }; /bin/rm " + destination + "; fi\n"
	}
	if flush {
		script += "/usr/bin/dscacheutil -flushcache\n/usr/bin/killall -HUP mDNSResponder\n"
	}
	return script
}

// DarwinHostsScript is the elevated step that makes dev.msh.team resolve without
// the gateway daemon, while cleaning up the scoped resolver older releases
// installed.
//
// It owns both files because they interact: an /etc/resolver entry for the same
// domain takes the lookup to port 53535, and a stopped daemon there would fail
// even though the static hosts entry is correct. Both are therefore written in
// one administrator prompt.
//
// hostsAfterFile is the exact content computed before elevation. The script
// refuses to replace a hosts file that no longer matches hostsBeforeFile (exit 73
// resolver_conflict), so an edit made in between is never overwritten. The
// replacement itself is a temp file plus mv, so /etc/hosts is never half-written.
//
// A resolver that is not byte-for-byte ours is never removed or shadowed; the
// script reports resolver_conflict instead. The check runs before hosts is
// touched, so a foreign corporate resolver leaves the machine as it was.
func DarwinHostsScript(action, hostsBeforeFile, hostsAfterFile, hostsTarget, resolverSource, resolverTarget string, flush bool) string {
	if action != "prepare" && action != "remove-dns" {
		return ""
	}
	before := shellQuote(hostsBeforeFile)
	after := shellQuote(hostsAfterFile)
	hosts := shellQuote(hostsTarget)
	temporary := shellQuote(hostsTarget + ".kwp.tmp")
	resolver := shellQuote(resolverTarget)
	resolverInput := shellQuote(resolverSource)
	resolverDirectory := shellQuote(filepath.Dir(resolverTarget))
	var script strings.Builder
	script.WriteString("set -eu\n")
	script.WriteString("if test -L " + hosts + "; then /usr/bin/printf %s resolver_conflict >&2; exit 73; fi\n")
	script.WriteString("if test -L " + resolverDirectory + " || test -L " + resolver + "; then /usr/bin/printf %s resolver_conflict >&2; exit 73; fi\n")
	script.WriteString("resolver_owned=0\n")
	script.WriteString("if test -e " + resolver + "; then\n")
	script.WriteString("  if /usr/bin/cmp -s " + resolverInput + " " + resolver + "; then resolver_owned=1; else /usr/bin/printf %s resolver_conflict >&2; exit 73; fi\n")
	script.WriteString("fi\n")
	script.WriteString("if /usr/bin/cmp -s " + before + " " + hosts + "; then :; else /usr/bin/printf %s resolver_conflict >&2; exit 73; fi\n")
	script.WriteString("if ! /usr/bin/cmp -s " + before + " " + after + "; then\n")
	script.WriteString("  /bin/rm -f " + temporary + "\n")
	script.WriteString("  /usr/bin/install -m 644 " + after + " " + temporary + "\n")
	script.WriteString("  /bin/mv -f " + temporary + " " + hosts + "\n")
	script.WriteString("fi\n")
	script.WriteString("if test \"$resolver_owned\" = 1; then /bin/rm -f " + resolver + "; fi\n")
	if flush {
		script.WriteString("/usr/bin/dscacheutil -flushcache\n/usr/bin/killall -HUP mDNSResponder\n")
	}
	return script.String()
}

func darwinAdministratorCommand(script string) string {
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(script)
	return "do shell script " + strings.TrimSuffix(encoded.String(), "\n") + " with administrator privileges"
}

func darwinAdministrator(ctx context.Context, script, _ string) error {
	command := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", darwinAdministratorCommand(script))
	output, err := command.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(output)) + " " + err.Error())
	}
	return nil
}
