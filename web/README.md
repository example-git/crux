# Crux web and desktop client

A SolidJS client for the native Crux execution host. The browser and Electron
renderer use the same HTTP and WebSocket APIs. `crux web` owns one local host
process, serves the compiled client, and runs workspaces through the existing
Crux backend.

## Run

From the repository root, build with the Go version declared in `go.mod`:

```sh
./build.sh --build
./crux web --cwd /absolute/path/to/project
```

Open the printed URL. The default listener is `127.0.0.1` with an automatically
allocated port. Nothing opens automatically. Stop the host with Ctrl+C.

For first-time desktop setup, install the frontend dependencies. The Electron
launch command accepts native host options:

```sh
npm --prefix web install
npm --prefix web run desktop -- --cwd /absolute/path/to/project
```

Electron launches the repository's `crux` binary and terminates its host on exit.
`CRUX_BINARY=/absolute/path/to/crux` selects another binary explicitly. The
renderer is sandboxed, has no Node integration, and uses the same served assets
as the browser. A narrow preload bridge exposes the native directory picker;
filesystem operations and agent execution stay in the Go host.

## Package the desktop app

Electron-builder uses `web/icon.png` for application icons and bundles it for
the window icon.

```sh
npm --prefix web run desktop:pack
npm --prefix web run desktop:dist
```

`desktop:pack` creates an unpacked application; `desktop:dist` creates installers
for the current platform: DMG and ZIP on macOS, NSIS on Windows, and AppImage on
Linux. Both commands build the renderer, then run
`bash build.sh --build --embedded-mitmproxy` from the repository root before
packaging. Node/npm, Bash, and the repository's Go
version must be available. Outputs go to `dist/desktop`. Neither command publishes
artifacts or installs the app. Distribution signing requires the usual
electron-builder signing credentials in the build environment.

The native host is bundled outside ASAR and serves its embedded renderer assets.
Packaged launches default to the home directory rather than the application
bundle; `--cwd` selects a project explicitly. `CRUX_BINARY` can still select an
external host. The preload stays inside the application package.

Other launch options:

```sh
./crux web --listen 127.0.0.1:8080 --continue
./crux web --cwd /absolute/project --session SESSION_ID
./crux web --cwd /absolute/project --data-dir /absolute/project-data
```

`--session` accepts an exact ID or an unambiguous prefix. It cannot be combined
with `--continue`. `--yolo`, `--debug`, and native `--channels` options are also
forwarded to the workspace. `--json` prints the ready URL for launchers.

The web host listener is local-only. Non-loopback listeners, `--host`, and CLI
`--connection` selections are rejected. Electron's project sidebar selects saved
remote connections through the local host, reusing native client-owned runtime
publication and authentication.

## Settings and state

There is no browser-specific model or credential database. The host loads the
same configuration stack as native Crux: system, user, machine-owned state,
project configuration, and native runtime overrides. Existing provider plugins,
accounts, credentials, MCP configuration, hooks, skills, and tools are resolved
by that backend. Credentials stay in the host; configuration responses are
redacted by the native API.

The Settings panel selects main and small models, applies provider-declared
runtime controls, and displays provider availability and configuration
provenance. Changes use the native global or workspace scope and provider owner,
then update the running agent. Native validation errors are shown, not replaced
with another provider or model. The account panel supports API-key checking and
saving, saved-account switching, sign-out, and browser, pasted-code, or device
OAuth login for native provider registrations. Loopback login uses the same
callback relay as the terminal client. Completion requires the original login's
native runtime receipt; a refreshed status page alone is not a success receipt.
Disabled providers are hidden from model pickers and the provider list by default.
Use **Show disabled providers** in provider settings to manage or re-enable them.
OAuth providers have **Token maintenance**, which refreshes the saved token at
global or workspace scope for the exact provider registration. It does not start
a new sign-in; providers without refresh support report the native error.
Provider bundle installation remains a CLI operation.

Settings → Configuration → **Advanced configuration** writes a native key and
JSON value at global or workspace scope, or removes its saved override. This
covers settings without dedicated forms, including hooks, LSP definitions, and
tool lists. Removing an override exposes inherited configuration. The editor
starts empty rather than copying redacted values back into configuration, retains
failed drafts, and distinguishes a successful write from a failed view refresh.
Changes follow each subsystem's native lifecycle; restart language servers from
Integrations after changing their launch configuration. Use the dedicated Models
and Providers controls for runtime model selection and account operations.

**Authentication storage repair** reviews retained native operations by their
original workspace and operation IDs. Failed account mutations prefill those IDs
without starting a repair. Review shows recorded progress without changing files;
repair and abandonment use that exact reviewed revision. Partial failures retain
the reported progress and an exact-request retry. Abandonment retires recovery
intent without undoing saved changes. Repair only completes recorded storage
writes: restart the local web host to reload the saved settings, then review the
active account. A repair receipt does not confirm sign-in or runtime publication.

The workspace switcher browses the host's configured workspace roots and loads
the chosen directory's native history and config. **Create or clone…** creates an
empty directory, initializes a Git repository, or clones an HTTP(S) or SSH remote.
Choose a configured root and a relative directory path. Creation selects the
resulting path without switching workspaces; **Open** loads it separately.
Existing directories are not replaced. Git uses the host's credentials with
terminal credential prompts disabled. Closing the creation form cancels its
request; failed or canceled Git operations can leave files behind.

Directory creation uses `POST /v1/browser/create` with `root`, `relative_path`,
`mode` (`plain`, `git-init`, or `git-clone`), and `clone_url` for cloning only.
The native creator returns `{ "path": "/canonical/created/path" }`; it does not
attach a workspace or alter its runtime configuration. The route uses the same
management authorization and configured roots as directory browsing.

A custom `--data-dir` applies to the launch workspace; switching to another
workspace uses that workspace's normal data directory.

## Workspace interface

- Sessions: search, running/archive filters, rename, branch with origin tracking,
  archive/restore, export Markdown, delete, and rewind with optional file restore.
- Conversation: full-width chat with distinct user, assistant, and tool colors,
  streamed Markdown, code copying, searchable history, tool-call summaries,
  expandable arguments/results, reasoning, images, and attachments. Native
  conversation summaries have a distinct expandable card; streaming summaries
  open automatically, while errors remain visible outside the collapsed content.
- Composer: per-session drafts, file references, native skills, MCP prompt
  arguments and resource previews, a Normal/Plan/YOLO/Plan + YOLO dropdown,
  analysis effort, and Stop. Plan is session-scoped; YOLO controls tool
  auto-approval across the running workspace.
- Queue and Steer: explicit controls in the composer and Settings. Both persist
  through native `options.tui.delivery_mode`. Queue leaves the active turn
  running; Steer interrupts active generation and tools to redirect the turn.
  Summarization finishes before pending steering is applied. Pending messages
  remain visible and can be cleared.
- Session details: token/cost totals, context meter, summarization, plan/todos,
  native project context, files read, and saved file checkpoints.
- Background tasks: the toolbar Tasks button shows the workspace running count.
  Filter by session or workspace, search, follow live output, stop or restart
  shell tasks, continue agent tasks, open child sessions, and detach foreground
  jobs. Native task notifications render as separate expandable task cards, not
  user messages or XML.
- Shell dock: the always-available **Tasks** tab lists background work, and
  **Help** lists shortcuts and commands. Drag the expanded dock's top edge to
  resize it; its height is remembered, with a 220-pixel minimum. **Pop out** moves
  the panel into a separate window. Closing that window or choosing **Dock panel**
  returns it to the main window without recreating its panel state.
  The **Commands** tab runs workspace commands to completion with
  exit status and command history; output is also saved to the session.
  **Terminal** opens an interactive login shell (`$SHELL -l`) in the workspace
  through a host PTY, so editors, pagers, and full-screen programs work. **Attach
  tmux…** lists the same Crux tmux sessions as the terminal client and attaches
  to one; detaching (prefix, then `d`) ends that tab. Terminal tabs stay open
  while the dock is hidden and close their process when closed or when the page
  unloads. Up to 8 terminals run at once. Ctrl shortcuts go to the focused
  terminal; Cmd shortcuts still reach Crux. Terminals are not available on
  Windows hosts.
- Integrations: MCP state/catalog refresh, server sign-in (including servers the
  native runtime reports as needing OAuth), prompts/resources,
  language-server lifecycle and diagnostics, and skill content inspection.
- Decisions: native permission requests and structured question batches.
- Recent errors: the toolbar, command palette, or `/errors` opens the last 50
  errors retained in this browser tab, including native provider failure and
  refusal details. Search and copy individual or listed errors after dismissing
  their original banners or toasts. The list is kept in this browser's local
  storage for 7 days and survives page reloads; **Clear history** removes it.

In Electron, Settings → Behavior → **Background transparency** adjusts the
backdrop without fading text or controls. The default is 75% transparency;
changes apply immediately and are saved in browser storage.

Settings → Behavior → **Enable notifications** requests browser permission and
turns on alerts. The choice is remembered across page reloads while the browser
still grants permission; if permission was withdrawn, Behavior says so and alerts
stay off until re-enabled. Completed turns, background tasks,
permission requests, and questions notify when the tab is hidden or unfocused.
**Send test notification** also works while focused. Alerts use native event IDs
to avoid replay duplicates and do not wait for animation-frame rendering.
Clicking an alert focuses the client and opens its session only if its original
workspace is still active. Resolved decision prompts close their alerts.

Electron requests approval in a native dialog for the Crux window; unrelated
origins and embedded frames cannot use that grant. Alerts can contain session
titles, task summaries, and errors. Browser and system settings control whether
notifications appear; unsupported, denied, or failed delivery is shown in
Behavior. Disabling alerts or changing workspaces closes outstanding alerts.
These controls do not change terminal notification configuration.

Use Cmd/Ctrl+K for commands, Cmd/Ctrl+N for a new session, Cmd/Ctrl+F to search
the conversation, Cmd/Ctrl+J for the shell, and Cmd/Ctrl+, for Settings.

Reconnection reloads history and outstanding prompts before applying buffered
events. Closing a browser tab does not stop the host; Electron owns and stops
its host process. Unsent drafts are saved per workspace and session in browser
local storage and restored after a page reload; sending a message clears its
draft. Drafts older than 30 days are discarded. Attachments are kept while all
drafts fit in 2 MiB; otherwise attachments are dropped from the oldest drafts
first and the restored draft reports how many were omitted.

Browser storage belongs to the page's address. `crux web` picks a new port on
each launch by default, so drafts, recent errors, and the notification setting
carry over between launches only with a fixed `--listen` address.

## Instructions

Settings → Instructions provides two editors:

- **Session instructions** are stored with the selected conversation. Inherit
  uses native prompts; Add appends literal session text; Replace sends only the
  session text as the system prompt, including when that text is empty. Replacement
  excludes native tooling, provider, project, memory, goal, and plan prompt layers.
  Conversation history and tool permissions remain unchanged. Title generation
  and summarization retain their own task prompts. New, branched, and child
  sessions start without the override.
- **Native instruction files** edits the configured project, global, and provider
  files used by native Crux. Missing configured files can be created on save.
  File edits are shared rather than session-scoped; source settings and the
  selected provider determine which files enter the native prompt. The editor
  accepts regular UTF-8 files up to 2 MiB, not final-path links or directories.

Both editors retain drafts after save errors and report revision conflicts
instead of overwriting a detected newer version. Saved instructions apply at the
next model-response boundary, not to a request already executing. Session
replacement takes precedence over the shared instruction files.

## Path privacy

Open **Path privacy** from the command palette for the selected session. Enable
**Translate paths**, add absolute file or folder paths, and optionally give each
a unique label. **Subfolders** applies a target recursively. The native backend
replaces selected paths with stable aliases in model-facing prompts and tool
output, and resolves aliases back to their original paths for execution.

**Censor image copies** obscures protected images sent to the model without
modifying the original files. **Use inherited settings** removes the session's
explicit override. Save applies the policy to new turns; revision conflicts are
reported so a newer policy is not silently overwritten. Path translation is not
an operating-system sandbox and does not replace tool permissions.

## Performance

Solid's keyed stores update changed messages without rerendering the entire
transcript. Workspace events are batched once per animation frame, with repeated
full-message snapshots coalesced by message ID. Streaming Markdown is parsed at
most once per 80 ms, then rendered immediately on completion. The transcript
initially loads and mounts the most recent 120 messages. **Load earlier messages**
fetches older history in 120-message pages from the native API (`limit` and
`before` on the session messages endpoint); reloads after a turn keep pages
already loaded. Find shows **Search full history** and the rewind dialog shows
**Load earlier messages** until the whole conversation is loaded; export always
uses the full conversation. Offscreen message layout uses CSS
`content-visibility`.

The event stream opts in to two compact features. With `message_delta`, a
streaming update that only extends a text field is sent as the appended text
instead of the whole message; other part changes send only the changed parts.
The client rebuilds the full message from its per-connection copy and reloads
the transcript if a revision is ever missed. With `session_busy`, the host
reports busy and idle transitions for sessions seen on the connection, checked
in-process every 500 ms, so the client no longer polls the session list while
work is running. Other clients of the same channel keep receiving full
snapshots.

Event-driven reloads are deduplicated per batch: a frame containing several new
messages, completed runs, or task notifications triggers at most one session
list, transcript, queue, task, pending-decision, or workspace read. Explicit
actions still read immediately. Background polling (task counts, live task
output, and foreground-job counts) pauses while the tab
is hidden; returning to the tab refreshes sessions, the queue, and tasks once.
The live event stream and browser notifications continue while hidden.

The host listens only on loopback, so responses and the event stream are not
compressed: compression would add CPU on both ends without a bandwidth benefit.

Production assets are embedded in the Go binary. There is no runtime Node server
or CDN dependency. Hashed assets are immutable-cacheable; API responses are not.

## Develop and test

```sh
npm --prefix web ci
npm --prefix web test
npm --prefix web run build
./build.sh --build
npm --prefix web run test:integration
```

The frontend suite renders the production Solid components in a DOM test
environment, exercising composer/session controls, MCP context, account login
receipts, task controls, decisions, sanitization, and draft retention. It also
tests event coalescing, transport state, and the Electron host launcher.

The integration test launches the actual repository binary through the Electron
host launcher, drives the production web state and transport over HTTP and
WebSocket, and uses disposable settings plus a deterministic local provider.
It does not launch a browser or Electron window. It checks native model
persistence, streaming, history recovery, asset delivery, native Queue drain and
clear, native Steer interruption, session mode/effort/branching, shell persistence,
background task output, completion notifications, restart/stop, and host
shutdown. Tests use synthetic providers, not personal accounts; DOM
checks do not establish visual layout or real-provider sign-in acceptance.

The Go acceptance tests cover native permissions, questions, same-origin
boundaries, settings propagation, and model execution:

```sh
go test -race ./internal/webclient ./internal/question ./internal/permission
```

For hot reload, run the host on the development proxy's fixed port in one terminal
and Vite in another:

```sh
./crux web --listen 127.0.0.1:8080 --cwd /absolute/project
npm --prefix web run dev
```

Rebuild the embedded assets and local binary, then start Electron:

```sh
npm --prefix web ci --no-audit --no-fund
npm --prefix web run build
./build.sh --build
npm --prefix web run desktop
```

## Reuse the transport

`src/api.ts` exports `CruxClient`, a fetch/WebSocket client with native authority
headers, client identity, workspace attachment, session presence, cancellable
requests, and explicit
error responses. `src/state.ts` adds Solid stores, reconnect/snapshot recovery,
message batching, and native model updates. Both use the same-origin host served
by `crux web`; Electron IPC is optional and used only for the folder picker.
