# Crux embedded TUI demo

`crux demo --port 8767` serves the real Go UI with dummy session data and embedded xterm.js assets. No Node runtime, checkout, provider credentials, or external frontend files are needed to run the built binary. Open http://127.0.0.1:8767.

The production `model.New`, message/tool factories, sidebar, branding, dialogs, completions, layout, and `UI.View()` produce the output. xterm displays that ANSI frame. Dummy data is loaded on every fresh page; edits are held in the page and Reset clears them. The demo does not start agents, run tools, or execute menu confirmation actions.

## Build and develop

```sh
# Rebuild browser assets after JS/CSS/HTML changes, then embed them in Crux.
npm --prefix tools/tui-mock ci
npm --prefix tools/tui-mock run build:web
go build -o crux .
./crux demo

# Development: Vite 8767 proxies the same Go server on 8768.
npm --prefix tools/tui-mock run dev

# Fixture tests, including native expansion, menus, and data edits.
go test ./internal/ui/model ./internal/ui/demo -run '^TestPreview' -count=1
```

To regenerate the pinned embedded headless parser, run `npm --prefix tools/tui-mock run build:headless` before `build:web`. Its source bundle, capture adapter, and upstream license live in `tools/tui-mock/public` and are copied into the embedded web directory by Vite.

Compiled assets live in `internal/ui/demo/web` and are included by `go:embed`. Keep generated assets with source changes; normal Go builds need no npm step unless browser sources changed. `go generate ./internal/ui/demo` also rebuilds them. Go renderer changes appear after rebuilding the binary; the Vite development server rebuilds and refreshes on UI source changes and reports failed builds visibly.

## Registry and coverage

`internal/ui/model/preview_registry.go` is the central discovery entry point. It supplies the catalog used by the browser and HTTP API. Modal registration pairs its ID, label, and actual constructor in one entry, so a new registered modal appears in the selector automatically. `preview_examples.go` registers message/tool/task fixtures; `preview_overlays.go` supplies completion fixtures. `PreviewData` in `preview_data.go` defines their editable inputs.

Existing production renderer changes flow into the demo automatically on rebuild. **A wholly new component still needs a fixture registration and representative data.** The demo cannot infer how to construct an arbitrary new Go component. The factory coverage test fails when a dedicated tool case has no fixture. Menu tests exercise every registered modal/popover. Sidebar, editor, To-Do, status, and header use the normal full-session layout.

The catalog contains 81 examples, 14 modals, and 3 popovers. Output-bearing fixtures carry enough content to exercise native disclosure/expansion (typically 24 lines); acknowledgments that have no collapsible production body are labeled accordingly. DUMMYAI uses the normal provider brand and ASCII-wordmark pipeline. Three dummy models cover 64K, 280K, and 1M context windows.

## Agent help endpoints

`GET /help` and `/help/index.md` serve an embedded Markdown index linking detailed screenshot, controls, and audit guides. `/` serves the same index to HTTP clients and the interactive page to requests accepting `text/html`; `Accept: text/markdown` explicitly selects the index. All help pages remain available with or without a browser connected.

`/help/options.md`, `/help/fixture.md`, and `/help/frame.md` are generated from the running binary's Go types with the existing `invopop/jsonschema` library. Their corresponding `.schema.json` endpoints expose machine-readable schemas. Each rebuild updates these references automatically; the screenshot defaults use the same source as the request handler. These describe structural types, not all runtime validation rules.

Handwritten guides live in `tools/tui-mock/public/help` and are copied to the embedded web directory by `build:web`.

## Agent control API

With no connected browser, `POST /api/control` accepts `{"action":"screenshot"}` or `{"action":"set","state":{"cols":65,"rows":25,"modal":"instructions"},"screenshot":true}`. It renders the production ANSI frame through embedded Goja, xterm/headless 6.0.0, SVG, and resvg WASM, with no Node or browser runtime. Defaults are 160×45 cells (1280×720 pixels), 13px text and an 8×16 cell grid. macOS loads its local Menlo and Apple Symbols fonts; other platforms use embedded Go Mono. All platforms include Noto Sans JP and monochrome Noto Emoji fallbacks. Their upstream SIL Open Font Licenses are preserved beside the font files in `public/fonts`. Receipts identify `captureRenderer: "goja-xterm-svg-resvg"`, dimensions, visible lines, and capture time. This experimental renderer has different font metrics from the browser's Menlo; font coverage and rasterization can differ. Each clientless request starts from default preview state, with optional `state` fields matching `PreviewOptions`; unsupported fields fail. Other control actions still require a browser. An explicit missing client ID never falls back to clientless rendering.

With a browser connected, `POST /api/control` sends a command to a connected preview page. It returns **after xterm paints**, with a revision, visible terminal lines, item IDs, named regions, and cell/pixel geometry. `GET /api/control` lists connected client IDs. With multiple pages, include `clientId` explicitly. A missing/disconnected page or expired command returns an error. The local server binds only to loopback.

Use the included client to save diagnostics:

```sh
node tools/tui-mock/control.mjs \
  '{"action":"set","state":{"example":"tool-job_output","toolsExpanded":false},"screenshot":true}' \
  --png /tmp/job-output.png --json /tmp/job-output.json
```

Options: `--url http://127.0.0.1:8767`, `--client ID`, `--file command.json`, `--png output.png`, and `--json receipt.json`. The PNG command requires `screenshot:true` or `action:"screenshot"`.

The same commands are available as `await window.cruxPreview.control(command)` and through the page's **Preview control API** JSON panel.

| Command | Effect |
| --- | --- |
| `{"action":"catalog"}` | List example/model/modal/popover IDs. |
| `{"action":"inspect"}` | Read the painted frame, lines, item IDs, and named regions. |
| `{"action":"set","state":{"example":"all","focusItem":"ITEM_ID","scroll":0}}` | Jump to a transcript item; scroll is an offset from its top. |
| `{"action":"set","state":{"modal":"models","menuRow":1}}` | Open a native modal and select a row. |
| `{"action":"set","state":{"popover":"files","menuRow":2}}` | Open a native completion popover. |
| `{"action":"find","text":"lines hidden"}` | Locate visible text and return zero-based cells plus browser click coordinates. |
| `{"action":"click","cell":{"x":5,"y":13}}` | Send a native chat click, including disclosure toggles. |
| `{"action":"screenshot"}` | Capture the actual xterm canvas as PNG. |
| `{"action":"data"}` | Read the dummy data document and current overrides. |
| `{"action":"reset"}` | Restore initial dummy state, data, font, and spacing. |

`set` accepts the page controls: `example`, `model`, `scenario`, `modal`, `popover`, `menuRow`, `focusItem`, `scroll`, `compact`, `planExpanded`, `toolsExpanded`, `toolsCompact`, `showUser`, `input`, and `submitted`. Top-level `fontSize` and `lineHeight` adjust xterm. `data` supplies fixture overrides. Unknown commands/fields and invalid selections produce errors.

## Editing fixture values

Read `GET /api/fixture` or `action:"data"` to discover exact native JSON names (some native structs use PascalCase). Patch existing values with JSON Pointer paths:

```json
{
  "action": "patch",
  "changes": [
    {"path":"/session/Title","value":"A long title to test sidebar wrapping"},
    {"path":"/examples/tool-job_output/result/content","value":"First output line\nSecond output line"},
    {"path":"/provider/brand/short_name","value":"EXAMPLE"},
    {"path":"/models/0/context_window","value":64000}
  ],
  "screenshot": true
}
```

The document includes session/To-Do values, models, provider branding, file changes, usage, MCP/LSP states, every registered example's message parts/tool parameters/results/status, task and session menu rows, file/resource/command completions, permission requests, codebase-index status, instructions settings/sections, summarization settings, and editor placeholders. Structured tool bodies and metadata retain their production formats. Message parts retain their concrete Go types; use the registered fixture for the message type you want to edit.

`patch` updates the current data document; `set.state.data` replaces the override object and merges it onto defaults. Arrays can be replaced. Preserve the native schema; paths are case-sensitive. `session.MessageCount` is derived from transcript items. `/models` is the single owner of model definitions. Inactive fixtures are edited immediately and become visible when selected. Built-in UI labels and behavior remain production code.

## Screenshot diagnostics

Browser capture uses xterm's WebGL canvas with a preserved drawing buffer, after its paint acknowledgment. It returns a PNG with the same revision as the text receipt. HTTP receipts include `/api/control/frames/<command-id>.png`; the server retains the last 12 captures in memory. The browser API returns a PNG data URL directly. PNGs contain the terminal only; browser controls are outside the capture.

A diagnostic model does not need to operate browser tools: it can call HTTP and read the PNG. With no browser connected, the embedded Goja/xterm/SVG/resvg path renders the PNG directly. Browser-only interaction commands still require a connected page. The Go server does not bundle Chromium. Missing WebGL or lost contexts produce an explicit capture error. Text-only models can use the line/region receipt; vision-capable models can inspect the PNG.

## Main implementation files

- `internal/cmd/demo.go`: `crux demo` entry point.
- `internal/ui/demo/`: embedded HTTP server and diagnostic command/image bridge.
- `internal/ui/model/preview_registry.go`: catalog and modal constructors.
- `internal/ui/model/preview_data.go`: editable native fixture data.
- `internal/ui/model/preview*.go`: real UI adapter, examples, long outputs, and tests.
- `tools/tui-mock/main.js`: xterm and page controls.
- `tools/tui-mock/preview-control.js`: agent controls, paint acknowledgments, and PNG capture.
- `tools/tui-mock/control.mjs`: HTTP diagnostic CLI.

## Read-only saved-session replay

Select the filesystem source **only at process startup**:

```sh
./crux demo --project /path/to/project -s SESSIONID
```

`-s` is the short form of `--session`; full IDs and unambiguous session short hashes follow the continue command's resolution rules. With `-s` and no `--project`, the startup working directory (or `--cwd`) supplies the project. The server looks only for that project's `.crux/crux.db`, opens it with the existing read-only SQLite connection, and performs no migrations. Without these flags, initial data remains dummy. `--project` without `-s` enables the saved-session picker but still starts on the dummy fixture.

The API accepts **session IDs only**, never a project directory or database path:

- `GET /api/sessions`: list sessions from the configured source, including child sessions.
- `POST /api/sessions/load` with `{"sessionId":"..."}`: load an in-memory snapshot.
- Control command `{"action":"loadSession","sessionId":"..."}`: load and paint a snapshot in the browser.
- Control command `{"action":"sessions"}`: discover the configured source's sessions.

**Top**, **Message # → Go**, and **Bottom** use native chat scrolling. For imported sessions, numbers refer to the database's stored-message order; tool-result rows jump to their linked tool-call item. Messages with no visual item return an explicit explanation. Diagnostic receipts include `storedMessages` with number, role, ID, and target item ID, plus `scrollOffset`. Use `state.messageNumber` or `state.bottom` through the API for the same actions.

Session messages, IDs, content parts, tool-result links, summaries, To-Dos, usage totals, and persisted file-history differences are loaded through the application's existing decoders/renderers. No tool or provider is executed. Session mode imports providers and models through the normal launch configuration/discovery path, stopping before configuration persistence. It supplies the redacted provider surfaces, real branding, and provider-qualified model choices to the native UI. The saved provider/model identity is retained; missing historical metadata is disclosed. Live MCP/LSP/usage activity is not reconstructed from the database. A fresh reload with `-s` loads a new snapshot; Reset returns to dummy data.

`{"action":"data","path":"/session"}` reads one effective data subtree. Normal diagnostic receipts omit the potentially large fixture document and report `dataModified`; use the data command when the document itself is needed.

Older databases without `unseen_local_tokens` use its migration default of zero through a read-only query projection. This is disclosed in diagnostic `schemaNote`; the database is never migrated by the demo.
