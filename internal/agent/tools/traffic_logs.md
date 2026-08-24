Search and inspect Crux's bounded HTTP and WebSocket traffic database without loading full payloads into context.

<usage>
- Returns a timestamp-sorted list of matching HTTP requests/responses and WebSocket handshakes/frames
- Summarizes JSON shape and extracts effective instructions, user messages, and assistant messages
- Filters by substring, protocol, direction, phase, RFC3339 time range, and sort order
- Defaults to 20 newest records and never returns more than 100
- Set include_body only when a compact summary is insufficient; raw output is truncated
</usage>

<tips>
- Search for a provider hostname, model, request ID, error text, or message fragment
- Use protocol=websocket and direction=outbound to inspect Codex request frames
- Use sort=asc to reconstruct a request/response sequence chronologically
- Credential-bearing headers and URL query values are redacted before storage
</tips>
