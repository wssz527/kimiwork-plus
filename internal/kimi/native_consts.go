package kimi

// Marker lines and receipt names for the native TOML writers. They are protocol
// constants: an installed block is recognised by these exact bytes, so changing
// one would strand every earlier installation behind a conflict.
const (
	subagentBlockBegin = "# BEGIN KIMI WORK GATEWAY SUBAGENTS"
	subagentBlockEnd   = "# END KIMI WORK GATEWAY SUBAGENTS"
	subagentReceipt    = "native-subagent-config-receipt.json"

	contextBlockBegin = "# BEGIN KIMI WORK GATEWAY CONTEXT POLICY"
	contextBlockEnd   = "# END KIMI WORK GATEWAY CONTEXT POLICY"
	contextReceipt    = "native-context-policy-receipt.json"

	servicesReceipt = "native-services-config-receipt.json"
	servicesIntent  = "native-search-services-intent.json"

	markerWord = "KIMI WORK GATEWAY SUBAGENTS"
)
