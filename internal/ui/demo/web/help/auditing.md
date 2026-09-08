# Visual audit workflow and troubleshooting

[Help index](/help/index.md) · [Screenshots](/help/screenshots.md) · [Controls](/help/controls.md)

## Reproducible audit

1. Discover the running server and connected clients with `GET /api/control`. Do not interrupt other sessions or target an unidentified browser page.
2. Read `GET /api/catalog` and choose representative fixtures and overlays.
3. Capture a 1280×720 baseline. Save its command, JSON receipt, and PNG.
4. Inspect contrast, hierarchy, borders, alignment, wrapping, clipping, selection, footer hints, disclosure indicators, and empty/error states.
5. Capture compact states with explicit dimensions and compare equivalent content. Repeat all desired state in clientless commands.
6. Reproduce each suspected defect with the smallest fixture/state combination. Include the screenshot, exact command, affected component, expected behavior, and severity.
7. Distinguish production UI problems from font or rasterization artifacts. Compare with the browser or native terminal if glyph coverage or rendering fidelity is in doubt.

The preview invokes production UI rendering, but it does not run agents, execute tools, or confirm real menu actions. A screenshot is evidence of presentation, not proof of authentication, notification delivery, or tool execution. Native preview disclosure clicks can verify preview expansion; do not claim unrelated runtime behavior from them.

## Common errors

| Symptom | Meaning and next check |
| --- | --- |
| `Select one connected preview client` | A browser-only action has no client, an explicit client is missing, or multiple pages require selection. Check `GET /api/control`; use a supported clientless screenshot command if appropriate. |
| Unexpected browser size | A browser is connected, so the command used that page rather than clientless defaults. Check the receipt and client list. |
| Unsupported field or invalid dimensions | Correct the request; clientless uses `state.cols` and `state.rows`, not pixel sizes or top-level browser font controls. |
| Unknown fixture/model/modal | Discover identifiers through `/api/catalog`. |
| PNG URL returns 404 | Capture was evicted from the 12-image cache or the server restarted. Recapture and save promptly. |
| Slow first capture | Embedded JS and WASM initialize lazily. Compare subsequent timings before attributing the cost to every capture. |
| Missing glyphs or differing spacing | Inspect reported font and grid. Non-macOS Go Mono has limited symbol coverage; browser and server rasterization can differ. |
| Browser paint timeout or WebGL error | The selected browser failed to acknowledge/capture a frame. This is distinct from the embedded clientless engine. |
| Connection refused | Verify the user-supplied address and server availability. Do not restart unrelated processes. |

## Root returns the wrong format

`/help` and the `.md` guide URLs always return Markdown. `/` uses the request's `Accept` header: browser HTML requests receive the interactive UI; ordinary HTTP requests receive the guide index. Use `Accept: text/markdown` explicitly for agent documentation.

## Build and availability

The guides, parser, and browser assets are embedded in the binary. Source changes require a rebuild before an already-running server can expose them. The source guides live under `tools/tui-mock/public/help`; the frontend build copies them into `internal/ui/demo/web/help`. Normal users of a built binary need neither the checkout nor Node.
