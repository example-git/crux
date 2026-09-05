# CLI compatibility layer

> **Experimental and not well tested:** The compatibility layer may have bugs or incomplete behavior. Verify it with your own workflows before relying on it.

Crux includes a built-in, unofficial command-line compatibility layer for automation that invokes one of four supported command names. The adapters are part of the normal Crux binary. Installing aliases remains explicit and local to the current user, and normal `crux` invocation is unchanged.

The supported command names and researched contracts are:

| Command | Contract version | Prompt entrypoints | Machine-readable output | Realtime protocol |
|---|---:|---|---|---|
| `codex` | Codex CLI 0.149.1 | positional interactive prompt; `exec [PROMPT]`; stdin | `exec --json` JSONL; structured output; `--output-last-message`; app-server JSON Schema and TypeScript generation | `app-server --stdio` or `--listen stdio://` |
| `claude` | Claude Code 2.1.245 | positional interactive prompt; `-p`; stdin | `json`; `stream-json`; structured output | multi-turn stream JSON on stdin/stdout or `--sdk-url` WebSocket |
| `agy` | Antigravity CLI 1.1.20 | `-i`; `-p` | `json`; `stream-json` | multi-turn stream JSON on stdin/stdout |
| `copilot` | GitHub Copilot CLI 1.0.80 | `-i`; `-p`; piped stdin | `--output-format json` JSONL | ACP v1 and SDK JSON-RPC v3 over stdio |

`agy` is the canonical Antigravity command. The installer does not create `gemini` or `antigravity` aliases.

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

Every researched root flag is accepted with its researched arity. Enumerated and structured values that map to real native behavior (such as Codex `--sandbox`/`--ask-for-approval`, Claude `--permission-mode`/`--input-format`/`--output-format`, Antigravity `--mode`/`--input-format`/`--output-format`, and Copilot `--mode`/`--output-format`) are still validated, so invalid explicit values, conflicting options, missing values, and unknown flags fail rather than silently selecting a fallback. Flags documented below as compatibility no-ops accept any explicit value without validation, since the value never reaches native execution. Parser errors use the target's exit-code and stdout/stderr family where implemented. Unsupported administrative subcommands remain explicit errors because accepting a flag does not emulate vendor account, daemon, installation, cloud, or mutation commands.

### Accepted no-op flags

The following accepted flags are compatibility no-ops. Their arguments are consumed but not validated, and they do not change native Crux execution.

- **Codex:** `-c`/`--config`, `--enable`, `--disable`, `--remote`, `--remote-auth-token-env`, `--strict-config`, `--oss`, `--local-provider`, `-p`/`--profile`, `--approve-for-me`, `--dangerously-bypass-hook-trust`, `--search`, `--thread-source`, `--ignore-user-config`, `--ignore-rules`, `--color`, `--skip-git-repo-check`, `--no-alt-screen`, `--add-dir`, `-i`/`--image`, `--ephemeral`, `--uncommitted`, `--base`, `--commit`, `--title`, `--all`, `--include-non-interactive`, and the three researched `--codex-run-as-*` helper switches. A model supplied to interactive mode is also accepted without changing the interactive model. `app-server` additionally accepts `-c`/`--config`, `--enable`, `--disable`, `--strict-config`, `--code-mode-host`, `--analytics-default-enabled`, `--ws-auth`, `--ws-token-file`, `--ws-token-sha256`, `--ws-shared-secret-file`, `--ws-issuer`, `--ws-audience`, and `--ws-max-clock-skew-seconds` as no-ops; `daemon` and `proxy` remain explicit errors as unsupported daemon-management subcommands.
- **Claude:** `--agents`, `--autocompact`, `--max-thinking-tokens`, `--agent`, `--effort`, `--add-dir`, `--session-id`, `--no-session-persistence`, `--allowedTools`/`--allowed-tools`, `--max-turns`, `--max-budget-usd`, `--include-hook-events`, `--file`, `--from-pr`, `-n`/`--name`, `-w`/`--worktree`, `--tmux`, `--betas`, `--tools`, `--permission-prompt-tool`, `--allow-dangerously-skip-permissions`, `--exclude-dynamic-system-prompt-sections`, `--forward-subagent-text`, `--prompt-suggestions`, `--settings`, `--setting-sources`, `--bare`, `--safe-mode`, `--disable-slash-commands`, `--plugin-dir`, `--plugin-url`, `--mcp-config`, `--strict-mcp-config`, `--ide`, `--chrome`, `--no-chrome`, `--cloud`, `--environment`, `--teleport`, `--remote-control`/`--rc`, `--remote-control-session-name-prefix`, `-d`/`--debug`, `--debug-file`, `--brief`, `--ax-screen-reader`, `--init`, `--init-only`, `--maintenance`, `-d2e`/`--debug-to-stderr`, `--session-mirror`, `--enable-auth-status`, `--task-budget`, `--workload`, `--managed-settings`, `--handle-uri`, `--thinking`, `--thinking-display`, `--append-subagent-system-prompt`, `--plan-mode-instructions`, `--resume-session-at`, `--resume-drops-turn`, `--reply-on-resume`, `--rewind-files`, `--watch-artifact`, `--watch-artifact-no-autoreact`, `--prefill`, `--prefill-b64`, `--deep-link-origin`, `--deep-link-repo`, `--deep-link-last-fetch`, `--deep-link-cwd-b64`, `--plugin-dir-no-mcp`, `--advisor`, `--enable-auto-mode`, `--messaging-socket-path`, `--channels`, `--dangerously-load-development-channels`, `--no-home-settings`, `--remote`, `--pool`, `--correlation-id`, `--ref`, `--on-branch`, `--agent-id`, `--agent-name`, `--team-name`, `--agent-color`, `--plan-mode-required`, `--parent-session-id`, `--teammate-mode`, and `--agent-type`. System-prompt options are no-ops only in interactive mode; headless mode translates them as prompt context.
- **Antigravity:** `--agent`, `--effort`, `--add-dir`, `--project`, `--new-project`, `--sandbox`, and `--disable-slash-commands`. A model supplied to interactive mode is accepted without changing the interactive model. `--remote-control` and `--bg-updater` select unavailable server/updater modes and therefore fail explicitly.
- **Copilot:** `--connect`, `--context`, `-n`/`--name`, `--session-id`, `--enable-memory`, `--share`, `--share-gist`, `--agent`, `--effort`/`--reasoning-effort`, `--mode`, `--plan`, `--autopilot`, `--max-autopilot-continues`, `--max-ai-credits`, `--enable-reasoning-summaries`, `--add-dir`, `--plugin-dir`, `--no-custom-instructions`, `--disallow-temp-dir`, `--no-ask-user`, `--attachment`, `-s`/`--silent`, `--allow-all-paths`, `--allow-all-urls`, `--allow-tool`, `--allow-url`, `--available-tools`, `--excluded-tools`, `--secret-env-vars`, `--additional-mcp-config`, `--disable-builtin-mcps`, `--disable-mcp-server`, `--enable-mcp-server`, `--allow-all-mcp-server-instructions`, `--add-github-mcp-tool`, `--add-github-mcp-toolset`, `--enable-all-github-mcp-tools`, `--banner`, `--no-banner`, `--bash-env`, `--no-bash-env`, `--mouse`, `--no-mouse`, `--screen-reader`, `--plain-diff`, `--no-color`, `--experimental`, `--no-experimental`, `--extension-sdk-path`, `--log-dir`, `--log-level`, `--no-auto-update`, `--config-dir`, `--sandbox`, `--no-sandbox`, `-w`/`--worktree`, `--remote`, `--no-remote`, `--remote-export`, `--no-remote-export`, `--with-token`, `--binary-version`, `--prefer-version`, `--print-debug-info`, `--session-idle-timeout`, `--dynamic-retrieval`, `--cloud`, `--managed-server`, `--auth-token-env`, `--no-auto-login`, and `--host`. `--stream` still controls emitted JSONL deltas.

## Known differences

- Sessions are Crux sessions. Vendor session identifiers, account state, billing, quotas, telemetry, and cloud state are neither read nor fabricated.
- Additional directories, attachments/images, agent and effort overrides, non-persistent sessions, turn limits, budget limits, and target project/context selectors are accepted no-ops. Claude fork requests are native persisted session forks when a source session is supplied. Structured-output schemas are enforced against the final native response and reported in the target result envelope.
- Headless model selection maps to native Crux. Interactive model overrides are accepted no-ops.
- Headless system-prompt options are translated into delimited prompt context. They are not sent as vendor-owned system messages.
- Permission mappings are conservative. Explicit bypass maps only to native Crux bypass; bounded, plan, deny, or tool-restriction requests run fail-closed when their finer target semantics cannot be preserved.
- Machine-readable envelopes and event streams follow the target-facing shape but describe a Crux-native run. Claude and Antigravity expose the bound native Crux session UUID. Copilot preserves caller-generated SDK IDs through an atomic external-to-native binding stored in the workspace data directory. Usage and cost fields are derived from native session deltas.
- Claude, Antigravity, Copilot ACP, and Codex app-server protocol input is newline-delimited JSON with a 10 MiB maximum per message. Copilot SDK stdio uses `Content-Length` JSON-RPC framing, matching the installed 1.0.80 client, with the same 10 MiB payload bound. Codex app-server supports initialization, account/model discovery, thread start/list/read/resume/fork, turn start, and turn interrupt over stdio. `generate-json-schema`, `generate-internal-json-schema`, and `generate-ts` write schemas for that implemented subset and include a `crux_compatibility` scope manifest; they do not claim the unimplemented portions of the official protocol. Copilot supports native-backed ACP v1 session creation/loading, validated model configuration, prompt usage updates, cancellation, and close. SDK JSON-RPC v3 supports standard-client connect/status/model discovery, create/resume/list/latest/metadata/delete, send/abort, lifecycle events, and bounded event-log reads over framed stdio. TCP listeners are not started.
- Claude stream input pins one native session across user turns, resume/fork, replay and duplicate UUID behavior, priorities, full text partial-message lifecycles, validated model switching, native cancellation, usage, common verified SDK controls, and structured results. `--sdk-url` connects the same protocol over WebSocket. Antigravity stream input pins the resolved native conversation, applies the five-minute default or explicit timeout per turn, reports native usage, and emits structured results; it warns on unknown events and rejects unsupported control or slash-command events.
- Copilot headless prompt mode requires `--allow-all-tools` or `COPILOT_ALLOW_ALL`, matching the researched CLI contract. SDK compatibility implements the core v3 lifecycle and returns method-not-found for unimplemented administrative methods; UI-server behavior and TCP listeners remain unavailable.

## Local installation

Build Crux and the local alias manager from the same checkout:

```sh
go build -o "$HOME/.local/bin/crux" .
go build -o "$HOME/.local/bin/crux-compat" ./cmd/crux-compat
```

Install the four managed hard links and prepend their private directory through the detected shell profile:

```sh
"$HOME/.local/bin/crux-compat" install \
  --executable "$HOME/.local/bin/crux"
```

The default managed root is the private `compatibility` directory next to Crux's global data file. Use `--root`, `--shell`, or `--profile` to select explicit local paths. `--skip-path` creates the links without changing a shell profile. The selected Crux executable and managed root must be on a filesystem that supports hard links between them.

Installation never replaces an existing unrecognized `codex`, `claude`, `agy`, or `copilot` file in the managed directory. It also refuses symlinked, unexpectedly populated, or group/world-accessible managed directories. On Windows, private directories use protected ACLs granting access only to the current user, SYSTEM, and Administrators; existing directories are checked against the same access policy. Existing official tools elsewhere on PATH are not modified.

Start a new shell after installation, then check both configured and active PATH state:

```sh
crux-compat status
command -v codex claude agy copilot
```

`path setup: true` means the managed profile line and private PATH file are present. `path active: true` means the current process already has the private bin directory in PATH.

## Enable, disable, and repair

Aliases are enabled after installation:

```sh
crux-compat enable
crux-compat disable
```

When enabled, the managed names dispatch to Crux compatibility adapters. When disabled, the hard links remain first in PATH but forward the complete process invocation to the first original executable with the same name later in PATH. Forwarding preserves arguments, environment, working directory, stdin, stdout, stderr, and exit status. It skips other hard links to the managed Crux inode to avoid recursion. If no original executable exists later in PATH, the command fails clearly with exit status 127.

Mode changes are idempotent and refuse an unhealthy installation. Inspect and repair missing or stale links before retrying:

```sh
crux-compat status
crux-compat repair
```

Repeat `--root` for a custom managed root. If installation used `--skip-path`, pass it to repair to preserve that choice. Repair validates the replacement PATH file and profile update before removing the old setup. It replaces only links identified by the saved managed fingerprint, refuses collisions or changed files, and restores the prior aliases, PATH files, profiles, and persisted state if any repair step fails.

## Removal

Remove managed aliases, the installer-owned PATH line and PATH file, and compatibility state with:

```sh
crux-compat uninstall
```

Uninstall preflights every existing alias and the managed PATH setup before deleting anything. It refuses to remove changed or unrecognized files; a collision at any alias leaves every recognized alias and persisted state in place. If a removal step fails after preflight, the transaction restores the aliases, profile, PATH file, and state. Uninstall preserves unrelated shell-profile content and does not remove official tools elsewhere on PATH. After uninstall, start a new shell or remove the old private bin entry from the current shell's PATH. The separately built `crux-compat` manager binary can then be deleted if it is no longer needed.

## Independence and non-affiliation

This layer provides local process-contract compatibility and does not claim official tool identity. Crux is independent and is not affiliated with, sponsored by, endorsed by, maintained by, or provided by OpenAI, Anthropic, Google, or GitHub. Third-party names are used only to identify the command contracts expected by local automation.

The compatibility layer does not bundle official binaries, logos, OAuth identities, credentials, update services, or private vendor protocols. Authentication and model access remain owned by the providers configured in Crux. Review applicable licenses, terms, and trademark requirements before distributing an alias installation or compatibility build to anyone else.
