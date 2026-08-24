# Crux hooks

Hooks are user-defined commands that run at bounded agent lifecycle events. They can block a tool call, halt a turn, approve a call before the normal permission prompt, rewrite tool input, or append context to a tool result.

Only `PreToolUse` is currently supported.

## Configuration

Hooks can be declared in `crux.json`:

```jsonc
{
  "hooks": {
    "PreToolUse": [
      {
        "name": "protect-root",
        "matcher": "^bash$",
        "command": "./hooks/protect-root.sh",
        "timeout": 10
      }
    ]
  }
}
```

Or in `cruxrc`:

```bash
hook add PreToolUse \
  --name protect-root \
  --matcher '^bash$' \
  --command './hooks/protect-root.sh' \
  --timeout 10
```

Project hooks take precedence over global hooks. Matching commands are deduplicated, run in parallel, and aggregated in configuration order.

The command is resolved from the current working directory, not from the directory containing the configuration file. Use an absolute command path for a global hook when its location is not relative to every project.

Hooks run for top-level tool calls and detached background-agent tool calls. A synchronous delegated agent is represented by its outer tool call and does not recursively run the parent's hooks for every child call.

## Input

A hook receives JSON on standard input:

```json
{
  "event": "PreToolUse",
  "session_id": "313909e",
  "cwd": "/path/to/project",
  "tool_name": "bash",
  "tool_input": {
    "command": "go test ./..."
  }
}
```

The following environment variables are also available:

| Variable | Description |
| --- | --- |
| `CRUX_EVENT` | Event name |
| `CRUX_TOOL_NAME` | Tool being called |
| `CRUX_SESSION_ID` | Current session ID |
| `CRUX_CWD` | Current working directory |
| `CRUX_PROJECT_DIR` | Project root |
| `CRUX_TOOL_INPUT_COMMAND` | `command` from tool input, when present |
| `CRUX_TOOL_INPUT_FILE_PATH` | `file_path` from tool input, when present |

Event names are case-insensitive and accept snake case. `PreToolUse`, `pretooluse`, and `pre_tool_use` select the same event.

## Output

A hook communicates through its exit status and optional JSON on standard output.

| Exit status | Result |
| --- | --- |
| `0` | Parse the JSON result, if any |
| `2` | Deny this tool call; stderr is the reason |
| `49` | Halt the complete turn; stderr is the reason |
| Other | Non-blocking hook error; the tool call continues through normal controls |

The native JSON envelope is:

```json
{
  "version": 1,
  "decision": "allow",
  "halt": false,
  "reason": "",
  "context": "Additional context",
  "updated_input": {
    "command": "go test -race ./..."
  }
}
```

- `decision` is `allow`, `deny`, or omitted. `allow` bypasses the normal permission prompt for that exact call.
- `halt` ends the turn.
- `reason` explains a denial or halt.
- `context` is appended to the tool result visible to the model.
- `updated_input` is a shallow-merge patch. Omitted keys remain unchanged; nested values supplied by the hook replace the corresponding nested value.

When multiple hooks match, deny wins over allow, halt is sticky, reasons and context are joined in configuration order, and later input patches win for colliding keys.

Crux also accepts the compatible Claude Code `hookSpecificOutput` decision envelope. Input rewrites remain Crux shallow-merge patches rather than complete input replacement.

## Examples

### Block a destructive command

```bash
#!/usr/bin/env bash
set -euo pipefail

if [[ ${CRUX_TOOL_INPUT_COMMAND:-} =~ rm[[:space:]]+-(rf|fr)[[:space:]]+/ ]]; then
  echo "Refusing to remove the filesystem root" >&2
  exit 2
fi
```

### Approve read-only tools

```jsonc
{
  "matcher": "^(view|ls|grep|glob)$",
  "command": "echo '{\"decision\":\"allow\"}'"
}
```

### Add context without approving

```bash
#!/usr/bin/env bash
set -euo pipefail

if [[ ${CRUX_TOOL_INPUT_FILE_PATH:-} == *.go ]]; then
  echo '{"context":"Run gofmt and focused Go tests after editing."}'
else
  echo '{}'
fi
```

Omitting `decision` preserves the normal permission flow.

### Rewrite a command

```bash
#!/usr/bin/env bash
set -euo pipefail

input=$(cat)
command=$(jq -r '.tool_input.command' <<<"$input")
rewritten=$(some-rewriter <<<"$command")
jq -n --arg command "$rewritten" '{updated_input:{command:$command}}'
```

## Execution and security

Inline commands and shebang-less shell scripts run through Crux's embedded shell. A script with a shebang dispatches to the named interpreter. Hooks inherit the invoking user's privileges and can inspect their declared input, start processes, or access the filesystem like other configuration commands.

A timeout or non-blocking hook failure does not deny the tool call. Use exit status `2` for a policy denial and `49` only when retrying within the same turn must also be prevented.

Review every hook before enabling it. Do not place credentials in hook output, logs, or committed configuration.
