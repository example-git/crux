Manage notes scoped to the active durable project (for example UI Cleanup), independently of repository-wide memory.

- `action: "list"`: return a compact newest-first index of note IDs and short titles, without note bodies. Use the returned `next_offset` to retrieve older entries.
- `action: "read"`: supply a `topic` ID from that project's index to load one note on demand. If `next_offset` is returned, continue reading that same topic at that offset.
- `action: "append"` (default): supply durable Markdown `content`. Start with a short descriptive heading that will serve as its index title. Existing content-only calls remain supported.

Consult the index and read relevant notes before relying on past project context. Do not load the whole notes archive into instructions. The archive remains intact, and its index is rebuilt from current entries whenever read, including task-update evidence. Note IDs are project-qualified; switching projects does not make the former project's notes available in the new project.
