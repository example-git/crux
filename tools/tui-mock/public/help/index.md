# Crux UI preview: agent guide

The preview renders Crux's production terminal UI with fixture data or a read-only session snapshot. Use it to inspect layout, colors, wrapping, dialogs, and tool cards without starting agents or executing tools.

## Guides

- [Screenshots and sizing](/help/screenshots.md): clientless capture, state options, PNG retrieval, fonts, and receipts.
- [Discovery and interaction](/help/controls.md): catalogs, fixtures, browser commands, and saved-session replay.
- [Audit workflow and troubleshooting](/help/auditing.md): reproducible comparisons, errors, and verification limits.

## Generated schema references

These pages and their linked JSON Schemas are generated from the running binary's compiled Go types using `invopop/jsonschema`. Every rebuild picks up type changes automatically; no manual schema regeneration step is needed.

- [Preview options and clientless defaults](/help/options.md) · [JSON Schema](/help/options.schema.json)
- [Native fixture data](/help/fixture.md) · [JSON Schema](/help/fixture.schema.json)
- [ANSI preview frame](/help/frame.md) · [JSON Schema](/help/frame.schema.json)

These are structural references, not complete validators for runtime catalog choices or browser commands. Use the guides for operational rules and `/api/catalog` for current identifiers.

## Quick start

The usual local address is `http://127.0.0.1:8767`. Use the address of the server already running; do not restart someone else's server.

1. `GET /api/control` lists connected browser clients.
2. `GET /api/catalog` lists available fixture, model, modal, and popover identifiers.
3. With no browser clients connected, send `POST /api/control` with `Content-Type: application/json` and this body:

```json
{"action":"screenshot"}
```

4. Read the JSON response, then `GET` the relative URL in `screenshot.url` from the same server. Save that PNG promptly and inspect it together with the receipt.

The clientless default is **1280×720 pixels**, or **160 columns × 45 rows**, using an 8×16-pixel cell grid. A connected browser instead captures its current terminal dimensions.

## Finding help and the browser UI

- `GET /help`, `/help/`, and `/help/index.md` always return this Markdown index.
- `GET /help/screenshots.md`, `/help/controls.md`, and `/help/auditing.md` return the linked guides as `text/markdown`.
- `GET /` returns this index for ordinary HTTP clients, including requests with no `Accept` header or `Accept: */*`.
- Browser requests accepting `text/html` receive the existing interactive page at `/`. Request `Accept: text/markdown` to select Markdown explicitly.
- This is request-based content negotiation, not a separate process mode. Connecting a browser affects control-command routing, not availability of these help pages.

No browser or Node runtime is required for clientless capture. The optional checkout CLI uses Node as an HTTP client only; any HTTP client can call the API directly.
