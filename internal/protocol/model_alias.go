package protocol

import (
	"regexp"
	"strings"
)

// uuidProviderPrefix matches the provider id the official client uses for a BYOK
// provider: "byok-" followed by a canonical UUID and a slash. The hex digits are
// matched case-insensitively because the client may render them either way.
var uuidProviderPrefix = regexp.MustCompile(`^byok-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/`)

// stripModelAliasPrefix removes the Kimi-side provider prefix from a model name.
//
// Aliases are the fixed legacy prefixes. A UUID provider id is the form the
// official client uses now, and it differs per provider, so it is recognized by
// shape instead of a fixed list. A "byok-" name that is neither a known alias nor
// a UUID provider id is left untouched: stripping arbitrary prefixes would make a
// model whose own id starts with "byok-" unroutable.
func stripModelAliasPrefix(modelName string) string {
	for _, prefix := range Aliases {
		if strings.HasPrefix(modelName, prefix) {
			return modelName[len(prefix):]
		}
	}
	if match := uuidProviderPrefix.FindString(modelName); match != "" {
		return modelName[len(match):]
	}
	return modelName
}
