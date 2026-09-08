# Discovery and interaction

[Help index](/help/index.md) · [Screenshots](/help/screenshots.md) · [Audit workflow](/help/auditing.md)

## Endpoints available without a browser

| Request | Result |
| --- | --- |
| `GET /api/control` | Connected client IDs and advertised control actions. Some actions require a browser. |
| `GET /api/catalog` | Current fixture/model/modal/popover registry. |
| `GET /api/fixture` | Native fixture data document. |
| `POST /api/preview` | Production ANSI frame and geometry, not a PNG. Accepts preview options directly, rather than an `action` wrapper. |
| `POST /api/control` | Clientless screenshots when no browser is connected; see the screenshot guide. |
| `GET /api/control/frames/<id>.png` | PNG at the exact URL returned by a capture. |

For an imported snapshot, `/api/catalog?importId=ID` and `/api/fixture?importId=ID` select its data. Do not invent identifiers: obtain them from discovery or snapshot loading.

## Browser control commands

Open `/` in a browser to connect the interactive preview. `GET /api/control` reveals its client ID. Send the following to `POST /api/control`, adding `clientId` when targeting a particular page. Browser commands wait for that page's rendering acknowledgment.

| JSON command | Effect |
| --- | --- |
| `{"action":"catalog"}` | Discover the page's registry. |
| `{"action":"inspect"}` | Read painted text, item IDs, regions, and geometry. |
| `{"action":"set","state":{"modal":"instructions"}}` | Change page state. |
| `{"action":"find","text":"lines hidden"}` | Find visible text and cell/click coordinates. |
| `{"action":"click","cell":{"x":5,"y":13}}` | Send a native preview click, such as a disclosure toggle. |
| `{"action":"screenshot"}` | Capture the current terminal canvas. |
| `{"action":"data"}` | Read fixture data. |
| `{"action":"data","path":"/session"}` | Read a bounded subtree. |
| `{"action":"reset"}` | Restore dummy state and page settings. |

Add `"screenshot": true` to a state-changing browser command to obtain a matching PNG. Browser font controls are top-level `fontSize` and `lineHeight`. These are not clientless options.

## Fixture edits

Native field names are case-sensitive. Discover them with `/api/fixture` or the browser `data` command. `set.state.data` supplies fixture overrides. Browser `patch` commands change existing JSON Pointer paths:

```json
{
  "action": "patch",
  "changes": [
    {"path":"/session/Title","value":"A long title for wrapping checks"},
    {"path":"/examples/tool-job_output/result/content","value":"First line\nSecond line"}
  ],
  "screenshot": true
}
```

Arrays can be replaced. Preserve the native schema and use a fixture of the appropriate message/tool type. These edits affect preview data, not production source code or persisted session content. Clientless capture does not support the `patch` action; use `state.data` with a screenshot request instead.

## Read-only saved sessions

The server's filesystem source is selected at startup:

```sh
crux demo --project /path/to/project -s SESSIONID
```

Do not restart a user's server to change its source without their direction. With a source configured:

1. `GET /api/sessions` lists available sessions.
2. `POST /api/sessions/load` with `{"sessionId":"..."}` loads an in-memory snapshot.
3. Use its returned import identifier in screenshot `state.importId` or the imported fixture/catalog endpoints.

Browser equivalents are `{"action":"sessions"}` and `{"action":"loadSession","sessionId":"..."}`. `state.messageNumber` and `state.bottom` select imported transcript positions.

The API accepts session IDs, not arbitrary database paths. It reads the configured project's database without migrations and does not execute providers or tools. Historical content does not reconstruct live MCP/LSP activity. Treat saved-session screenshots and receipts as private data.
