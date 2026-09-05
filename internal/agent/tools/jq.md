Run Crux's native gojq evaluator directly without invoking Bash or requiring an external jq executable.

<usage>
- Accepts the same jq modes exposed by Crux's shell builtin: raw, joined, compact, slurped, null, exit-status, and raw input
- `input` contains a serialized JSON stream, or line-oriented text when `raw_input` is true
- `files` processes one or more files in order; paths outside the workspace require read permission
- `args` binds string variables and `json_args` binds JSON variables as `$name`
- `filter` defaults to `.`
- Output stops at 30 KiB and is marked as truncated
</usage>

<constraints>
- `input` and `files` are mutually exclusive
- `null_input` cannot be combined with `input` or `files`
- Standard jq features not implemented by Crux's shell builtin remain unavailable, including streaming flags, filter files, slurp/raw files, positional args, and YAML flags
</constraints>

<examples>
- Extract: `{"filter":".name","input":"{\"name\":\"crux\"}"}`
- Construct: `{"null_input":true,"filter":"{host: $host, port: $port}","args":{"host":"localhost"},"json_args":{"port":8080}}`
- Read lines: `{"raw_input":true,"input":"one\ntwo","filter":"ascii_upcase","raw_output":true}`
</examples>
