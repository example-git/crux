Inspect the current Git repository through a bounded read-only operation without invoking the general shell tool.

Use this only when the user explicitly requests Git/history/commit/PR work, or when a specific Git fact is necessary and unavailable from existing context. Do not use it for routine repository discovery, automatic start/end checks, after ordinary edits, or repeatedly during implementation. In large repositories, prefer a relevant path-scoped status or diff over a repository-wide operation.

Supported actions are status, diff, log, and show. Use staged with diff for the index. Path is restricted to the current workspace. Revision is accepted only by show and must not begin with an option prefix.
