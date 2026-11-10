# internal/kimi, internal/platform, assets/injection — interface and evidence

Owner: worker B. Scope: `internal/kimi/`, `internal/platform/`, `assets/injection/`
and their tests. `go.mod`/`go.sum`, `cmd/`, `internal/core`, `internal/bridge`,
`frontend/`, `build/` belong to the main session.

## 1. Wiring the contract worker

`RunContractWorker` is the child entry point. The daemon must check the flag
before any other work, because the worker is the same binary:

```go
if len(os.Args) > 1 && os.Args[1] == kimi.ContractWorkerFlag {
    os.Exit(kimi.RunContractWorker(os.Stdin, os.Stdout))
}
```

`kimi.WorkerMain()` is the same thing with `os.Stdin`/`os.Stdout` bound, for a
one-line call. The parent never needs the flag: `RunJob` resolves
`os.Executable()` itself.

The worker reads one JSON job on stdin and writes one JSON outcome on stdout.
It receives no file, network or process API, and its environment is stripped to
`TMPDIR`/`TEMP`/`TMP`/`SystemRoot`.

## 2. Contract probing

```go
result := kimi.Inspect(ctx, kimi.ProbeOptions{})            // sample catalog
result := kimi.Inspect(ctx, kimi.ProbeOptions{Models: saved}) // configured catalog
result.Supported()                                          // only true for "verified"
```

`ContractResult.State` is one of `verified`, `incompatible`, `unverified`,
`unavailable`. Only `verified` may be shown as compatible. `Summarize(result)`
renders the panel line.

An explicit `Archive` is probed even when missing, so the caller learns
`unavailable` instead of silently falling back to another installation. An empty
configured catalog answers `unverified/models_empty`: it is not a probe.

```go
launch := kimi.InspectLaunchContract(ctx, kimi.ProbeOptions{})
// supported | unsupported | unverified | unavailable
```

Only `supported` means `DAIMON_BUNDLE_DIR` can be exported to a child.

## 3. Install and native state

```go
installs := kimi.Detect(runtime.GOOS, env)          // []Installation, read-only
state := kimi.DetectState(ctx, kimi.ProbeOptions{}) // NativeState for the panel
```

`NativeState.Application` is `running`, `not_running` or `unknown`. **`unknown`
is never reported as `not_running`**; the panel must render it as "could not
determine". `DetectState` never starts, stops, signals or focuses the official
application, and its contract probe only reads `app.asar`.

```go
identity := kimi.ProcessStartIdentity(ctx, pid)     // darwin ps lstart / windows FILETIME
ok := kimi.SameProcess(ctx, pid, identity)          // false for unknown
```

`ProcessIdentity.Kind == "unknown"` never compares equal, so a failed check
cannot authorise acting on a process.

## 4. Managed CDP session

```go
child, err := kimi.StartKimi(ctx, kimi.LaunchOptions{
    Installation: install, UserDataDir: isolatedProfile, Env: os.Environ(),
    ProtectedEnvNames: []string{"OPENAI_API_KEY"},
})
pipe := child.Pipe()            // NUL-terminated JSON over fd3/fd4
host := kimi.NewHost(pipe, kimi.InjectionOptions{
    Payload: string(rendererBytes), Revision: rev, Settings: publicSettings,
    OnReply: func(ctx context.Context, s *kimi.SurfaceSession, gen int, id string, v any) { ... },
})
sessions, err := host.Sync(ctx)
err = host.Reply(ctx, session, generation, id, answer)
host.DetachAll(ctx)
err = child.Stop(5 * time.Second)
```

`ctx` bounds the launch request only, never the child: the process is started
with `exec.Command`, so a caller's `defer cancel()` does not kill a managed Kimi
that Start just launched. The child ends through `Child.Stop`, which signals only
the pid it started.

`kimi.ClassifySurface(type, url)` returns `work`, `chat` or `other`. Only `work`
(`app:/file:/kimi:` + `/kimi-agent.html`) and `chat` (`https://www.kimi.com` with
`/`, `/chat/history`, `/chat/<id>`) receive anything. A query, a fragment, a
lookalike host, a non-`page` target or an unrecognised path is `other`.

Every reply is bound to the generation that asked for it, and the evaluation is
pinned to the main context the request came from.

## 5. Injection payload

```go
data, asset, err := injection.TranslationRenderer()
```

`assets/injection/translation-renderer.v1.js` is the 0.12.1 renderer, copied
byte for byte (SHA-256 `f130501caa33f85e1a90adf5bbe78255f6c1589c39727513877350a63eb97743`,
44,033 bytes). The host prepends `__kwgTranslationScope`, `__kwgTranslationGeneration`
and `__kwgTranslationConfig`; the payload is always the tail and is never
rewritten.

## 6. Platform

```go
lock, err := platform.AcquireLock(stateDir)   // platform.ErrLocked for a second owner
defer lock.Release()

store, err := platform.OpenCredentials("dev.kimi-work-plus.preview")
value, err := store.Get(key)                  // platform.ErrCredentialMissing
err = store.Set(key, value)
err = store.Delete(key)

desc, err := platform.DescribeService(platform.ServiceOptions{Binary: abs, StateDir: abs})
// desc.Install / desc.Uninstall / desc.Status: commands for the user to run
components := platform.Components()           // native gap report
```

There is no plaintext credential fallback: an unsupported platform answers
`ErrUnsupported`. The store never decrypts an old Electron `safeStorage` blob.

`DescribeService` renders the definition and the commands; it writes no file and
runs nothing. launchd uses `RunAtLoad=false` and `KeepAlive=false`, and the
Windows task is `ONLOGON` with `/RL LIMITED`. Nothing is registered at install
time.

## 7. Evidence

Verified with Go 1.26.10 at `toolchain/go/bin/go`, `GOPATH=toolchain/gopath`.

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go test ./...` | all packages pass |
| cross build darwin/windows/linux | pass |
| 20 synthetic fixture seeds vs node:vm | identical (`TestFixtureSeedsMatchNodeVM`) |
| real Kimi 3.2.25 `Inspect` | `verified`, 5 accepted, 12 functions, ~800 ms |
| real Kimi 3.2.25 launch probe | `unsupported/packaged_bundle_override_disabled` |
| time budget | killed at 1.20 s (`ErrProbeTimeout`) |
| RSS budget 96 MiB | killed at 180 ms (`ErrProbeResource`) |
| sandbox surface | 8/8 probes `undefined` |
| Keychain round trip | get/set/replace/delete pass on macOS |

The live tests are read-only and skip when no installation exists. They are
skipped, not failed, when `KIMI_SKIP_LIVE=1`.

## 8. Remaining gaps

- `remote_credentials`: existing secrets are bound to the released build's
  Electron identity. This build stores its own secrets natively and does not
  decrypt those blobs. Reported as `legacy-entry`.
- `official_updater`: the native addon drives Squirrel/Sparkle inside the
  official application's framework with code-signature checks. Not
  reimplemented; reported as `legacy-entry`.
- `remote_companion`: macOS-only in the released build; not built here.
- `DAIMON_BUNDLE_DIR` is disabled in packaged builds of Kimi 3.2.25, so the
  subagent bundle override is `unsupported` on the current release. This is a
  real capability limit of the installed application, not a migration defect.
- No Windows host was available. Windows code compiles and the path rules are
  unit-tested on macOS, but no Windows runtime behaviour is claimed.

---

# Round 2: Controller, native import, managed launch

## 9. Controller (the API the bridge calls)

```go
controller, err := kimi.NewController(kimi.ControllerSettings{
    Installation: install,                 // from kimi.Detect
    StateDir:     stateDir,                // this product's private directory
    UserDataDir:  isolatedProfile,         // must NOT be the signed-in profile
    GatewayURL:   "http://127.0.0.1:PORT/v1",
    GatewayToken: localToken,
    Models:       savedModels,
    AccountScope: kimiAccountID,           // when known
    AllowNativeImport: false,              // true only for a user-initiated import
    Translation: kimi.TranslationSettings{Enabled: true, Provider: "bing", TargetLanguage: "zh"},
})
controller.SetTranslationHandler(func(ctx context.Context, r kimi.TranslationReply) (any, error) {
    return yourProvider.Translate(ctx, r.Value)   // main session owns the provider
})
```

| Method | What it does |
|---|---|
| `Start(ctx)` | launches a managed child in `UserDataDir`, attaches CDP, injects |
| `Stop(ctx)` | ends only the child this controller started |
| `Tick(ctx)` | re-syncs injection, returns `TranslationStatus` |
| `Reconcile()` | runs the native integration, returns `IntegrationResult` |
| `ImportModels(ctx)` | native import through the managed bridge |
| `WorkSurface(ctx)` | what is on screen, through the bridge |
| `StartBridge(ctx, actions)` | publishes the loopback bridge descriptor |
| `Status(ctx)` | read-only `ControllerStatus` |
| `NativeState(ctx)` | read-only official-application state |

### The unmanaged-Kimi rule

`Start` calls `DetectState` first. If the official application is running, it
returns `ErrUnmanagedKimiRunning` and `ControllerState == "needs_user_close"`.
It does not attach, signal, or kill that process. If the process state cannot be
determined, it fails with `kimi_process_state_unknown` rather than assuming it is
not running. A managed child is only ever started into an isolated
`UserDataDir`; the signed-in profile is refused outright.

## 10. Native integration

`NativeIntegration.Reconcile` is the Go port of the released build's
`native-work-integration.mjs`. It reads Kimi's own `byok-providers.json` and
`config.json`, and writes only entries it can prove it owns:

- ownership is a SHA-256 digest recorded in `native-work-intent.json`; an entry
  whose bytes no longer match its digest belongs to the generator or the user
  and is reported as `foreign_provider` / `foreign_model`, never overwritten;
- `config.json` is re-read and compared byte for byte before every write, so a
  concurrent generator edit wins and the call reports `concurrent_modification`;
- a missing credential is `waiting-config/credential_not_generated`; the key is
  never invented locally;
- an account change is `account_changed` and is **not** in the importable set;
- the intent records digests only, never a token.

States: `idle`, `ready`, `repaired`, `conflict`, `needs-native-import`,
`waiting-config`, `pending`, `disabled`, `account-unverified`.
`IntegrationResult.NeedsNativeImport()` is true only for the reasons a native
import can actually repair.

## 11. Native bridge

`BridgeHost` serves `status`, `work-surface`, `import-models`,
`close-work-settings`, `activate-work`, `open-work-settings` on a loopback
listener bound to port 0, and publishes `.kimi-work-desktop-bridge.json`
(0600, 64-hex token). Every request is checked for `Origin`, `Sec-Fetch-Site`,
exact `Host` and a constant-time bearer token. `BridgeClient` refuses any
descriptor that is not `127.0.0.1` with a port, a 64-hex token and owner-only
permissions; it follows no redirect and uses no proxy.

## 12. Round 2 evidence

| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go test ./internal/kimi ./internal/platform ./assets/injection` | pass |
| cross build darwin/windows/linux | pass |
| native reconcile rules | 13 tests, including foreign-entry preservation, digest ownership, lock exclusivity, account-change refusal |
| bridge routes and auth | 8 tests, including wrong token, non-loopback descriptor, group-readable descriptor |
| managed child over a real private pipe | pass: fd3/fd4, isolated profile, only the Work surface attached |
| translation round trip | pass: binding → provider callback → pinned reply |
| unmanaged-Kimi guard | skipped on this host (no official Kimi running); the code path returns `needs_user_close` |

## 13. Round 2 gaps

- The managed child is a stand-in process, not Kimi. Transport and injection are
  proven; official-build behaviour is not.
- `ImportModels` needs a real Kimi settings page carrying
  `clawBridgeAPI.importByokModels`. The host route is implemented and tested
  against a fake `PageActions`; the page-side call is not exercised on a live
  application.
- Credentials and the official updater remain `legacy-entry`
  (`platform.Components()`), unchanged from round 1.
- No Windows host was available.

---

# Round 3: real PageActions, and five corrections

## 14. Real PageActions over CDP

`NewCDPActions(pipe, kimiDir, kimiHome)` is the production `PageActions`. Every
step is one `Runtime.evaluate` against an allowlisted desktop page
(`app://localhost/{kimi-agent,desktopclaw,work-settings}.html`, no query, no
fragment). No coordinate is driven, nothing is navigated, and no page content is
returned to a caller.

The expressions are the released build's own calls:

| Action | Call |
|---|---|
| import probe | `typeof globalThis.clawBridgeAPI?.importByokModels === "function"` |
| import | `clawBridgeAPI.revealByok()` then `clawBridgeAPI.importByokModels(token, baseUrl)` |
| open settings | `kimiAgentAPI.openSettings("general")` on the Work view |
| close settings | `clawBridgeAPI.closeSettingsPage()` + `__KIMI_DESKTOP_BRIDGE.closeKimiWorkSettingPage()` |
| activate Work | `__KIMI_DESKTOP_BRIDGE.openKimiWorkTab()` |
| surface | `document.visibilityState`, `#root` painted, `isSettingsPageOpen()` |

`ImportModels` ordering: expected account checked first (nothing is evaluated on
a mismatch), then the import page is looked for on every allowlisted target; if
absent and `OpenSettings` was authorised, `openSettings` runs, the target is
re-polled six times at 250 ms, the import runs, and the overlay is closed through
the official control. A confirmed import that still owes a restart reports
`RequiresRestart` rather than claiming it applied.

`Start` now publishes the bridge itself (`NewCDPActions` + `StartBridgeHost`), so
`ImportModels` is reachable after a launch. If the bridge cannot start, the
controller stays `ready` with reason `native_bridge_unavailable` rather than
pretending the launch failed.

## 15. The five corrections

| Issue | Fix |
|---|---|
| `Configure` mutated host options without the host lock | `Host.Configure` takes `optionsMu`, and every read of the options goes through an accessor. A scope change invalidates the sessions; a view-only change does not. |
| `Status` exposed the descriptor token and profile path | `Status` carries no descriptor at all. `PublicState` is the renderable view: it reports only `Bridge{Published, Contract}`. |
| `isUserProfile` was macOS-only and a single string compare | `validateProfile` requires the profile to resolve inside `StateDir`, with no symlink escape, on every platform. A child of the signed-in profile is refused. |
| `kimiHome` guessed `<profile>/daimon` | `ResolveKimiHome` follows Kimi's own rule: `daimon-storage.json` `shareDir`, else `<profile>/daimon-share/daimon`, and refuses anything resolving outside the managed profile. `ControllerSettings.KimiHome` is an override honoured only inside. |
| `Translation.Enabled == false` still injected | With translation off the host keeps the pipe (the bridge shares it) but injects nothing, attaches no surface, and reports `inactive/translation_disabled`. The native import bridge is published on that path too: it is not part of translation. |

## 16. Round 3 evidence

| Check | Result |
|---|---|
| `go vet` | clean |
| `go test ./internal/kimi ./internal/platform ./assets/injection` | pass |
| `-race` | clean |
| cross build darwin/windows/linux | pass |
| `ResolveKimiHome` | 5 tests: layout, shareDir, outside refusal, symlink escape, relative input |
| profile containment | outside, nested-in-real-profile, symlink escape all refused |
| `Configure` under concurrent `Sync` | race detector clean |
| `PublicState` | no token, no loopback URL, no path |
| CDP actions over a real private pipe | pass: surface read, import (3 models, restart owed), close, activate |
| account gate | a wrong expected key refuses before any evaluation |

## 17. What is still not proven

The child in the wiring tests is a stand-in that models the preload API surface.
That proves the calls, the ordering, the allowlist and the account gate. It does
**not** prove that the installed official build exposes
`clawBridgeAPI.importByokModels` with these semantics, because exercising that
requires driving the user's own running Kimi, which is out of bounds.

This item is therefore recorded as **blocked at the official-build level, not
implemented-and-verified**. The Go side is complete and reachable; the remaining
evidence needs a real application session that the user authorises separately.
No half-implemented path is presented as working.

---

# Round 4: native enhancements (subagents, context, services, MCP)

All four writers take an **explicit `kimiHome`**. There is no default: a caller
cannot reach the user's real installation by omitting an argument.

```go
subagents, _ := kimi.NewSubagentConfig(stateDir, kimiHome)
context,   _ := kimi.NewContextPolicyConfig(stateDir, kimiHome)
services,  _ := kimi.NewServicesConfig(stateDir, kimiHome)
mcp,       _ := kimi.NewMCPManager(transport)   // transport is the kernel client
```

## 18. Subagent policy — `subagent_config.go`

```go
status := subagents.Update(kimi.SubagentPolicy{
    Mode: kimi.SubagentModePool, ModelAlias: "byok-kth-kimi/kimi-a",
    Models: []kimi.SubagentPoolEntry{{ModelAlias: "…", Description: "…"}},
}, kimi.SubagentCapabilities{ForceKeySupported: &supported})
status := subagents.Inspect()
status := subagents.Close()
```

Modes `inherit` / `blocked` / `force` / `pool` are preserved exactly. The block
lives between `# BEGIN KIMI WORK GATEWAY SUBAGENTS` and its end marker, with an
owner line carrying the owner id and body hash. A block whose hash does not
match, a duplicate marker, a near-miss marker elsewhere, a foreign
`[secondary_model]` section and a symlinked config are all refused rather than
rewritten. `ForceKeySupported: false` omits `force = true`, which is what a
kernel that dropped the key needs.

## 19. Context policy — `context_policy.go`

```go
ratio := 0.85
reserved := int64(8000)
status := context.Update(kimi.ContextPolicy{Mode: "custom",
    TriggerRatio: &ratio, ReservedTokens: &reserved})
status := context.State()
```

`triggerRatio` is the decimal the schema takes (`0.50 … 0.99`); a percent is a
presentation concern. Both keys belong to the kernel's `LoopControlSchema`, whose
config section is `[loop_control]` — a table under any other name is not read at
all. Two layouts are supported: a marker block, and a plain `[loop_control]`
table tracked by a receipt that records the original line of every key it
replaced, or that the key was absent, so a release restores the file exactly. A
value this module did not write is `foreign_value`, never overwritten. A layout
that cannot be edited without guessing — `[[loop_control]]`, `[loop_control.x]`,
a root `loop_control.x = …`, a second header, a duplicated key — is
`unsupported_layout`, `duplicate_table` or `duplicate_key`, never a rewrite.

## 20. Web services — `services_config.go`

```go
status := services.Update(kimi.ServicesInput{
    Search: &kimi.ServiceSettings{Enabled: true, BaseURL: "https://…/search", APIKey: "…"},
    Fetch:  &kimi.ServiceSettings{Enabled: false},
})
status := services.State()
intent := services.Intent()
```

Search and fetch are independent: omitting one keeps its saved intent, and
`Enabled: false` restores only that table. Rules enforced: an enabled service
needs both an endpoint and a key; plain HTTP is accepted only for loopback; a
query string is refused; a saved key is reused only for the same endpoint; a
table this module did not write is `foreign_service`.

## 21. MCP — `mcp_manager.go`

```go
status := mcp.List(ctx)
status := mcp.Add(ctx, kimi.MCPServer{Name: "files", Transport: "stdio",
    Command: "/usr/bin/mcp-files", Args: []string{"--root", "/tmp"}})
status := mcp.Update(ctx, "files", kimi.MCPServer{Enabled: &off})
status := mcp.Remove(ctx, "files")
status := mcp.Test(ctx, "files", "")     // the only call that can start a process
status := mcp.Close(ctx)
```

Validation is per field against an allowlist, per transport family (`stdio` and
`http`/`sse` cannot share fields), with `__proto__`/`prototype`/`constructor`
refused outright. Every value a caller sees is redacted: command and arguments
are clipped and scrubbed, headers and env are reduced to names, a URL loses its
userinfo and query, and a probe's output is bounded and redacted. A second
`Test` while one is unsettled is refused rather than overlapping.

`MCPTransport` is an interface, so the installed kernel's client plugs in and a
test supplies a fake.

## 22. Round 4 evidence

| Check | Result |
|---|---|
| new tests | **33 pass** (`TestSubagent*`, `TestContextPolicy*`, `TestServices*`, `TestMCP*`) |
| `go vet` | clean |
| cross build darwin/windows/linux | pass |
| byte-exact restore | `inherit` and `Close` reproduce the original file, including CRLF and a missing trailing newline |
| foreign protection | foreign `[secondary_model]`, foreign service table, foreign context value, modified block, duplicate markers, symlinked config — all refused |

## 23. What is not implemented, and why

| Item | State | Why |
|---|---|---|
| MCP kernel client | **blocked** | The installed application exposes its client as a JS module. `MCPTransport` is the seam; a Go implementation needs the kernel's own contract, which is not readable from outside the running app. The manager itself is complete and tested against a fake. |
| `findInstalledKernel` | not ported | It resolves a path inside the app bundle; `internal/kimi` already discovers the install, and a second resolver would be a parallel implementation with its own drift. |
| Search/fetch *verification* | source + fixture only | The tables are written and read back; whether the kernel routes `WebSearch` through them needs a live session. |
| Subagent *effect* | source + fixture only | The block is written; whether the kernel honours `force`/`pool` for a given build needs a live session and is what `SubagentCapabilities` records. |

None of these removes an enhancement: the configuration surface for all four is
implemented, and the parts that need a live application are named as such rather
than presented as working.

---

# Round 5: real MCP store, remote bundle, panel bindings

## 24. MCP is a JSON file, not the kernel API

Reading the shipped kernel bundle settled what round 4 left open. The MCP server
list is **not** in `config.toml` and does **not** need the kernel's JS client:

```js
const MCP_CONFIG_KEY = "mcp.json";
constructor(storage, bootstrap) { this.path = join(bootstrap.homeDir, "mcp.json"); }
async list()  { return (await this.read()).servers; }
async write(file, rawServers) {
  const text = `${JSON.stringify({ ...file.raw, mcpServers: rawServers }, null, 2)}\n`;
  await this.storage.write(CONFIG_SCOPE, MCP_CONFIG_KEY, encoder.encode(text), { atomic: true });
}
function persistedEntry(server) { const { name: _name, ...entry } = server; return entry; }
```

So `list`, `add`, `update` and `remove` are file operations, and
`mcp_store.go` implements them:

```go
store, _ := kimi.NewMCPFileStore(kimiHome)   // explicit home, no default
manager, _ := kimi.NewMCPManager(store)      // the existing CRUD surface
```

The store matches the kernel's own behaviour: `{"mcpServers": {name: entry}}`,
entries without a `name`, `transport` inferred from `command`/`url` when absent,
two-space JSON with a trailing newline, atomic replace, duplicate add refused,
unknown update refused, remove idempotent, and the file's key order preserved so
the list does not reshuffle on every read. Unknown **top-level** keys survive a
write.

## 25. The one part that is genuinely blocked

`testGlobalMcpServer` is not a file operation:

```js
async testGlobalMcpServer(name, options) {
  const server = await this.globalMcpConfig.get(name);
  return { ...await this.engineAccessor.get(IMcpManagementService).testServer({ server, ...cwd }) };
}
```

`IMcpManagementService` owns the connection manager: it spawns the process or
opens the connection, performs the MCP handshake, lists tools and shuts the probe
down. There is no file to read and no endpoint to call; it is the kernel's own
runtime. `MCPFileStore.TestServer` therefore returns `ErrMCPTestUnavailable`, the
manager maps it to `native_mcp_test_requires_kernel_runtime`, and
`MCPBinding.Probe.Available` is false so the panel disables the button instead of
showing a failure after a click.

## 26. Remote bundle: implemented, with its boundary named

`remote-bundle.mjs` splits into data and in-process work. The data part is now Go:

```go
result, err := kimi.BuildRemoteAsar(files, []string{"package.json", "bootstrap.mjs"})
ok := kimi.VerifyBundleSignature(bundle)        // codesign --verify --strict
ok := kimi.VerifyRemoteBundleMetadata(metadata, bundleID, executableName)
data, ok := kimi.ReadRemoteAsarEntry(result.Data, "bootstrap.mjs")
```

`BuildRemoteAsar` is **byte-identical** to the released build for the same input
and the same entry order (`TestRemoteAsarMatchesTheReleasedBuild`, fixture
generated by `remote-bundle.mjs`). The order is an argument rather than a map
iteration because it is part of the archive's bytes; a mismatched order is
refused, not silently reordered.

Not implemented, and why:

| Piece | Why not Go |
|---|---|
| companion `bootstrap.mjs` | runs inside Electron's main process; it is a versioned JS asset, not portable |
| `installRemoteUpdateProxy` | replaces `setFeedURL`/`checkForUpdates`/`quitAndInstall` on a live Electron object; there is no Go equivalent inside that process |
| `remote-official-updater.node` | drives Squirrel/Sparkle and checks ShipIt's signature; that is the addon's contract |
| `remote-credentials-service` | needs Electron's `safeStorage` under a fixed bundle identity; `internal/platform` provides a native store for this build's own secrets but cannot decrypt those blobs |

## 27. Panel bindings — no file write is reported as adoption

The released panel renders three layers and refuses to collapse them. The Go side
now produces exactly that shape:

```go
binding := subagents.Binding(policy, observed)   // {configured, policy, runtime}
binding := context.Binding(policy)               // {policy, state}
binding := services.Binding()                    // {intent, state}
binding := mcp.Binding(ctx)                      // {servers, probe, state}
```

`SubagentConfigured` emits both `model` and `modelAlias`, and pool rows as
`{id, description}`, because that is what the existing page reads. `Runtime.State`
is `file-applied` for a written file and only becomes `loaded` when the caller
supplies `observed: true`; even then it reports `observed-schema`, which the panel
treats as "the tool structure updated", never "this model is chosen".

`ContextBinding` and `ServicesBinding` are intermediate structures, not the
page's own contract: the page reads `ctx.policy` as the request policy with
`ctx.status` as a state string, and `search.services[name]` as
`{enabled, endpoint, keySet, status}`. `internal/bridge/policies.go` performs that
mapping, because the saved key must not leave the process.

## 28. Round 5 evidence

| Check | Result |
|---|---|
| tests in `internal/kimi` | **159 pass** |
| `go vet` | clean |
| cross build darwin/windows/linux | pass |
| Go asar vs released build | byte-identical, same header digest |
| MCP store vs kernel shape | read, add, update, remove, unknown top-level keys, key order |
| panel bindings | no secret rendered, no adoption claimed from a write |

Nothing in this round wrote to the real installation: every test used a temporary
Kimi home, and the only real-machine access was `os.Stat` on the installed kernel
bundle to confirm it exists.

## 29. Round 6: restart safety and input boundaries

Every writer is constructed per call, so a restart is a second instance over the
same state directory. `restart_test.go` covers that path for all three config
writers, and the audit found six real defects behind it:

| Defect | Consequence | Fix |
|---|---|---|
| the context table was `[compaction]` | the kernel registers `loopControl`, so the values were never read | `contextTable = "loop_control"`, matching `LoopControlSchema` |
| two keys appended the table header twice | the file held two `[loop_control]` headers, which no TOML parser accepts | edits are planned first and the file is rebuilt once |
| the context writer emitted the subagent markers | a context write claimed the subagent block's identity | the block markers are a parameter of the shared editor |
| a receipt could not record that a key was absent | the second write adopted its own line as the original, so a release restored the written value | `contextOriginalAbsent` sentinel |
| narrowing a policy left the dropped key behind | the value stayed with no receipt claiming it, so a release reported `foreign_value` | the dropped field is handed back in the same write |
| the subagent receipt had no `previous` journal | a crash between the receipt and the config write was reported as `owner_conflict` instead of `handoff_pending` | `previous{ownerID, blockHash}` restores the released `superseded`/`recoverable` semantics |

`ownsValue` compares the recorded line, not the owner id: a restart creates a new
owner, so requiring the same owner would turn this module's own configuration
into a foreign value after every restart.

`scanContext` refuses a layout it cannot extend without guessing —
`[[loop_control]]`, `[loop_control.x]`, a root `loop_control.x = …`, a second
header, a duplicated key — instead of appending beside it.

`ContextBinding` and `ServicesBinding` are intermediate structures; the panel's
`{policy, status}` and `{enabled, endpoint, keySet, status}` shapes are produced by
`internal/bridge/policies.go`.

## 30. Integrated preview acceptance boundary

The final macOS preview started the installed Kimi 3.2.25 executable with its own
profile and private CDP pipe. The managed process outlived the launch request.
With translation disabled the native bridge still answered an import request;
the isolated official window required login and the request returned
`native_bridge_waiting_settings`. Model adoption and translated reasoning in a
logged-in official session remain unverified.

Windows cross-compilation is not native-launch acceptance. `StartKimi` currently
uses `exec.Cmd.ExtraFiles` for fd3/fd4, which Go does not support on Windows.
Windows handle inheritance and Chromium pipe transport remain an implementation
blocker, even though both desktop and Gateway PE executables build successfully.

The bridge reconciles owned context/subagent/services settings when the isolated
configuration file appears or changes. File-level tests cover delayed creation,
no-op observations and preservation of foreign values; real kernel reload is
still not established. Remote companion/updater and old Electron safeStorage
compatibility remain outside the implemented equivalence boundary.
