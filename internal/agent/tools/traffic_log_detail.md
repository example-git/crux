Inspect one traffic record selected by the composite record ID returned by traffic_logs.

<usage>
- Requires a record_id such as http/request/17, http/response/17, or websocket/frame/9
- Returns bounded metadata and compact request/response summaries for exactly one record
- Omits the body by default
- Set include_body only when a body prefix is necessary; body and total output remain strictly truncated
</usage>

<tips>
- Prefer traffic_log_search when looking for specific text in a large request or response body
- Request and response IDs are table-local, so always copy the complete composite ID
- Credential-bearing values are redacted before storage, but traffic records remain private
</tips>
