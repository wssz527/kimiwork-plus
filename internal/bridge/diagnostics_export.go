package bridge

import (
	"runtime"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

// diagnosticsExport builds the downloaded report. The state document is never
// serialized as-is: every field below is picked by name, so a new sensitive
// field added to State stays out until it is deliberately added here too.
func (n *Native) diagnosticsExport() core.Document {
	return diagnosticsExportDocument(diagnosticsStateSummary(n.State(), n.diagnosticsConfig()), n.diagnostics.history())
}

func diagnosticsExportDocument(summary core.Document, events []diagnosticEvent) core.Document {
	cleaned := make([]diagnosticEvent, 0, len(events))
	for _, event := range events {
		cleaned = append(cleaned, sanitizeDiagnosticEvent(event))
	}
	return core.Document{
		"schemaVersion": diagnosticsSchemaVersion,
		"generatedAt":   time.Now().UnixMilli(),
		"app": core.Document{
			"version":  Version,
			"buildId":  BuildID,
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
		},
		"redaction": core.Document{
			"note": "本报告只包含脱敏后的操作诊断信息，用于排查模型同步等间歇性失败。",
			"excluded": []string{
				"token", "apiKey", "cookie", "account", "endpointUrl",
				"path", "sourceId", "aliases", "fullState",
			},
		},
		"summary": summary,
		"events":  cleaned,
	}
}

func (n *Native) diagnosticsConfig() core.Document {
	if n == nil || n.service == nil || n.service.Store == nil {
		return nil
	}
	d, _, err := n.service.Store.Read()
	if err != nil {
		return nil
	}
	return d
}

func diagnosticsStateSummary(state, config core.Document) core.Document {
	summary := core.Document{
		"version":         Version,
		"buildId":         BuildID,
		"platform":        runtime.GOOS + "/" + runtime.GOARCH,
		"integrationMode": "unknown",
		"modelCount":      0,
		"connectionCount": 0,
	}
	if config != nil {
		summary["integrationMode"] = core.IntegrationMode(config)
		summary["modelCount"] = len(core.Rows(config["models"]))
		summary["connectionCount"] = len(core.Rows(config["connections"]))
	}
	if process := core.Object(state["kimiProcess"]); process != nil {
		summary["process"] = diagnosticsProcess(process)
	}
	summary["profile"] = diagnosticsProfile(state["profile"])
	summary["integration"] = diagnosticsIntegration(core.Object(state["integration"]))
	summary["nativeImport"] = diagnosticsNativeImport(core.Object(state["integration"]))
	if report, ok := state["compatibility"].(kimi.CompatibilityReport); ok {
		summary["compatibility"] = diagnosticsCompatibility(report)
	}
	summary["transport"] = diagnosticsTransport(kimi.ManagedTransportInfo())
	if accounts := core.Object(state["accounts"]); accounts != nil {
		summary["accounts"] = core.Document{
			"state":  diagnosticText(core.String(accounts["state"]), 24),
			"reason": diagnosticText(core.String(accounts["reason"]), 100),
			"count":  diagnosticsRows(accounts["accounts"]),
		}
	}
	if operation := core.Object(state["operation"]); operation != nil {
		summary["currentOperation"] = diagnosticsOperation(operation)
	}
	return summary
}

// diagnosticsProcess keeps `running` a tri-state. Only a real bool is reported:
// anything else means "not known", which is what nil already means elsewhere.
func diagnosticsProcess(process map[string]any) core.Document {
	var running any
	if value, ok := process["running"].(bool); ok {
		running = value
	}
	return core.Document{
		"running": running,
		"state":   diagnosticText(core.String(process["state"]), 24),
		"reason":  diagnosticText(core.String(process["reason"]), 100),
	}
}

func diagnosticsProfile(value any) core.Document {
	switch profile := value.(type) {
	case kimi.ProfileStatus:
		return core.Document{
			"mode":          diagnosticText(profile.Mode, 24),
			"implemented":   profile.Implemented,
			"canReuseLogin": profile.CanReuseLogin,
			"reason":        diagnosticText(profile.Reason, 100),
		}
	case core.Document:
		return diagnosticsProfileDocument(profile)
	case map[string]any:
		return diagnosticsProfileDocument(profile)
	}
	return core.Document{}
}

func diagnosticsProfileDocument(profile map[string]any) core.Document {
	implemented, _ := profile["implemented"].(bool)
	canReuse, _ := profile["canReuseLogin"].(bool)
	return core.Document{
		"mode":          diagnosticText(core.String(profile["mode"]), 24),
		"implemented":   implemented,
		"canReuseLogin": canReuse,
		"reason":        diagnosticText(core.String(profile["reason"]), 100),
	}
}

func diagnosticsIntegration(integration map[string]any) core.Document {
	if integration == nil {
		return core.Document{}
	}
	out := core.Document{
		"state":               diagnosticText(core.String(integration["state"]), 24),
		"reason":              diagnosticText(core.String(integration["reason"]), 100),
		"imported":            integration["imported"] == true,
		"applied":             integration["applied"] == true,
		"adopted":             integration["adopted"] == true,
		"mode":                diagnosticText(core.String(integration["mode"]), 24),
		"routeCleanupPending": integration["routeCleanupPending"] == true,
	}
	if desktop := core.Object(integration["desktop"]); desktop != nil {
		out["desktop"] = core.Document{
			"state":   diagnosticText(core.String(desktop["state"]), 24),
			"reason":  diagnosticText(core.String(desktop["reason"]), 100),
			"managed": desktop["managed"] == true,
		}
	}
	return out
}

// diagnosticsNativeImport mirrors the sanitized receipt. The receipt is read
// back out of the published integration document, so only the safe keys below
// are copied even though the original result also carries a source id.
func diagnosticsNativeImport(integration map[string]any) core.Document {
	if integration == nil {
		return nil
	}
	return diagnosticImportView(diagnosticImportDocument(core.Object(integration["nativeImport"])))
}

// diagnosticImportView renders a sanitized import as an explicit document, so
// the published shape is a fixed list of keys rather than a struct tag set.
func diagnosticImportView(imported *diagnosticImport) core.Document {
	if imported == nil {
		return nil
	}
	out := core.Document{
		"ok":                  imported.OK,
		"state":               imported.State,
		"reason":              imported.Reason,
		"models":              imported.Models,
		"requiresRestart":     imported.RequiresRestart,
		"restartDeferred":     imported.RestartDeferred,
		"applied":             imported.Applied,
		"adopted":             imported.Adopted,
		"opened":              imported.Opened,
		"routeCleanupPending": imported.RouteCleanupPending,
	}
	if imported.Closed != nil {
		out["closed"] = *imported.Closed
	}
	return out
}

func diagnosticsOperation(operation map[string]any) core.Document {
	out := core.Document{
		"id":     diagnosticText(core.String(operation["id"]), 32),
		"action": diagnosticAction(core.String(operation["action"])),
		"state":  diagnosticText(core.String(operation["state"]), 40),
		"stage":  diagnosticText(core.String(operation["stage"]), 24),
		"reason": diagnosticText(core.String(operation["reason"]), 100),
	}
	if value, ok := operation["startedAt"]; ok {
		out["startedAt"] = documentInt64(value)
	}
	if value, ok := operation["finishedAt"]; ok {
		out["finishedAt"] = documentInt64(value)
	}
	if imported := diagnosticImportOf(operation["result"]); imported != nil {
		out["import"] = diagnosticImportView(imported)
	} else if imported := diagnosticImportDocument(core.Object(operation["result"])); imported != nil {
		out["import"] = diagnosticImportView(imported)
	}
	return out
}

func diagnosticsCompatibility(report kimi.CompatibilityReport) core.Document {
	features := []core.Document{}
	for _, feature := range report.AffectedFeatures {
		features = append(features, core.Document{
			"feature":      diagnosticText(feature.Feature, 64),
			"reason":       diagnosticCodeList(feature.Reason),
			"writeBlocked": feature.WriteBlocked,
		})
	}
	return core.Document{
		"status":           diagnosticText(report.Status, 24),
		"officialVersion":  diagnosticText(report.OfficialVersion, 64),
		"verifiedVersion":  report.VerifiedVersion,
		"kernelVersion":    diagnosticText(report.KernelVersion, 64),
		"kernelCommit":     diagnosticText(report.KernelCommit, 64),
		"archiveSha256":    diagnosticText(report.ArchiveSHA256, 64),
		"versionChanged":   report.VersionChanged,
		"previousVersion":  diagnosticText(report.PreviousVersion, 64),
		"checkedAt":        report.CheckedAt,
		"affectedFeatures": features,
		"guards": core.Document{
			"modelSyncWriteBlocked":     report.Guards.ModelSyncWriteBlocked,
			"accountSwitchWriteBlocked": report.Guards.AccountSwitchWriteBlocked,
		},
		"checks": core.Document{
			"catalog": core.Document{
				"status":        diagnosticText(report.Checks.Catalog.Status, 24),
				"contractState": diagnosticText(report.Checks.Catalog.ContractState, 24),
				"reason":        diagnosticText(report.Checks.Catalog.Reason, 100),
				"parser":        diagnosticText(report.Checks.Catalog.Parser, 32),
				"modelCount":    report.Checks.Catalog.ModelCount,
				"acceptedCount": report.Checks.Catalog.AcceptedCount,
			},
			"accountApi": core.Document{
				"status":          diagnosticText(report.Checks.AccountAPI.Status, 24),
				"source":          diagnosticText(report.Checks.AccountAPI.Source, 32),
				"getUserProfile":  report.Checks.AccountAPI.GetUserProfile,
				"getByokProfile":  report.Checks.AccountAPI.GetByokProfile,
				"byokSourcesList": report.Checks.AccountAPI.ByokSourcesList,
				"modelImportApi":  report.Checks.AccountAPI.ModelImportAPI,
				"reason":          diagnosticCodeList(report.Checks.AccountAPI.Reason),
			},
			"storeFormat": core.Document{
				"status": diagnosticText(report.Checks.StoreFormat.Status, 24),
				"format": diagnosticText(report.Checks.StoreFormat.Format, 32),
				"reason": diagnosticText(report.Checks.StoreFormat.Reason, 100),
			},
		},
	}
}

func diagnosticsTransport(transport kimi.ManagedTransport) core.Document {
	return core.Document{
		"kind":      diagnosticText(transport.Kind, 24),
		"supported": transport.Supported,
		"verified":  transport.Verified,
		"reason":    diagnosticText(transport.Reason, 100),
	}
}

// diagnosticImportDocument copies only the safe keys out of a receipt-shaped
// document, then runs the same sanitizer the log uses.
func diagnosticImportDocument(doc map[string]any) *diagnosticImport {
	if doc == nil {
		return nil
	}
	out := &diagnosticImport{
		OK:                  doc["ok"] == true,
		State:               core.String(doc["state"]),
		Reason:              core.String(doc["reason"]),
		Models:              int(documentInt64(doc["models"])),
		RequiresRestart:     doc["requiresRestart"] == true,
		RestartDeferred:     doc["restartDeferred"] == true,
		Applied:             doc["applied"] == true,
		Adopted:             doc["adopted"] == true,
		Opened:              doc["opened"] == true,
		RouteCleanupPending: doc["routeCleanupPending"] == true,
	}
	if closed, ok := doc["closed"].(bool); ok {
		out.Closed = &closed
	}
	return sanitizeDiagnosticImport(out)
}

func diagnosticsRows(value any) int {
	switch rows := value.(type) {
	case []any:
		return len(rows)
	case []AccountSummary:
		return len(rows)
	}
	return 0
}
