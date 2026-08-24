# Provider plugin manifest v1

Provider plugins are versioned, data-only manifests interpreted by bounded host primitives. Bundles never contain executable, WebAssembly, RPC, subprocess, or daemon behavior. They do not gain trust, credentials, or network access merely because they exist.

This document is normative for `manifest_version: 1`. Full provider manifests use [`provider-plugin.schema.json`](../../provider-plugin.schema.json). Provider preset manifests use [`provider-preset-plugin.schema.json`](../../provider-preset-plugin.schema.json). The Go semantic validators are in `internal/providerplugin/manifest`.

## Bundle types

A missing `plugin_type` or `plugin_type: "provider"` selects the full declarative provider contract documented below. It may declare bounded authentication, operations, configuration, and capability metadata interpreted by the host.

`plugin_type: "provider-preset"` selects the separate Catwalk-compatible catalog contract. Its `preset` contains only provider identity, Foundation-owned implementation `type`, endpoint, environment-variable credential reference, headers, defaults, and models. Presets cannot declare OAuth, operations, compatibility adapters, static files, or executable behavior. They select an implementation already compiled into Foundation and contribute no provider registry registration.

Trusted compatible presets replace an existing catalog entry in place by provider ID or append a new entry. They remain catalog data under every runtime ownership profile; profiles still control which executable provider implementations are compiled and selectable. See [`deepseek-preset.plugin`](examples/deepseek-preset.plugin) for the canonical example.

## Installation sources and bundle shape

A plugin is installed from exactly one of these v1 sources:

1. a local directory containing `manifest.json` at its root; or
2. an HTTPS Git repository containing `manifest.json` at its root.

A source directory does not need a `.plugin` suffix. The installer validates and copies it into an immutable host-owned snapshot whose direct-child name ends in `.plugin`. Provider initialization reads only that installed snapshot; it never executes from the source directory, a mutable Git checkout, a worktree, `PATH`, or the network.

```text
<global-data-directory>/plugins/
└── example.echo.plugin/
    ├── manifest.json               required, UTF-8 JSON
    └── instructions/               optional declared UTF-8 text
```

The canonical installation root is `plugins` beneath `GlobalWorkspaceDir()`: `$CRUX_GLOBAL_DATA/plugins` when the override is set, otherwise `$XDG_DATA_HOME/crux/plugins`, `%LOCALAPPDATA%/crux/plugins` on Windows, or `~/.ai-cli/data/crux/plugins`. Acquisition staging is `plugins` beneath `GlobalCacheDir()`: `$CRUX_CACHE_DIR/plugins`, `$XDG_CACHE_HOME/crux/plugins`, the Windows local cache, or `~/.cache/crux/plugins`. Trust and provenance are private host state beneath `<global-data-directory>/plugin-state`; project `.crux` directories are never searched.

The example directories in [`examples`](examples) use this layout. The source/destination directory name is a location, not an identity or trust decision. `manifest.json.id`, the canonical snapshot digest, publisher identity, provider identity, and declared capabilities form the trust identity.

For a local source, the installer snapshots the bytes present when installation begins. For Git, it uses an in-process Git implementation rather than a shell command, resolves `--ref` (branch, tag, or commit) to an exact commit, and snapshots that tree. When `--ref` is omitted, the remote default branch HEAD is resolved once and the resulting commit is recorded. Submodules, Git LFS placeholders, alternate object stores, local/file transports, hooks, and symlinks are not followed or executed. V1 remote installation accepts HTTPS URLs; a private repository can be cloned by the user and installed through the local-directory path until a host-owned Git credential broker is defined.

Installation is explicit and transactional. Existing plugin IDs are not overwritten without an explicit update operation. The CLI trusts only the exact validated digest produced by that explicit install or update; `--no-trust` installs it for inspection without activation. Updates repeat validation and exact-digest trust evaluation; installed Git plugins are never auto-pulled. A failed copy or validation leaves the previous installed generation intact. If persisting trust fails after a first install commits, the new bundle remains installed but untrusted and inactive.

During first initialization, the host may offer:

```text
crux plugins install <directory-or-https-git-url> [--ref <branch|tag|commit>] [--no-trust]
```

The user may install one or more sources or skip provider installation and continue in a valid core-only state. Source strings and Git references are installer input, not persisted manifest identity and are never exposed to provider configuration.

The host enforces 64 MiB per bundle, 32 MiB per file, 1,024 files, 256 directories, depth 16, and 1,024-byte relative paths. It validates direct children only, computes and verifies the canonical whole-bundle SHA-256, binds install-time or separately granted trust to that exact digest, quarantines all duplicate plugin/provider claimants, and persists trust privately and atomically. Regular archive files are not v1 installation sources and are rejected rather than extracted; malformed archive inputs therefore fail closed. The host rejects symlinks/reparse points, non-regular files, special devices, path escapes, case-colliding paths, digest changes during snapshotting, and Git trees with unsupported entry modes.

### Status interfaces

`crux plugins list` exposes the local execution host's authoritative status; `--json` returns the complete revisioned snapshot. Each entry includes bundle type, plugin and provider identity, version, canonical digest, lifecycle state, independent trust and compatibility states, declared capability groups, safe provenance, installation time, and bounded redacted validation diagnostics. An empty directory returns a valid core-only snapshot.

A Crux server exposes the same redacted status through `GET /v1/plugins`, and the typed client exposes it as `Client.PluginSnapshot`. This endpoint is host-global rather than workspace-scoped: a remote client reports the server's installed plugins and never substitutes plugins from the client machine. The current server transport has no administrative authentication boundary, so installation, trust changes, and rescans intentionally remain local-host CLI operations; no unauthenticated remote mutation endpoint exists.

## Identity and compatibility

Required root fields:

| Field | Contract |
| --- | --- |
| `$schema` | Optional editor hint. It has no compatibility or trust semantics. |
| `plugin_type` | `provider` for this full contract; omission preserves compatibility with existing v1 bundles. |
| `manifest_version` | Exact integer schema major. V1 accepts only `1`; omission is invalid. |
| `id` | Stable plugin identity: lowercase namespaced segments separated by `.` or `-`. It does not change across releases. |
| `version` | Canonical SemVer without a `v` prefix. Build metadata is allowed but is not an identity substitute for the bundle digest. |
| `name`, `description` | Bounded display metadata, never used as identity. |
| `publisher` | Stable publisher ID plus display/support metadata. Trust is still exact-digest-bound and host-owned. |
| `compatibility` | Inclusive host API bounds, optional host release bounds, and required/optional feature IDs. |
| `provider` | The one logical provider registered by this bundle. |
| `models` | Closed, ordered model catalog. At least one model is required. |
| `capabilities` | Named provider-neutral declarations and operations. |
| `configuration` | Strict Draft 2020-12 schema and generic presentation hints. |
| `extensions` | Optional opaque `x-*` values. Unknown ordinary root fields are rejected. |

`compatibility.host_api.min` and `.max` are inclusive. A host API outside that range makes the plugin incompatible. An unknown `required_features` value is incompatible; an unknown `optional_features` value is ignored and preserved. Feature IDs are stable lowercase namespaced identifiers.

`compatibility.host_version` uses inclusive canonical SemVer bounds. It is a release constraint, not a replacement for host API and feature negotiation. Omitting it permits every host release that satisfies the API and feature contract.

### Stable logical provider identity

`provider.id`, `provider.account_namespace`, model IDs, metadata namespaces, and configuration keys are migration identities. Display names, brand fields, catalog position, and bundle filename are not identities.

Integrated, compatibility-backed, and plugin-native implementations must use the same logical provider ID. One immutable registry generation has exactly one owner for that ID. Bundle presence and discovery order never select ownership.

`provider.aliases`, `provider.login_order`, `provider.account_order`, and both brand gradient colors are explicit presentation/account declarations. `legacy_account_aliases` declares migration aliases only. It does not create additional credential stores. `default_large_model` and `default_small_model` must reference entries in the same manifest; no first-model fallback is applied.

### Temporary compatibility adapters

A migration bundle may declare `capabilities.compatibility_adapter`. Its `id` must name a host-known `integrated-*` adapter and `delegates` must include `construction`. Every `inventory` item names its delegated capability, classifies the remaining behavior as `finite-core-primitive` or `private-stateful`, and describes the boundary. Finite items must name a proposed bounded primitive; private/stateful items cannot claim one. Every delegate must have at least one inventory item. The declaration contains no code and cannot name another provider's adapter or an unknown capability group.

Compatibility declarations are migration scaffolding, not an executable extension mechanism. `finite-core-primitive` means a provider-neutral bounded interpreter must exist and pass parity tests before the delegate is removed. `private-stateful` means behavior remains in the named host adapter until it can be represented by a finite host-owned state machine with explicit lifecycle, bounds, and tests. Construction is removed last.

### Host rollout profiles

The execution host selects one immutable startup profile with `CRUX_PROVIDER_PROFILE`:

- `core-only` keeps core-owned capabilities and ignores provider bundles;
- `integrated` enables integrated providers and ignores provider bundles;
- `plugin-compat` enables trusted native bundles and selected compatibility bundles;
- `plugin-native` enables only trusted native bundles plus core-owned capabilities.

`CRUX_PROVIDER_PLUGINS` is an optional comma-separated allowlist of independently enabled provider IDs. An empty allowlist permits every trusted bundle allowed by the profile. Unknown profile names fail closed. A release may compile a narrower profile ceiling; environment settings cannot broaden that ceiling. Installation and exact-digest trust never bypass the rollout profile. Profile and registry ownership are immutable for the host process, so restart the execution host after changing installation, trust, or rollout settings.

Missing, disabled, invalid, incompatible, untrusted, or profile-excluded bundles remain explicit unavailable providers when configuration has a durable plugin ownership reference. Configuration, account records, selected models, recent models, and opaque transcript metadata are preserved. Crux never downloads or activates a replacement automatically.

### Upgrade migration and rollback

When a configured provider is first owned by an active manifest, Crux adds a generic `{id, version}` plugin reference to that provider configuration. Before the atomic config write it creates private pre-image backups of the global config and account store plus a versioned journal under the host-global data directory. OAuth values, account IDs/namespaces, selected and recent models, and provider configuration remain unchanged. Transcript metadata uses the opaque on-read migration and is not rewritten.

`crux plugins rollback-migration` restores the latest configuration pre-image only when the current config still matches its journaled post-image. This compare-and-swap rule prevents rollback from overwriting later config edits. The account backup is integrity-verified but the live account store is neither compared nor rewritten, because the migration never mutates it and later account changes must survive rollback. Restart Crux after rollback. Backups contain credentials and therefore remain private host state; do not copy them into a plugin bundle or project repository.

## Model catalog

Each model declares:

- stable `id` and display `name`;
- positive `context_window` and `default_max_tokens`, with output not exceeding context;
- zero-or-positive input/output and cached pricing per million tokens;
- explicit input and output modalities;
- optional reasoning levels/default and numeric budget bounds;
- optional JSON `default_options` interpreted only by declared runtime controls/transports;
- optional namespaced `extensions`.

Model order is display order. Duplicate IDs are invalid. The capability registry preserves this order, and user models merge once ahead of declared models with duplicate IDs resolved in the user's favor; these overrides never mutate the bundle catalog.

## Configuration schema

`configuration.schema` is an embedded Draft 2020-12 object schema. It must explicitly contain:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {},
  "additionalProperties": false
}
```

Unknown configuration properties fail closed. `configuration.fields` supplies generic labels, descriptions, secret presentation, advanced flags, and ordering. Every presentation hint must reference a schema property. A UI may render only schema vocabulary negotiated as a host feature; unsupported required vocabulary makes the plugin incompatible.

Configuration values are data. They are never shell-expanded and cannot invoke command substitution. Secrets are stored by the execution host and represented by credential references, not returned through configuration snapshots.

## Credentials and OAuth

`credentials` defines stable host-owned credential handles:

- `api-key`, `bearer`, and `oauth2` are secret-bearing kinds; `none` is explicit anonymous access;
- `config_property` identifies the login/configuration input, not a storage path;
- `audience` lists endpoint IDs to which host transport may attach the credential;
- `scopes` are the maximum declared OAuth scope set;
- `legacy_fields` supports transactional migration from older host records.

Plugins never receive raw credentials. Manifests reference host-owned credential handles; the host attaches a credential only after transforms, only to a declared endpoint and allowed origin, and according to the credential kind.

An OAuth flow references declared authorization/token/revocation endpoints and credential IDs. It explicitly declares:

- finite client ID/secret templates;
- authorization and refresh scopes;
- PKCE mode;
- loopback, hosted-paste, or device redirect behavior and state requirement;
- authorization query fields;
- form or JSON token requests, auth style, and separate code/refresh field lists;
- JSON Pointers for access token, refresh token, expiry, and token type;
- whether an omitted rotated refresh token preserves the previous token;
- timeout and maximum response body.

Manifest validation requires explicit OAuth expiry fallback, timeout, body-size, state, PKCE, scope, and refresh-token-preservation policy; authors cannot rely on executor defaults. The executor retains defensive 5-minute and 1 MiB ceilings for programmatically constructed declarations, but checked bundles must state their values. The host owns state/PKCE generation, browser or hosted-paste coordination, token exchange, expiry interpretation, refresh locking, rotation, persistence, revocation attempts, account selection, and redaction. Hosted-paste flows validate state whenever the pasted callback contains it; a provider-displayed bare code remains accepted because it carries no state field.

## Endpoints and headers

Every network operation references an endpoint with:

- an absolute credential-free `base_url`;
- allowed `https` and/or `wss` schemes;
- an exact hostname allowlist;
- override policy: `forbidden`, `same-origin`, or `allowed-hosts`;
- an optional credential handle;
- explicit redirect behavior.

URL userinfo, ambient proxy-based origin substitution, arbitrary schemes, and relative origins are invalid. Path templates are operation-local and begin with `/`. A future endpoint override must satisfy both the declared scheme/host policy and host security policy.

Header rules run in declaration order:

1. common capability rules;
2. operation rules;
3. host credential attachment;
4. host-required protected identity/security rules.

Operations are `set`, `set-if-absent`, `append`, `append-unique`, and `delete`. Every operation except `delete` requires a template. `protected` prevents later user or extension replacement. Manifests cannot protect a header from host deletion or redaction. Hop-by-hop headers, `Host`, content length, proxy authentication, and other host-reserved names are rejected by the future transport compiler even if structurally valid.

## Finite value templates

Templates are non-executable tagged values:

| Kind | Meaning |
| --- | --- |
| `literal` | The exact JSON value in `value`. |
| `config` | A validated provider configuration property named by `ref`. |
| `credential` | An opaque credential handle named by `ref`; only host brokers may resolve it. |
| `context` | An allowlisted host context value, such as OAuth code, model ID, request ID, or registry revision. |
| `concat` | Ordered concatenation of string-producing child templates. |
| `uuid` | Host-generated UUID. |
| `unix-time` | Host clock value when clock use is allowed by the primitive. |
| `random-hex` | Host-generated random bytes encoded as lowercase hex. |

The tag-specific fields are mutually exclusive. Templates have no loops, recursion through references, file reads, environment reads, shell expansion, command substitution, network access, regular expressions, or arbitrary functions. Host context names are a negotiated feature registry; an unknown required context reference is incompatible.

## Structural transformations

### JSON pipelines

JSON transforms operate on parsed JSON with RFC 6901 pointers. Available operations are:

- `set`, `set-if-absent`, and `delete`;
- `copy` and `move` from another pointer;
- `rename-key` within an object;
- `filter-array` using a finite predicate;
- `keep-keys` and `drop-keys` within an object.

Predicates are `exists`, `equals`, `not-equals`, `contains`, `starts-with`, and `matches-enum`. They do not execute regular expressions or scripts. Every pipeline declares `max_operations`; exceeding it is invalid. The host also enforces depth, node-count, string-size, and output-size ceilings.

A failed required transform fails the provider operation. Sending the original untransformed body is not a v1 fallback.

### Prompt pipelines and roles

Prompt operations are `prepend`, `append`, `insert-after-role`, `remove-lines-with-prefix`, `drop-role`, and `join-adjacent-role`. They operate on normalized message blocks, not serialized wire bytes. Conditions use the same finite predicate language.

Role maps explicitly map system, developer, user, assistant, and tool roles. Unknown roles are rejected, dropped, or warned-and-dropped exactly as declared. Protocol compilers may impose stricter public-protocol constraints; manifests cannot weaken them.

### Bidirectional tool codecs

A tool codec contains unique host/provider name pairs, optional per-tool parameter name pairs, and the structural surfaces to which it applies:

- definitions;
- prompt references;
- historical calls;
- historical results;
- normalized stream events.

Both sides must be unique after optional inbound case folding. The host compiles forward and reverse maps and rejects collisions. Rewriting is structural; textual replacement over raw JSON or SSE bytes is forbidden.

## Operations and protocol profiles

An operation separates its logical `kind`, public protocol profile, and wire transport. Every provider manifest declares exactly one `inference` operation; zero or multiple inference operations are invalid, so catalog endpoint and protocol selection never depend on declaration-order fallback:

- protocols: `anthropic-messages`, `openai-responses`, `gemini-generate-content`, `gemini-interactions`, `generic-json`;
- transports: `http-json`, `sse`, `websocket-json`.

The host owns framing, TLS, redirects, cancellation, parsing, backpressure, normalized events, and hard resource ceilings. A manifest selects named transforms, role maps, tool codecs, continuation, and compaction implemented by those bounded host primitives.

For `anthropic-messages`, the optional finite `capabilities.anthropic` policy binds only to that inference protocol. It can resolve a validated client version through environment, bounded HTTPS probe, host cache, and literal fallback; protect ordered identity/beta headers; reuse one process-scoped session identity; bound and transform Messages JSON; construct deterministic billing/session metadata; apply the structural tool codec; and retain bounded state while reversing streamed aliases. Invalid URLs, regular expressions, header names, byte offsets, formats, or protocol bindings fail semantic validation. No manifest code executes.

Omitting an operation-local retry policy means no operation-local replay. Omitting time hints selects host defaults: 30 seconds to connect, 300 seconds per request, and 60 seconds idle while streaming. Hints may lower, never raise, host ceilings.

### Normalized streaming events

The stable host event vocabulary is:

`warning`, text/reasoning start-delta-end, tool-input start-delta-end, `tool-call`, `tool-result`, `source`, `usage`, `finish`, and `error`.

Mappings select source events and extract normalized fields through JSON Pointers. Unknown events are ignored, warned, or rejected as declared. Streaming operations declare an event source, terminal-event requirement, unknown-event policy, and maximum event size. EOF without a required terminal event is an incomplete-stream error; cancellation always wins over retry.

Arbitrary raw-byte rewriting, provider-private executable framing recovery, and unbounded stateful event synthesis are unsupported. A required behavior must be represented by an audited finite host primitive before the provider can be installed.

### Retry and authentication

A retry policy explicitly declares attempts, delay/factor/jitter, `Retry-After` handling, statuses/codes, transport/EOF classes, one-time authentication refresh, and replay safety:

- `idempotent` permits replay only when the host owns idempotency;
- `before-first-event` permits replay only before any observable event;
- `never` forbids replay.

The host applies the lower of manifest requests and host ceilings. Context cancellation/deadline is never retried. SDK retries are disabled when the host retry owner is active. Ambiguous accepted inference, compaction, token rotation, or tool execution is never replayed through another owner.

### Continuation

Continuation modes are `none`, public Responses `previous-response`, and public Gemini `previous-interaction`.

Public continuation declares the response ID pointer/request field, stable request fields, append-only history requirement, storage requirement, full-replay/error fallback, idle lifetime, and opaque metadata namespace. `previous-response` is valid only with OpenAI Responses; `previous-interaction` only with Gemini Interactions. Continuation metadata is versioned and preserved independently of plugin availability.

### Compaction

Compaction modes are `none`, `local-summary`, and `remote-operation`. Declarative remote compaction names another operation, retained token budget, tool-pair preservation, and metadata namespace. Provider-private selection algorithms are unsupported until represented by a finite audited host primitive. A malformed semantic compaction result is not retried unless the declared operation is both replay-safe and retryable.

## Usage, images, controls, metadata, and errors

### Usage

Usage sources are response, stream, or a dedicated operation. Mappings normalize input, output, reasoning, cache-read, cache-write, and total tokens. Window mappings normalize used/limit/remaining/reset values and their reset format. Fallback is explicitly `zero`, `estimate`, or `unavailable`; omission is invalid.

### Images

Image policy explicitly declares accepted media types and source limit, plus optional side, output-byte, patch, output-format, alpha, quality, resize, and aggregate history budgets. No target-specific image fallback exists. Missing optional limits mean that dimension is not constrained by the plugin, but host global limits still apply.

### Native instructions and runtime controls

Instruction profiles map IDs to safe bundle-relative UTF-8 files and name an explicit default. Paths cannot be absolute, cleaned differently, or escape the bundle.

Runtime controls are generic typed values with scope and optional request JSON Pointer. Enum defaults must belong to their values. UI rendering is host-owned and feature-negotiated.

### Opaque metadata

Each metadata contract has a namespace, independent integer version, scope, strict Draft 2020-12 schema, replay requirement, and optional legacy projection. The persistence envelope remains opaque when the plugin is absent. Unknown namespace/version payloads are byte-preserved across database, REST, SSE, server, and client boundaries; interpretation is never required merely to open history.

### Errors

Error mappings classify statuses/codes into authentication, authorization, rate limit, capacity, context overflow, invalid request, content filter, server, transport, or unknown. Mappings are evaluated in declaration order; the first mapping whose non-empty status and code predicates both match wins. Message/code extraction uses JSON Pointers. Plugin text is untrusted, bounded, and redacted before display or persistence.

## Data-only execution boundary

Provider plugins are configuration, not code. The host parses immutable manifest values and declared static UTF-8 files, then executes only audited core transports and transformation primitives. The schema has no executable path, command, arguments, unrestricted process environment, WASM module, RPC hook, subprocess permission, startup policy, restart policy, or native ABI. A narrowly typed policy may name a bounded host input, such as the Anthropic client-version environment override; that does not grant ambient environment access or execution. Executable/runtime fields are unknown and fail strict decoding.

This boundary is deliberate: installation and exact-digest trust protect endpoint, credential, and transformation policy without creating an unsigned-code runtime. If a future provider behavior cannot be represented safely, the core must first gain a finite provider-neutral primitive with explicit validation and limits; bundles cannot escape into arbitrary code.

## Structural and semantic validation

Validation order is:

1. resolve a local directory or HTTPS Git ref to an immutable source snapshot without executing source-controlled code;
2. bounded secure read of exactly one `manifest.json` value;
3. Draft 2020-12 structural validation with unknown ordinary fields rejected;
4. semantic validation of IDs, versions, ranges, uniqueness, references, origin policy, template unions, transform limits, codec bijection, protocol/continuation combinations, metadata schemas, and safe paths;
5. snapshot structure, canonical digest, duplicate provider/plugin, host compatibility, and exact-digest trust validation;
6. atomic install into the canonical direct-child `*.plugin` destination;
7. bounded declarative compilation;
8. atomic registry-generation activation.

`manifest.json` is capped at 1 MiB. Provider presets additionally reject unknown provider types, literal API keys, unsafe endpoints, invalid headers, duplicate or missing models, invalid defaults, and malformed costs or reasoning declarations. An invalid entry does not prevent valid sibling plugins from being diagnosed, but duplicate plugin or provider IDs quarantine every contender. There is no first-wins or last-wins fallback.

Generate schemas with:

```sh
go run main.go schema provider-plugin > provider-plugin.schema.json
go run main.go schema provider-preset-plugin > provider-preset-plugin.schema.json
# or regenerate all checked-in schemas
task schema
```

Canonical examples are schema-validated and semantically decoded by package tests.

## Forward compatibility

- `manifest_version` is a schema major. Unknown majors fail closed.
- V1 fields never change meaning. New V1 fields are optional additions; an older strict host may reject them rather than misinterpret them.
- Plugin SemVer communicates plugin release compatibility but never overrides host API/features.
- Unknown required features are incompatible. Unknown optional features and `x-*` extension payloads are ignored by behavior and preserved where stored.
- Unknown ordinary fields are errors. Extensibility exists only at explicit extension maps and versioned opaque metadata envelopes.
- Unknown metadata namespaces/versions are preserved losslessly but not interpreted.
- Missing, disabled, invalid, incompatible, untrusted, failed, or uninstalled plugins leave provider configuration, account references, model selections, and transcript metadata intact and explicitly unavailable. They never silently select another provider or integrated implementation.

## Canonical examples

- [`minimal.plugin`](examples/minimal.plugin/manifest.json) is a declarative generic JSON provider with no credential.
- [`responses-oauth.plugin`](examples/responses-oauth.plugin/manifest.json) demonstrates OAuth, endpoint policy, header and JSON/prompt transforms, a role map, a bidirectional tool codec, OpenAI Responses SSE events, continuation, retries, usage, images, instructions, controls, errors, and opaque metadata.

Both examples use reserved `.invalid` origins and are intentionally nonfunctional. Target-provider bundles and private endpoint policy are not distributed as authoring examples.
