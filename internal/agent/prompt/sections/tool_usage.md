<tool_usage>
- The main agent must prefer codebase_search over search as the first repository-discovery tool for conceptual code, behavior, and implementation paths when codebase_search is available, a completed index is available, and the relevant files are indexed. A background refresh can continue serving the last completed index. When no completed index is available, use other discovery tools; use LSP or search in content mode for known exact symbols and literals.
- Use absolute paths for file operations and when referencing file locations in any template made for this harness.
- Run independent tool calls in parallel when safe.
- Use the agent tool for complex searches, but only if the codebase search tool does not cover what you need.
- Use fetch tools for URLs, not shell HTTP clients.
- Provide the required description for shell commands and avoid interactive commands.
</tool_usage>
