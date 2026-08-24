# Crux configuration

Crux supports two configuration formats:

- `cruxrc` is executable Bash configuration built from Crux shell builtins.
- `crux.json` is static JSON validated by the checked-in schema.

Both formats are supported and deep-merged. They are trusted input: `cruxrc` executes as shell code, and selected JSON fields support shell expansion and command substitution. Review project configuration before starting Crux in an untrusted directory.

## Discovery and precedence

Crux loads user configuration and then project configuration from the repository root toward the current working directory. Files closer to the working directory have higher precedence.

Within one directory, precedence from lowest to highest is:

1. `crux.json`
2. `.crux.json`
3. `cruxrc`
4. `.cruxrc`

When JSON and shell configuration in the same directory overlap, `cruxrc` wins and Crux reports the conflict.

Default user locations are:

| Purpose | Unix-like default | Windows default |
| --- | --- | --- |
| User configuration | `~/.ai-cli/crux/` | `%USERPROFILE%\.config\crux\` |
| Mutable data | `~/.ai-cli/data/crux/` | `%LOCALAPPDATA%\crux\` |
| Cache | `~/.cache/crux/` | `%LOCALAPPDATA%\crux\cache\` |
| Project state | `<project>/.crux/` | `<project>\.crux\` |

XDG environment variables can change these defaults. Principal Crux overrides are `CRUX_GLOBAL_CONFIG`, `CRUX_GLOBAL_DATA`, `CRUX_CACHE_DIR`, `CRUX_SKILLS_DIR`, and `CRUX_AUTO_MEMORY_DIR`.

Machine-owned files under data directories are runtime state, not user configuration. Do not edit them as configuration or place executable `cruxrc` files there.

Crux does not discover legacy Crush configuration, environment variables, or state.

## `cruxrc`

A `cruxrc` file runs top to bottom in Crux's embedded Bash interpreter. Later statements win. It supports normal variables, conditionals, command substitution, and `source`.

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"

model large deepseek/deepseek-chat
permissions allow view ls grep
option skill-path ./skills
```

`CRUX_VERSION` is available while the file runs. Local development builds report `devel`.

### Providers

```bash
provider add <id> [flags]
provider remove <id>
```

Common flags include `--name`, `--type`, `--api-key`, `--base-url`, `--disable`, `--discover-models`, `--system-prompt-prefix`, `--tooling-instructions`, `--extra-header`, `--extra-body`, and `--provider-options`.

Custom providers use host-owned protocol implementations. Provider plugins and presets are installed separately through `crux plugins`; see [`../provider-plugins/README.md`](../provider-plugins/README.md).

### Models

```bash
model add <provider>/<model> [flags]
model remove <provider>/<model>
model large [<provider>/<model>] [flags]
model small [<provider>/<model>] [flags]
```

The large slot is the primary coding model. The small slot is used for auxiliary work such as summarization. Running `model large` or `model small` without an argument prints the current selection.

### MCP servers

```bash
mcp add <name> --type stdio|http|sse [flags]
mcp remove <name>
```

Stdio servers use `--command`, `--args`, and `--env`. HTTP and SSE servers use `--url` and optional repeated `--header` values. OAuth-capable servers can use the bounded OAuth configuration flags.

Configured remote MCP servers can connect when Crux initializes.

### Language servers

```bash
lsp add <name> --command <command> [flags]
lsp remove <name>
```

LSP configuration supports arguments, environment variables, file types, root markers, initialization options, settings, timeouts, and disabling.

### Hooks

```bash
hook add PreToolUse --command <command> [--name <name>] [--matcher <regex>] [--timeout <seconds>]
hook remove PreToolUse [--name <name>]
```

See [`../hooks/README.md`](../hooks/README.md) for the complete hook contract.

### Permissions

```bash
permissions allow view ls grep edit
permissions deny bash
```

Allowed tools skip interactive permission prompts. Denied tools are removed from the model's available tool set.

### Options

```bash
option progress false
option notifications disabled
option skill-path ./skills
option disable-skill <name>
option attribution-trailer-style assisted-by
option attribution-generated-with true
option ui compact true
```

Run the builtin `crux-config` skill or shell command help for the complete option and flag reference.

## JSON configuration

The canonical schema is [`../../schema.json`](../../schema.json).

```json
{
  "$schema": "https://raw.githubusercontent.com/example-git/crux/main/schema.json",
  "providers": {},
  "models": {},
  "mcp": {},
  "lsp": {},
  "hooks": {},
  "permissions": {},
  "options": {}
}
```

The schema is the authoritative field contract. Unknown or invalid explicit values fail configuration loading rather than silently selecting a fallback.

Selected string fields support `$VAR`, `${VAR}`, default/error forms, and `$(command)`. Provider request bodies remain structured JSON rather than shell-expanded data. A failed command substitution is a configuration error.

## Skills

Crux discovers embedded skills plus configured user and project paths. Project discovery includes:

- `.agents/skills`
- `.crux/skills`
- `.claude/skills`
- `.cursor/skills`

A skill is a directory containing `SKILL.md`. Use `option skill-path` for additional directories and `option disable-skill` to hide a skill.

## Security

Configuration executes with the invoking user's privileges. Do not launch Crux in a repository whose `cruxrc`, JSON substitutions, hooks, MCP commands, or LSP commands you have not reviewed. Keep credentials in environment variables or a secret manager rather than committing them to project configuration.
