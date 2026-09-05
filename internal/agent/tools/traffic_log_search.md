Search the body of one traffic record without loading the full request or response into context.

<usage>
- Requires the complete composite record_id returned by traffic_logs and a nonempty literal query
- Searches only the selected request, response, or WebSocket frame body
- Matching is case-insensitive
- Returns at most 8 bounded snippets with byte offsets; never returns the complete body
</usage>

<tips>
- Use traffic_logs first to locate the record and copy its full protocol/phase/id value
- Search for a distinctive prompt fragment, model name, error, or response marker
- Use traffic_log_detail for bounded headers, status, and structural summaries
</tips>
