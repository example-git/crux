List Crux HTTP and WebSocket traffic records without loading request or response bodies into context.

<usage>
- Returns a compact timestamp-sorted index of matching requests, responses, handshakes, and frames
- Each row includes a composite record ID such as http/request/17
- Filters by substring, protocol, direction, phase, RFC3339 time range, and sort order
- Defaults to 20 newest records and never returns more than 50
- Never returns headers, body summaries, or raw bodies
</usage>

<tips>
- Use traffic_log_detail with a composite record ID for one bounded record
- Use traffic_log_search with a composite record ID and query to search one body without loading it in full
- Search can locate records by provider hostname, model, error text, or a body fragment while keeping list output compact
- Credential-bearing values are redacted before storage, but traffic records remain private
</tips>
