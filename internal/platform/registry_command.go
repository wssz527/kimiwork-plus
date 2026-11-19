package platform

import "strings"

const hkcPrefix = `HKCU\`

// registryCommand is one parsed reg.exe invocation. It is a separate type from
// the command line so the accepted shapes can be checked on any host: this
// package runs on macOS too, where the Windows code never executes.
type registryCommand struct {
	verb      string
	subkey    string
	valueName string
	data      string
}

// registryTypeName renders a value type the way reg.exe names it in its query
// output. Only the string types this package reads are named; anything else is
// reported as empty so the caller can refuse it.
func registryTypeName(valueType uint32) string {
	switch valueType {
	case regSZ:
		return "REG_SZ"
	case regExpandSZ:
		return "REG_EXPAND_SZ"
	}
	return ""
}

const (
	regSZ       = 1
	regExpandSZ = 2
)

// parseWindowsRegistryCommand accepts the three shapes service.go builds and
// rejects everything else: this is not a general reg.exe front end.
func parseWindowsRegistryCommand(args []string) (registryCommand, bool) {
	if len(args) < 4 {
		return registryCommand{}, false
	}
	command := registryCommand{verb: strings.ToLower(args[0])}
	keyPath := args[1]
	if !strings.HasPrefix(strings.ToUpper(keyPath), hkcPrefix) {
		return registryCommand{}, false
	}
	command.subkey = keyPath[len(hkcPrefix):]
	var valueType string
	var hasData bool
	for i := 2; i < len(args); i++ {
		switch strings.ToLower(args[i]) {
		case "/v":
			if i+1 >= len(args) {
				return registryCommand{}, false
			}
			command.valueName = args[i+1]
			i++
		case "/t":
			if i+1 >= len(args) {
				return registryCommand{}, false
			}
			valueType = args[i+1]
			i++
		case "/d":
			if i+1 >= len(args) {
				return registryCommand{}, false
			}
			command.data = args[i+1]
			hasData = true
			i++
		case "/f":
		default:
			return registryCommand{}, false
		}
	}
	if command.subkey == "" || command.valueName == "" {
		return registryCommand{}, false
	}
	switch command.verb {
	case "query", "delete":
		return command, !hasData
	case "add":
		return command, hasData && strings.EqualFold(valueType, "REG_SZ")
	}
	return registryCommand{}, false
}
