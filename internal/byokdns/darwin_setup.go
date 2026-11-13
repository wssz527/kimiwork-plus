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
