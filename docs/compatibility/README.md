# CLI compatibility layer

> **Experimental and not well tested:** The compatibility layer may have bugs or incomplete behavior. Verify it with your own workflows before relying on it.

Crux includes a built-in, unofficial command-line compatibility layer for automation that invokes one of four supported command names. The adapters are part of the normal Crux binary. Installing aliases remains explicit and local to the current user, and normal `crux` invocation is unchanged.

The supported command names and researched contracts are:

| Command | Contract version | Prompt entrypoints | Machine-readable output | Realtime protocol |
|---|---:|---|---|---|
| `codex` | Codex CLI 0.153.4 | positional interactive prompt; `exec [PROMPT]`; stdin | `exec --json` JSONL; structured output; `--output-last-message`; app-server JSON Schema and TypeScript generation | `app-server --stdio` or `--listen stdio://` |
| `claude` | Claude Code 2.1.245 | positional interactive prompt; `-p`; stdin | `json`; `stream-json`; structured output | multi-turn stream JSON on stdin/stdout or `--sdk-url` WebSocket |
| `agy` | Antigravity CLI 1.1.20 | `-i`; `-p` | `json`; `stream-json` | multi-turn stream JSON on stdin/stdout |
| `copilot` | GitHub Copilot CLI 1.0.80 | `-i`; `-p`; piped stdin | `--output-format json` JSONL | ACP v1 and SDK JSON-RPC v3 over stdio |

`agy` is the canonical Antigravity command. The installer does not create `gemini` or `antigravity` aliases.

CLI version output uses the same native provider-version resolvers as Crux, including their release endpoints, environment overrides, caches, and offline fallback policies. Claude's version requires its active, trusted provider plugin's client-identity declaration. Installation and repair verify the executable against the resolved native Codex version, not a separately pinned installer version. The contract versions above and generated schema contract versions remain researched protocol baselines; dynamic CLI versions do not imply support for newly released protocol features.

## Compatibility matrix

The following matrix describes behavior translated into native Crux execution.

| Area | `codex` | `claude` | `agy` | `copilot` |
|---|---|---|---|---|
| Help/version | `-h`, `--help`, `-V`, `--version` | `-h`, `--help`, `-v`, `--version` | `-h`, `--help`, `-v`, `--version` | `-h`, `--help`, `-v`, `--version`, `version` |
| Headless prompt | `exec`, `e`, prompt or stdin | `-p`, prompt or stdin | `-p`, `--print`, `--prompt` | `-p`, `--prompt` |
| Interactive prompt | positional prompt | positional prompt | `-i`, `--prompt-interactive` | `-i`, `--interactive` |
| Model | `-m`, `--model` | `--model`, `--fallback-model` | `--model` | `--model` |
| Working directory | `-C`, `--cd` | process directory | process directory | `-C` |
| Sessions | `resume`, `exec resume`, `--last` | `-c`, `--continue`, `-r`, `--resume` | `-c`, `--continue`, `--conversation` | `--continue`, `-r`, `--resume` |
| Permissions | sandbox/approval mapping and explicit bypass | permission mode, deny controls, and explicit bypass | mode and explicit bypass | mode, deny controls, and explicit bypass |
| Output | text, `--json`, structured schema output, `--output-last-message` | text, JSON, stream JSON, structured output, partial/replay controls | text, JSON, stream JSON, structured output, timeout | text, expanded JSONL lifecycle events, stream control |
| Realtime | app-server stdio | multi-turn stream JSON and SDK WebSocket | multi-turn stream JSON | ACP v1 and SDK JSON-RPC v3 stdio |

Compatibility CLI parsing accepts unknown flags and ignores options without a native implementation instead of aborting an otherwise valid invocation. This includes Codex app-server and schema-generation options. Ignored options do not emit warnings or change native execution. Enumerated and structured values that select implemented behavior (such as sandbox/approval policy, input/output formats, and the SkySight Memgen contract) still require valid values and compatible combinations. Native permission enforcement and execution failures remain visible. Parser errors use the target's exit-code and stdout/stderr family where implemented. Unsupported administrative subcommands remain explicit errors; accepting their flags does not implement the operation.

Unknown `--flag=value` arguments are ignored as a single token. Codex and Claude preserve following positional text as prompt or session arguments rather than guessing whether an unknown flag takes a value; use the inline form for unknown value-bearing flags. Antigravity, Copilot, and Codex's app-server/schema parsers consume at most one following non-option argument for an unknown flag. Known no-op flags retain their value or variadic consumption, but tolerate absent values and never consume a following option as their value.

### Accepted no-op flags

The following accepted flags are compatibility no-ops. Their arguments are consumed but not validated, and they do not change native Crux execution.

- **Codex:** `--enable`, `--disable`, `--remote`, `--remote-auth-token-env`, `--strict-config`, `--oss`, `--local-provider`, `-p`/`--profile`, `--approve-for-me`, `--dangerously-bypass-hook-trust`, `--search`, `--color`, `--skip-git-repo-check`, `--no-alt-screen`, `--uncommitted`, `--base`, `--commit`, `--title`, `--all`, `--include-non-interactive`, and the three researched `--codex-run-as-*` helper switches remain no-ops. `--thread-source`, `--ignore-user-config`, and `--ignore-rules` are retained as typed invocation metadata but do not replace Crux configuration or permission ownership. `-i`/`--image`, `--ephemeral`, model selection, sandbox/approval policy, structured output, final-message output, and the documented SkySight `--config` bundle have native effects. Unknown `--config` keys and `--add-dir` are ignored. Invalid or partial SkySight Memgen contracts still fail explicitly. `app-server` additionally accepts `-c`/`--config`, `--enable`, `--disable`, `--strict-config`, `--code-mode-host`, `--analytics-default-enabled`, `--ws-auth`, `--ws-token-file`, `--ws-token-sha256`, `--ws-shared-secret-file`, `--ws-issuer`, `--ws-audience`, and `--ws-max-clock-skew-seconds` as no-ops, including leading `--config` overrides before `app-server`; `daemon` and `proxy` remain explicit errors as unsupported daemon-management subcommands.
- **Claude:** `--agents`, `--autocompact`, `--max-thinking-tokens`, `--agent`, `--effort`, `--add-dir`, `--session-id`, `--no-session-persistence`, `--allowedTools`/`--allowed-tools`, `--max-turns`, `--max-budget-usd`, `--include-hook-events`, `--file`, `--from-pr`, `-n`/`--name`, `-w`/`--worktree`, `--tmux`, `--betas`, `--tools`, `--permission-prompt-tool`, `--allow-dangerously-skip-permissions`, `--exclude-dynamic-system-prompt-sections`, `--forward-subagent-text`, `--prompt-suggestions`, `--settings`, `--setting-sources`, `--bare`, `--safe-mode`, `--disable-slash-commands`, `--plugin-dir`, `--plugin-url`, `--mcp-config`, `--strict-mcp-config`, `--ide`, `--chrome`, `--no-chrome`, `--cloud`, `--environment`, `--teleport`, `--remote-control`/`--rc`, `--remote-control-session-name-prefix`, `-d`/`--debug`, `--debug-file`, `--brief`, `--ax-screen-reader`, `--init`, `--init-only`, `--maintenance`, `-d2e`/`--debug-to-stderr`, `--session-mirror`, `--enable-auth-status`, `--task-budget`, `--workload`, `--managed-settings`, `--handle-uri`, `--thinking`, `--thinking-display`, `--append-subagent-system-prompt`, `--plan-mode-instructions`, `--resume-session-at`, `--resume-drops-turn`, `--reply-on-resume`, `--rewind-files`, `--watch-artifact`, `--watch-artifact-no-autoreact`, `--prefill`, `--prefill-b64`, `--deep-link-origin`, `--deep-link-repo`, `--deep-link-last-fetch`, `--deep-link-cwd-b64`, `--plugin-dir-no-mcp`, `--advisor`, `--enable-auto-mode`, `--messaging-socket-path`, `--channels`, `--dangerously-load-development-channels`, `--no-home-settings`, `--remote`, `--pool`, `--correlation-id`, `--ref`, `--on-branch`, `--agent-id`, `--agent-name`, `--team-name`, `--agent-color`, `--plan-mode-required`, `--parent-session-id`, `--teammate-mode`, and `--agent-type`. System-prompt options are no-ops only in interactive mode; headless mode translates them as prompt context.
- **Antigravity:** `--agent`, `--effort`, `--add-dir`, `--project`, `--new-project`, `--sandbox`, and `--disable-slash-commands`. A model supplied to interactive mode is accepted without changing the interactive model. `--remote-control` and `--bg-updater` are ignored; they do not start server/updater modes.
- **Copilot:** `--connect`, `--context`, `-n`/`--name`, `--session-id`, `--enable-memory`, `--share`, `--share-gist`, `--agent`, `--effort`/`--reasoning-effort`, `--mode`, `--plan`, `--autopilot`, `--max-autopilot-continues`, `--max-ai-credits`, `--enable-reasoning-summaries`, `--add-dir`, `--plugin-dir`, `--no-custom-instructions`, `--disallow-temp-dir`, `--no-ask-user`, `--attachment`, `-s`/`--silent`, `--allow-all-paths`, `--allow-all-urls`, `--allow-tool`, `--allow-url`, `--available-tools`, `--excluded-tools`, `--secret-env-vars`, `--additional-mcp-config`, `--disable-builtin-mcps`, `--disable-mcp-server`, `--enable-mcp-server`, `--allow-all-mcp-server-instructions`, `--add-github-mcp-tool`, `--add-github-mcp-toolset`, `--enable-all-github-mcp-tools`, `--banner`, `--no-banner`, `--bash-env`, `--no-bash-env`, `--mouse`, `--no-mouse`, `--screen-reader`, `--plain-diff`, `--no-color`, `--experimental`, `--no-experimental`, `--extension-sdk-path`, `--log-dir`, `--log-level`, `--no-auto-update`, `--config-dir`, `--sandbox`, `--no-sandbox`, `-w`/`--worktree`, `--remote`, `--no-remote`, `--remote-export`, `--no-remote-export`, `--with-token`, `--binary-version`, `--prefer-version`, `--print-debug-info`, `--session-idle-timeout`, `--dynamic-retrieval`, `--cloud`, `--managed-server`, `--auth-token-env`, `--no-auto-login`, `--host`, and `--port`. Ignoring `--port` does not open a TCP listener; SDK server execution still requires `--stdio`. `--stream` still controls emitted JSONL deltas.

## Known differences

- Sessions are Crux sessions. Codex app-server always displays a synthetic `apiKey` account with `requiresOpenaiAuth:false`, independently of native provider availability or authentication. This display-only account does not read, refresh, export, create, or replace native credentials. Native provider selection, authentication, permissions, and execution remain authoritative, and their failures remain visible. Vendor billing, quotas, telemetry, and cloud state are neither read nor fabricated.
- Additional-directory flags are accepted no-ops in all four adapters; they do not expand native filesystem permissions. Codex attachments/images and analysis-effort overrides reach the native turn; agent overrides, non-persistent session flags outside Codex, turn limits, budget limits, and target project/context selectors remain no-ops. Claude fork requests are native persisted session forks when a source session is supplied. Structured-output schemas are enforced against the final native response and reported in the target result envelope.
- Headless model selection maps to native Crux. Interactive model overrides are accepted no-ops. Codex app-server advertises every retained native provider/model pair as an uppercase `PROVIDER-MODELNAME` alias in `id`, `model`, and `displayName`. Punctuation is normalized; collisions receive a deterministic hash suffix, and assigned aliases remain stable for the workspace lifetime. Unavailable providers remain listed but fail visibly when selected. Thread start/resume and turn selection resolve advertised aliases to the exact native provider and raw model; conflicting explicit providers fail rather than redirect execution. Native configuration and persisted model selections retain their original provider/model identifiers.
- Codex headless developer instructions and output-schema instructions are carried as isolated native turn instructions, not concatenated into the user prompt. Other headless system-prompt adapters retain their documented target-specific translation.
- Permission mappings are conservative. Explicit bypass maps only to native Crux bypass; bounded, plan, deny, or tool-restriction requests run fail-closed when their finer target semantics cannot be preserved.
- Machine-readable envelopes and event streams follow the target-facing shape but describe a Crux-native run. Claude and Antigravity expose the bound native Crux session UUID. Copilot preserves caller-generated SDK IDs through an atomic external-to-native binding stored in the workspace data directory. Usage and cost fields are derived from native session deltas.
- Claude, Antigravity, Copilot ACP, and Codex app-server protocol input is newline-delimited JSON with a 10 MiB maximum per message. Copilot SDK stdio uses `Content-Length` JSON-RPC framing, matching the installed 1.0.80 client, with the same 10 MiB payload bound. Codex app-server supports initialization, synthetic account display, native configuration/model discovery, thread start/list/read/loaded-list/resume/fork/unsubscribe/archive/unarchive/delete/rollback, persisted archive and archived listing, idle unload with persisted-session resume, turn start/steer/interrupt, native `thread/name/set` title persistence, loaded-thread native compaction, experimental native background-terminal list/terminate/clean, and native lifecycle notifications over stdio. Steering applies text input to the exact active native turn. Archive translates to native persisted state for the thread and its native parent-session descendants, excludes forks from that lineage, and emits filtered `thread/archived` notifications after the response; transactional descendant failure, atomic writer admission, ephemeral archive, and archived read/turn access remain incomplete. Unarchive clears native persisted archive state for exactly the requested thread, returns a cold `notLoaded` projection before a filtered `thread/unarchived` notification, rejects active writers, and leaves archived descendants and independent forks unchanged; atomic writer admission, archived-related lineage enrichment, and live Desktop acceptance remain open. Delete transactionally removes persisted active or archived native parent-session subtrees in descendant-first order, excludes independent forks, rejects observed active writers before mutation, removes loaded bindings, and emits filtered `thread/deleted` notifications after the response. Native copied forks survive source deletion and project null source lineage; deletion writer preflight is observational rather than an atomic admission lease. Rollback transactionally removes the selected effective user turn and all later native history, persists the pruned state for future reads and resumes, and removes affected checkpoints without restoring workspace files. It rejects active or duplicate mutations; paginated thread history, the parent-owned Multi-Agent V2 restriction, and live Desktop acceptance remain open. Compaction uses native session summarization for loaded threads and emits exact two-field `contextCompaction` items; response-after-native-admission parity, per-thread scheduling, the parent-owned MAv2 restriction, restart behavior, and live Desktop acceptance remain open. Explicit `thread/shellCommand` requests use native workspace shell execution, load cold threads, attach to an active turn or create a standalone turn, preserve exact timeout validation, and exclude shell-only messages from projected history. Shell output is currently buffered into one delta; response-after-native-admission parity, cross-thread scheduling, partial timeout output, structured command actions, restart behavior, and live Desktop acceptance remain open. Plugin listing remains an explicit unsupported-method error. `generate-json-schema`, `generate-internal-json-schema`, and `generate-ts` write schemas for the implemented subset and include a `crux_compatibility` scope manifest; they do not claim the unimplemented portions of the official protocol. Copilot supports native-backed ACP v1 session creation/loading, validated model configuration, prompt usage updates, cancellation, and close. SDK JSON-RPC v3 supports standard-client connect/status/model discovery, create/resume/list/latest/metadata/delete, send/abort, lifecycle events, and bounded event-log reads over framed stdio. TCP listeners are not started.
- Claude stream input pins one native session across user turns, resume/fork, replay and duplicate UUID behavior, priorities, full text partial-message lifecycles, validated model switching, native cancellation, usage, common verified SDK controls, and structured results. `--sdk-url` connects the same protocol over WebSocket. Antigravity stream input pins the resolved native conversation, applies the five-minute default or explicit timeout per turn, reports native usage, and emits structured results; it warns on unknown events and rejects unsupported control or slash-command events.
- Copilot headless prompt mode requires `--allow-all-tools` or `COPILOT_ALLOW_ALL`, matching the researched CLI contract. SDK compatibility implements the core v3 lifecycle and returns method-not-found for unimplemented administrative methods; UI-server behavior and TCP listeners remain unavailable.

### Codex Desktop and SkySight

Desktop replacement requires more than the CLI subset above. An authorized SkySight-style `codex exec --json --ephemeral` invocation using the Memgen configuration below completed through native Crux and returned the requested model response. A rebuilt alias also passed isolated app-server checks for native thread-title persistence, response-before-notification ordering, notification filtering, and visible unsupported-method errors.

SkySight Computer History preparation supports `initialize`, `initialized`, legacy `getAuthStatus`, and `config/read` over app-server stdio. `getAuthStatus` always returns `authMethod:"apikey"`, `authToken:null`, and `requiresOpenaiAuth:false`. Its optional nullable `includeToken` and `refreshToken` flags retain boolean validation but do not export or refresh credentials. `account/read` always returns the synthetic `apiKey` account; `account/login/start` returns `type:"apiKey"`, and `account/logout` returns an empty result without changing that account or native authentication. These account operations work without starting the native workspace backend.

`config/read` with `includeLayers:false` returns the accepted native configuration projection and origins. Without `cwd`, project and workspace layers are excluded; an explicit `cwd` selects that workspace's view. The public `model` is the routing alias and `model_provider` is `crux`; `model_providers.crux` describes that adapter namespace. Native provider/model identifiers and account ownership remain unchanged behind it. Optional `memories.consolidation_model` and explicit native runtime model pins retain their native precedence. Reads use retained accepted load evidence without rereading files, reevaluating shell config, or rescanning providers. Origins name actual native sources; null layers that preserve an inherited value preserve its origin too. Each `version` is a SHA-256 fingerprint of that source's supported-field projection, not an upstream writable revision or a hash of secret-bearing configuration. `layers` is omitted; `includeLayers:true` is accepted and ignored. Other unused RPC parameters are accepted without changing the implemented native behavior. Generated JSON Schema and TypeScript describe the implemented response subset.

Initialized stdio tests cover account independence from backend startup and native credential state, configuration provenance, and complete qualified model discovery. An isolated rebuilt executable passed the always-on account sequence, exact native endpoint/raw-model routing through thread start, resume and turn overrides, conflicting-provider rejection, and visible native authentication failure while the synthetic account remained logged in. Resume validation also covers native user-message completion markers without treating them as assistant completion. These checks do not establish fresh SkySight summary generation or installed Desktop acceptance. No deployed binary, installed provider bundle, running helper, signing state, prompt, or execution override was changed.

The supported SkySight Memgen override is an atomic bundle. Pass every entry with a separate `-c` or `--config`; an incomplete bundle or a different value fails explicitly. Additional unknown override keys are ignored. Replace the example developer instructions with the instructions for the invocation.

```text
model_reasoning_effort="medium"
developer_instructions="memgen instructions"
model_provider="openai-memgen"
model_providers.openai-memgen.name="OpenAI"
model_providers.openai-memgen.requires_openai_auth=true
model_providers.openai-memgen.supports_websockets=true
model_providers.openai-memgen.http_headers={ "X-OpenAI-Memgen-Request" = "true" }
features.memories=false
features.apps=false
features.plugins=false
features.multi_agent=false
features.tool_search=false
features.tool_suggest=false
web_search="disabled"
mcp_servers={}
plugins={}
apps._default.enabled=false
analytics.enabled=false
otel.exporter="none"
otel.trace_exporter="none"
otel.metrics_exporter="none"
project_doc_max_bytes=0
skills.bundled.enabled=false
skills.config=[]
```

Installed Desktop application acceptance remains unverified. The active signed Desktop and helper processes were not replaced, attached to, or disrupted, and the repository has no non-disruptive installed-Desktop harness. Parser, schema, bridge, installed-call inspection, and SkySight helper success do not establish full installed Desktop acceptance.

Codex `--ephemeral` requests a temporary native session that is deleted on normal close; it is not a no-op. This does not establish never-persisted, crash-safe, or independently database-verified cleanup behavior.

Windows-only compatibility shims, including Windows-specific sandbox adaptation, are outside the currently validated scope. Shared native permission and sandbox enforcement remains unchanged; no foreign-platform compatibility claim is inferred from macOS validation.

## Local installation

Alias management is built into the main Crux executable; no separate manager needs to be compiled or installed. Install one provider alias or all four:

```sh
crux alias install codex
crux alias install --all
```

Choose exactly one provider name or `--all`. Supported names are `codex`, `claude`, `antigravity`, and `copilot`; `agy` also selects Antigravity, while native provider IDs `claude-ai` and `gemini-ag` select Claude and Antigravity. Antigravity's installed command is always `agy`. Unsupported names and conflicting or missing selectors fail without installing anything.

Installation hard-links the running Crux executable and prepends the private alias directory through the detected shell profile. Installing another provider adds that alias without removing the existing selection. Repeating an installation is idempotent.

The default managed root is the private `compatibility` directory next to Crux's global data file. Use `--root`, `--shell`, or `--profile` to select explicit local paths. `--skip-path` creates the links without changing a shell profile. The running Crux executable and managed root must be on a filesystem that supports hard links between them.

Installation never replaces an existing unrecognized `codex`, `claude`, `agy`, or `copilot` file in the managed directory. It also refuses symlinked, unexpectedly populated, or group/world-accessible managed directories. On Windows, private directories use protected ACLs granting access only to the current user, SYSTEM, and Administrators; existing directories are checked against the same access policy. Existing official tools elsewhere on PATH are not modified.

Start a new shell after installation, then check both configured and active PATH state:

```sh
crux alias status
command -v codex claude agy copilot
```

`installed: true` means the saved alias selection is healthy; aliases you did not select can still be shown as `missing`. `path setup: true` means the managed profile line and private PATH file are present. `path active: true` means the current process already has the private bin directory in PATH.

## Enable, disable, and repair

A new installation is enabled by default. Enable and disable apply to all currently installed aliases; adding an alias preserves the installation's existing mode:

```sh
crux alias enable
crux alias disable
```

When enabled, the managed names dispatch to Crux compatibility adapters. When disabled, the hard links remain first in PATH but forward the complete process invocation to the first original executable with the same name later in PATH. Forwarding preserves arguments, environment, working directory, stdin, stdout, stderr, and exit status. It skips other hard links to the managed Crux inode to avoid recursion. If no original executable exists later in PATH, the command fails clearly with exit status 127.

Mode changes are idempotent and refuse an unhealthy installation. Inspect and repair missing or stale links before retrying:

```sh
crux alias status
crux alias repair
```

Repeat `--root` for a custom managed root. If installation used `--skip-path`, repeat it when adding aliases or repairing to preserve that choice. Repair uses the running Crux executable and only repairs the saved alias selection; it does not install unselected aliases. An absent installation must first be created with `install`. Existing installations made by the old separate manager remain readable. Repair validates the replacement PATH file and profile update before removing the old setup. It replaces only links identified by the saved managed fingerprint, refuses collisions or changed files, and restores the prior aliases, PATH files, profiles, and persisted state if any repair step fails.

## Removal

Remove one provider alias or the entire installation:

```sh
crux alias uninstall codex
crux alias uninstall --all
```

Removing one alias preserves the others and their shared PATH setup. Removing the last alias also removes the installer-owned PATH line, PATH file, and saved state. Repeating removal of an absent alias is harmless.

Uninstall preflights every selected alias before deleting anything, and preflights PATH cleanup when removing the final selection. It refuses to remove changed or unrecognized selected files. If a removal step fails after preflight, the transaction restores the affected aliases, profile, PATH file, and state. Uninstall preserves unrelated shell-profile content and does not remove official tools elsewhere on PATH. After complete removal, start a new shell or remove the old private bin entry from the current shell's PATH.

## Independence and non-affiliation

This layer provides local process-contract compatibility and does not claim official tool identity. Crux is independent and is not affiliated with, sponsored by, endorsed by, maintained by, or provided by OpenAI, Anthropic, Google, or GitHub. Third-party names are used only to identify the command contracts expected by local automation.

The compatibility layer does not bundle official binaries, logos, OAuth identities, credentials, update services, or private vendor protocols. Authentication and model access remain owned by the providers configured in Crux. Review applicable licenses, terms, and trademark requirements before distributing an alias installation or compatibility build to anyone else.
