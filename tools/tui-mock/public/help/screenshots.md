# Screenshots and sizing

[Help index](/help/index.md) · [Controls](/help/controls.md) · [Audit workflow](/help/auditing.md)

## Routing

Send JSON commands to `POST /api/control`.

- Zero connected clients and no explicit client ID: `action: screenshot` or `action: set` with `screenshot: true` renders on the server.
- One connected client: an untargeted command goes to that browser.
- Multiple clients: include the intended `clientId`, obtained from `GET /api/control`.
- An explicit missing client ID fails; it does not select another client or switch to clientless rendering.

Browser screenshots use the browser's current size. The dimensions below describe the clientless renderer.

## Capture commands

Default **1280×720**:

```json
{"action":"screenshot"}
```

Compact Instructions dialog, **520×400**:

```json
{"action":"screenshot","state":{"cols":65,"rows":25,"modal":"instructions"}}
```

Expanded tool output at the default size:

```json
{"action":"set","state":{"example":"tool-job_output","toolsExpanded":true},"screenshot":true}
```

Each clientless request starts from default preview options; repeat all desired state fields in every request. It does not retain the preceding request's selected modal or size.

## Dimensions and fonts

Set `state.cols` and `state.rows`, not pixel width and height. Width is `cols × 8`; height is `rows × 16`. Valid ranges are 45–500 columns and 15–200 rows. Explicit out-of-range dimensions fail.

| Pixels | Columns | Rows |
| --- | ---: | ---: |
| 1280×720 (default) | 160 | 45 |
| 960×640 | 120 | 40 |
| 520×400 | 65 | 25 |
| 1920×1088 | 240 | 68 |

The fixed grid cannot express every pixel height: 1080 is not divisible by 16. The receipt reports actual dimensions.

The embedded engine is Goja → xterm/headless → SVG → resvg WASM. Text is 13px. macOS loads local Menlo and Apple Symbols alongside embedded Go Mono. All platforms include Noto Sans JP and monochrome Noto Emoji fallbacks for Japanese and emoji glyphs. The receipt identifies the actual font family. Font size and line-height overrides are currently browser-only; do not send them to clientless capture. Rasterization is not established as pixel-identical to a browser or native terminal.

## State fields

Use `GET /api/catalog` to obtain valid identifiers rather than guessing them.

| Fields | Purpose |
| --- | --- |
| `cols`, `rows` | Terminal dimensions in cells. |
| `example`, `model`, `scenario` | Fixture, model, and session scenario. |
| `modal`, `popover`, `menuRow` | Overlay and selected menu row; `none` closes an overlay. |
| `focusItem`, `scroll` | Focus an item by ID and adjust its scroll offset. |
| `compact`, `planExpanded`, `toolsExpanded`, `toolsCompact` | Layout and disclosure options. |
| `input`, `submitted` | Editor text and submitted fixture text. |
| `data` | Native fixture overrides; inspect `GET /api/fixture` for the schema. |
| `importId`, `messageNumber`, `bottom` | Read-only imported session snapshot and position. |
| `instance`, `resetRevision`, `click` | Native preview instance/reset/click inputs for advanced interaction; a click is a cell point `{ "x": 5, "y": 13 }`. |

Defaults include `example: all`, `model: dummy-coder`, `scenario: working`, `modal: none`, `popover: none`, and `planExpanded: true`. Unknown state fields fail. Clientless top-level fields are `action`, `state`, `screenshot`, and `clientId` only.

## Receipt and PNG

A successful response contains:

- `renderer`: `crux/internal/ui/model.UI.View`, identifying the production renderer.
- `captureRenderer`: `goja-xterm-svg-resvg` for clientless captures.
- `commandId` and `revision`: correlating this PNG with its receipt.
- `state`: effective preview options.
- `lines`: xterm's visible terminal text.
- `items` and `regions`: item IDs and named cell-space rectangles.
- `elapsedMs`: server-side render/capture time, excluding download time.
- `screenshot`: relative PNG `url`, pixel `width` and `height`, font and cell metrics.

Fetch `screenshot.url` from the same server. Only the latest 12 captures are retained in memory; older URLs expire and all URLs disappear on restart. Save both the PNG and JSON receipt for reproducible evidence.

## Optional checkout CLI

Run from a checkout with Node installed:

```sh
node tools/tui-mock/control.mjs \
  '{"action":"screenshot"}' \
  --url http://127.0.0.1:8767 \
  --png /tmp/preview.png --json /tmp/preview.json
```

Options also include `--client ID` and `--file command.json`. The server itself does not need Node. Direct HTTP clients should allow enough time for the first capture to initialize the embedded engines.
