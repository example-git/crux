# Crux and upstream Crush: implementation comparison

Crux began as a derivative of
[Charmbracelet Crush](https://github.com/charmbracelet/crush), but its runtime,
state, provider, remote-execution, and automation behavior has diverged.
This document records the differences established by a code-only comparison so
users and maintainers do not mistake a shared ancestry for a shared contract.

## Scope and method

The upstream reference was cloned into local scratch space on 2026-09-17 at
commit `ef7bae117905a3927b177ed6923f36e811737d53`. The comparison examined Go
source, Go tests, JSON schemas, and module/build metadata in that checkout and
in this Crux checkout. It intentionally did **not** use either project's
README, release notes, or other documentation as comparison evidence.

This is a source-level comparison, not a compatibility certification or a live
interoperability test. “No counterpart found” means that an equivalent package,
symbol, schema field, or code path was not found in the inspected upstream
tree; it does not prove that a capability can never be implemented elsewhere or
in a later upstream revision.

## Shared foundation

Both trees retain a terminal coding-agent architecture with interactive
sessions, repository tools, LSP integration, MCP support, permissions, hooks,
skills, configuration, and a client/server workspace abstraction. The
important question is not whether a similarly named concept exists, but whether
it has the same execution and ownership model. The sections below call out the
related upstream implementation where one exists.

## Application identity and persisted state

| Concern | Crux implementation | Upstream counterpart | Practical difference |
| --- | --- | --- | --- |
| Module and executable identity | `go.mod` declares `github.com/example-git/crux`; `internal/cmd/root.go` uses `crux`. | `github.com/charmbracelet/crush`; root command uses `crush`. | The binaries, imports, generated command references, and user-facing identities are different. |
| Workspace state | `internal/config/config.go` uses `.crux`; `internal/config/load.go` derives `crux.json`. | The equivalent upstream files use `.crush` and `crush.json`. | The default project state locations do not overlap. |
| Project instructions | Crux recognizes `CRUX.md` and `CRUX.local.md` variants in `internal/config/config.go`. | Upstream recognizes the corresponding Crush-named variants. | Project context file names are intentionally project-specific. |
| Runtime configuration | Crux's loader accepts isolated and remote-client loading modes and records provider ownership state in `internal/config/load.go`. | Upstream loads provider configuration directly from its catalog/configuration path. | Configuration with the same provider name is not necessarily interchangeable. |

## Provider, model, and account ownership

Crux changes provider configuration from a provider-ID record into an
owner-bound runtime contract.

### Provider provenance

`internal/config/config.go` defines `ProviderOwnerReference`,
`ProviderPluginReference`, and `ProviderPresetReference`; `ProviderConfig`
serializes `owner`, `plugin`, and `preset`. It also rejects bare API keys and
OAuth tokens in `ProviderCredentialOwner`: a credential mutation must identify
the provider owner that initiated it.

The inspected upstream `ProviderConfig` contains endpoint, type, credential,
model, and option fields, but no serialized provider owner/plugin/preset
references. It accepts raw credential values by provider ID. This is a
behavioral difference: Crux can preserve a configuration and mark it
unavailable when its exact owner disappears, rather than treating a same-ID
endpoint as an interchangeable replacement.

### Registry and catalog behavior

Crux's `internal/providerregistry/registry.go` creates immutable registrations
and rejects duplicate provider IDs, aliases, and nonempty account namespaces.
`internal/providerplugin/catalog.go` exposes plugin catalog entries only for
registered manifests, while `internal/config/provider.go` carries a provider
scan and registry together. A catalog entry is therefore not, by itself,
authority to execute a provider.

The upstream provider path instead uses Catwalk catalog data in
`internal/config/provider.go`, overlays saved configuration in
`internal/config/load.go`, and can materialize a known provider during
credential saving in `internal/config/store.go`. Crux replaces that model with
explicit registry ownership and plugin/preset identity.

### Model and OAuth mutation rules

Crux's `UpdatePreferredModelForOwner` verifies that both the selected model and
active registration match the expected owner. Its provider-authentication
service (`internal/providerauth/`) stores workspace-scoped operation state,
target validation, generations, and retained login state; matching commits are
staged through `internal/config/oauth_login_commit.go`.

Upstream's comparable selection and token-refresh paths are provider-ID based.
It has per-provider refresh coordination, but no inspected equivalent of Crux's
owner-bound model selection, account namespace registry, or workspace-scoped
retained authentication operation service.

**What this means in use:** Crux does not use provider IDs alone as authority
for selecting models, saving credentials, or completing authentication. A
provider/plugin replacement can remain visibly unavailable instead of silently
adopting a different owner.

## Remote execution is a different deployment model

Upstream contains a client workspace proxy with SSE reconnection
(`internal/workspace/client_workspace.go`), but the inspected upstream client
is a generic transport client. Crux adds an authenticated, client-authoritative
remote runtime protocol around the basic workspace concept.

### Authenticated enrollment and transport

- `internal/connection/tls.go` requires TLS 1.3 mutual authentication, verifies
  client certificates, and pins the saved server certificate on the client.
- `internal/connection/enrollment.go` implements temporary enrollment with a
  random token, finite lifetime, certificate pinning, staged client identity,
  and explicit approval.
- `internal/connection/live_authorization.go` binds request lifetime to a
  principal and cancels authorized work during revocation.
- `internal/cmd/connections.go`, `server_setup.go`, and `internal/daemon/`
  provide connection, setup, revocation, and managed-service paths.

No `internal/connection` or `internal/daemon` subsystem, remote-runtime schema,
or matching setup/connection command implementation was found in the inspected
upstream tree.

### Client authority is executable, not advisory

`internal/config/remote_runtime.go` defines a revisioned
`RemoteRuntimeProposal` containing provider definitions, selected models,
credential bindings, required bundles, controls, and remote authority data.
`RemoteAuthority` deliberately excludes private credential values from the
receiver acknowledgement.

On the production path, `internal/cmd/root.go` negotiates and submits this
runtime before creating a client-authoritative remote workspace.
`internal/server/server.go` puts non-loopback remote servers into a
client-runtime-only mode, and `internal/config/remote_runtime.go` validates the
principal and expected revision before publishing a replacement.
`internal/workspace/client_authority.go` publishes client changes and requires
a matching acknowledgement.

The result is distinct from forwarding a request to a generic server: for a
client-owned Crux workspace, the client selects the provider, model, and active
credential binding. The receiver executes the accepted runtime and rejects
stale, unauthenticated, or incompatible replacements instead of using its own
configuration as a silent fallback.

This behavior is also exercised by source tests such as
`internal/server/remote_authority_baseline_test.go` and
`internal/cmd/headless_authority_integration_test.go`, which cover unauthorized
admission, rejected authority publication, and no-fallback behavior. Those
tests were inspected, not executed for this document.

## Task lifecycle and durable agent workflow

### From background shells to managed work

Upstream has an in-memory `BackgroundShellManager` in
`internal/shell/background.go`. It starts, lists, waits for, and kills shell
processes while retaining their buffers in that manager.

Crux adds a general task model in `internal/task/` with pending, running,
completed, failed, killed, and lost states; durable task views; bounded output
retrieval; and completion notifications. `internal/agent/background_agent.go`
uses it for background agents, and `internal/imagegen/jobs.go` uses it for image
jobs. Workspace, server, and client channel code carry task events across the
remote workspace boundary.

**What this means in use:** Crux manages shells, agents, and image jobs through
one lifecycle and can explicitly mark a recovered nonterminal task as lost.
That is broader than the upstream in-memory shell facility; it is not a claim
that the two task APIs or identifiers are compatible.

### Plans, projects, and notes

Both trees have a recent-project registry. Crux additionally has
`internal/projects/service.go`, whose `Service` creates active/paused/completed
project documents with goals, success criteria, task trees, roots, and notes.
The agent registers project tools in `internal/agent/tools/projects.go`.

No corresponding upstream project-plan service, notes store, project document,
or project-tool surface was found. Upstream's first-time project initialization
and recent-directory tracking are related concepts, but they are not a durable
agent-managed plan.

### Memory and semantic retrieval

Crux's `internal/automemory/` service stores user/project scopes and its worker
can enqueue transcript-derived memory mutations and consolidation.
`internal/codebaseindex/` provides index lifecycle state, checkpoint/recovery
work, embeddings, ranked search, and a `codebase_search` agent tool. The tool
only serves a ready index and reports unavailable/building/failed state instead
of returning ordinary search results.

No `automemory`, `codebaseindex`, `semanticembedding`, or `codebase_search`
counterpart was found in the inspected upstream source/schema. Upstream LSP
semantic-token configuration is not an implementation of a repository semantic
index.

## Additional tool boundaries

### Image generation and editing

Crux has an `internal/imagegen/` subsystem and an `imagegen` agent tool. The
tool validates generate/edit mode and file/output choices, requests permission,
and queues a managed job rather than blocking the agent call. It supports
provider-selected models and bounded multi-output work. It also rejects
subagent calls in the inspected tool constructor.

No image-generation package, tool, or image-provider schema counterpart was
found upstream. This concerns generation/edit job execution; it should not be
confused with ordinary image attachment support.

### Browser-authenticated fetch

`internal/cookieutil/` imports supported Chromium/Firefox profile cookies.
`internal/agent/tools/fetch_browser.go` requires an interactive consent path,
uses the selected profile's user agent and cookie jars, and rejects non-HTTP(S)
URLs and embedded URL credentials before creating a client. The imported cookie
state stays request/session-local rather than being written into configuration.

The upstream tree opens browser URLs for OAuth in its workspace code, but no
browser-cookie import or cookie-backed fetch implementation was found. Opening
an OAuth URL is not equivalent to sending a user-authenticated web request.

### Traffic and provider diagnostics

Crux's `internal/trafficcapture/` plus `traffic_capture` tool can launch an
isolated capture around one executable or a relaunched PID and return capture/
viewer metadata. The selected target runs quietly behind a private terminal;
attachment opens an independent management shell with native `cap` controls for
logs, starting additional processes, stopping, and restarting. Target exit leaves
the shell and viewer available, log-follow interruption does not stop the target,
and detaching preserves the capture. Exiting the shell ends the capture and
stops its processes. `wait` reports the initially selected target's exit without
closing the shell. On macOS, an ad-hoc-signed launcher and packet-tunnel provider
apply the app rule to the launcher and its helpers. An embedded Go WireGuard
client connects those packets to mitmproxy without a separate WireGuard app or
userspace TCP/IP stack. Capture-scoped TLS relaxation does not modify host trust
stores. The tool rejects invalid target combinations and invalid
capture paths before launch. `internal/providerdiagnostics/runner.go` separately
runs account, usage, and connection checks for a configured provider and
produces redacted diagnostic output through the plugin command path.

No matching traffic-capture or provider-diagnostic package/tool was found in
the upstream source. These are separate mechanisms: capture records process
traffic, while provider diagnostics evaluate configured provider operations.

## CLI and automation surface

### Native CLI differences

Crux uses `crux`, `CRUX_PROFILE`, `.crux`, and `crux.json`; upstream uses the
corresponding Crush names. Crux additionally registers plugins, accounts,
export/import, and connection-related command paths where upstream registers a
provider-update path. Its schema command emits additional provider, preset,
image, branding, and remote-runtime targets.

Crux's release configuration also differs at the implementation level:
`.goreleaser.yml` builds a narrower target matrix and includes Crux schemas,
provider artifacts, remote-runtime schema, and compatibility material. The
inspected upstream GoReleaser configuration has additional packaging and
publishing blocks for its own distribution channels. Neither configuration
proves that an artifact has actually been published; it establishes the
configured build/release intent.

### Optional external command adapters

Crux alone has `internal/compatibility/` and built-in `crux alias` commands. `main.go`
registers adapters and dispatches from the invoked executable name before
normal Cobra execution. The declared aliases are `codex`, `claude`, `agy`, and
`copilot` (`internal/compatibility/localaddon/runtime.go`).

The adapter request model supports interactive/headless operation, stdin and
stream input, session modes, machine-readable output, permission/sandbox
mapping, and bounded timeout/turn/budget fields. Source tests cover concrete
translations such as Codex JSON-lines execution, Codex app-server stdio,
Claude print/JSON, and Agy plan/JSON requests.

`crux alias` supplies local `install`, `enable`, `disable`, `status`, `repair`,
and `uninstall` operations. Install and uninstall select one provider or `--all`;
no separately compiled manager is required. Disabled aliases can forward to a later executable
on `PATH`; this behavior is defined in
`internal/compatibility/localaddon/forward.go`. The code establishes the
adapter mechanism, not that every vendor feature, account, cloud service, or
currently installed alias is available.

## Interaction differences visible in code

Crux's global key map (`internal/ui/model/keys.go`) includes provider,
instruction, semantic-index, Queue/Steer, plan, and background-detach controls.
A saved remote connection can enter `internal/ui/menushell/`, a dedicated
workspace-selection UI. The upstream key map retains its own generic mode and
editor-selection bindings. These declarations demonstrate different interaction
surfaces, but this document does not claim every key is reachable in every TUI
state without a live interactive test.

## Migration implications

1. Use Crux configuration/state names and the `crux` executable explicitly;
   do not assume project state, environment variables, or context files are
   shared with Crush.
2. Re-establish provider configuration through a valid Crux provider owner,
   plugin, preset, or custom provider path. A familiar provider ID does not
   confer authority in Crux.
3. Treat Crux remote workspaces as a paired, authenticated deployment with
   client-owned runtime authority, not as a generic upstream server endpoint.
4. Test external-tool automation against the specific Crux compatibility
   adapter you intend to use. The presence of an alias does not imply complete
   vendor-product emulation.
5. Use Crux task/project/memory/indexing/image/diagnostic interfaces on their
   own terms; related upstream facilities do not imply shared persistence or
   lifecycle semantics.

## Evidence limits

The comparison intentionally avoids copying product documentation into a new
product document. Every major claim above is grounded in the listed source,
test, schema, or build-metadata paths. It does not report a build, test run,
network request, installed alias, or live remote connection as having been
verified. Re-run the code comparison against a new upstream commit when the
reference checkout changes.
