---
name: custom-agents
description: Use when the user wants to list, locate, create, inspect, or edit custom subagent definition files, or configure a custom subagent's provider, model, tools, description, or instructions.
---

# Custom Agent Definitions

Custom agent definitions are Markdown files with strict YAML frontmatter and a required instruction body.

## Locations and precedence

Definitions are direct `.md` files in these directories:

- Project scope: `<working-directory>/.ai-cli/agents/*.md`
- User scope: `~/.ai-cli/agents/*.md`

Project definitions override user definitions with the same `name`. Discovery is not recursive.

When listing definitions, search both directories separately with `mode: "files"`. Report every physical file with its scope and path. If both scopes define the same name, identify the project file as active and the user file as overridden. Do not hide overridden files.

## Required format

```markdown
---
name: code-reviewer
description: Reviews changes for correctness and regressions
model: provider-id/model-id
tools:
  - view
  - search
---

# Instructions

Review the requested changes and report concrete findings.
```

All four frontmatter fields are required. `tools: []` grants no tools. `tools: ["*"]` grants every available non-recursive tool. The wildcard must be the only tool entry.

The body is literal instructions for the subagent. It must not be empty. Template syntax in the body is not expanded.

## Validation rules

- `name` must match `^[a-z][a-z0-9_-]*$`.
- `coder` and `task` are reserved names.
- `description` must be non-empty.
- `model` must use an exact configured `provider/model-id` pair.
- The provider must exist and be enabled.
- Every named tool must exist and be enabled.
- Custom subagents cannot receive recursive delegation, plan lifecycle, question, or MCP resource-discovery tools.
- Duplicate tool names are invalid.
- Unknown frontmatter fields are invalid.

Use `crux_info` to inspect configured providers, models, enabled tools, and disabled tools before creating or changing explicit model or tool selections. If an explicit provider, model, or tool cannot be resolved, stop with a clear error instead of substituting a default.

## Configured Python script tool

A custom agent can receive a bounded `script` tool instead of `bash`. Add `script` to `tools` and configure exactly one Python file:

```yaml
tools: [script]
script:
  path: ./scripts/classify.py
  timeout: 30s
  variables:
    input:
      flag: --input
      required: true
    format:
      flag: --format
      default: json
      values: [json, text]
    mode:
      flag: --mode
      value: fast
```

Relative script paths resolve from the agent definition file's directory. The path must resolve to an existing regular `.py` file. `timeout` defaults to `2m` and must not exceed `10m`.

The subagent can provide only declared variables. A missing `flag` defaults to `--<variable-name>`, with underscores changed to hyphens. `required` variables must be provided unless they have a `default`. `values` restricts accepted values. `value` is preset by the definition and cannot be overridden by the subagent. Do not put secrets in preset values.

The tool runs the fixed script through `python3` or `python` without a shell. The model cannot choose another path, pass interpreter options, add arbitrary arguments, set environment variables, or change the working directory. Execution uses the normal permission service, configured timeout, context cancellation, and bounded stdout/stderr capture.

A `script` block without the `script` tool, or the `script` tool without a block, is invalid. Script validation errors remain scoped to that custom agent and surface when that agent type is invoked.

## Creating a definition

1. Determine the requested scope. If scope materially affects the request and is unspecified, ask for project or user scope.
2. Resolve and validate the exact provider/model and tools.
3. Check the target directory for an existing definition with the requested name.
4. Do not overwrite an existing file unless the user explicitly asked to edit it.
5. Write complete strict frontmatter and a non-empty instruction body.
6. Read the resulting file and report its absolute path and effective scope.

Use the Commands menu's **Create Agent Definition** action when the user wants the guided form. It creates the scoped file with all selected frontmatter fields and returns its path. The form can enable the bounded script tool and collect its path, timeout, and variables as a one-line JSON object. Enabling it automatically adds `script` to a non-wildcard tool list. The user or agent can then replace the instruction body.

## Editing a definition

Read the complete file before editing. Preserve unrelated fields and instructions. Apply only the requested changes, then verify the whole file against the format and validation rules above. If changing `name`, ensure the filename and precedence implications remain clear; do not rename files unless the user requested it.

The running workspace reads user and project definitions again before each model step and each selected agent invocation. Manual edits, guided creation, project overrides, and deletions take effect without restarting or reloading the client. A definition changed during an active model step appears on the next step; malformed definitions remain isolated and report their validation error only when selected.
