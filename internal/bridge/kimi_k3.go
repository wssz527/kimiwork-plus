package bridge

import (
	"strings"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

func kimiK3State(d core.Document, integration *kimi.IntegrationResult, running bool) string {
	if core.IntegrationMode(d) != core.IntegrationModeGateway {
		return ""
	}
	enabled := core.KimiK3Compatibility(d)
	count := 0
	needsSync, providerMismatch := false, false
	for _, raw := range core.Rows(d["models"]) {
		model := core.Object(raw)
		if model["protocol"] != protocol.ProtocolChat {
			continue
		}
		count++
		id := core.String(model["id"])
		if integration == nil || !integration.Applied || integration.Mode == core.IntegrationModeDirect {
			if enabled || (integration != nil && integration.State == kimi.IntegrationNeedsImport && integration.Reason == "models_missing") {
				needsSync = true
			}
			continue
		}
		alias := integration.Aliases[id]
		if !enabled {
			needsSync = needsSync || strings.HasSuffix(alias, "/"+protocol.KimiCompatibleModelID(id))
			continue
		}
		compatibleID := protocol.KimiCompatibleModelID(id)
		if !strings.HasSuffix(alias, "/"+compatibleID) {
			needsSync = true
		} else if alias != "byok-kth-kimi/"+compatibleID {
			providerMismatch = true
		}
	}
	if !enabled && d[core.KimiK3CompatibilityKey] == nil {
		return ""
	}
	switch {
	case count == 0 && enabled:
		return "no_chat_models"
	case needsSync:
		return "needs_sync"
	case providerMismatch:
		return "provider_mismatch"
	case enabled && running && !integration.Adopted:
		return "requires_restart"
	case enabled:
		return "ready"
	default:
		return ""
	}
}
