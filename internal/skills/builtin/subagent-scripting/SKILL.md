---
name: subagent-scripting
description: Use when coordinating Crux subagents programmatically with the subagent_script tool, crux subagent CLI, JavaScript orchestration, or Python scripts, including concurrent tasks, session-bound environment variables, output retrieval, stopping, and transcript continuation.
---

# Subagent Scripting

Use the existing managed-subagent runtime through either the `subagent_script` tool or `crux subagent`. Both interfaces preserve task IDs, child transcripts, parent-session ownership, preset configuration, and completion notifications.

## Choose the entry point

- Use `subagent_script` for orchestration directly from the main agent. Pass JavaScript in its `code` parameter.
- Use `crux subagent` from a Crux-launched Bash command or configured Python script when the surrounding workflow already uses shell commands, files, or Python.
- Use `crux subagent script workflow.js` to execute a JavaScript file through the same session-bound CLI. Use `-` instead of a filename to read stdin.

The JavaScript runtime is not Node.js. It provides standard JavaScript, the `subagents` API, `env`, and `print`, without filesystem access, modules, or arbitrary subprocess execution. API functions are synchronous; use ordinary loops and array methods, not `async`/`await` or `Promise.all`.

## Session binding

Crux supplies these variables to Bash commands and configured Python scripts:

| Variable | Meaning |
| --- | --- |
| `CRUX_SESSION_ID` | The session invoking the command or script. |
| `CRUX_PARENT_SESSION_ID` | The parent of a child session; empty for a top-level session. |
| `CRUX_SUBAGENT_URL` | The owning coordinator's loopback API address. |
| `CRUX_SUBAGENT_TOKEN` | An opaque capability bound to that session. |

Inherit this environment unchanged when invoking the CLI, including from Python subprocesses. Do not print the token, save it in task files, invent missing values, or copy another session's binding. The CLI has no `--session` override and rejects workspace/connection override flags. Changing directories does not change the bound session.

The API checks the session/token pair and task ownership. Child-agent shells and scripts receive a child-scoped binding, not the parent's capability: they can inspect their own task, not siblings or unrelated sessions. Recursive subagent launch remains unavailable to child agents.

The API remains available after a shell detaches, but closes with its owning coordinator. A missing or stale binding must be resolved by invoking the command from the intended live Crux session, not by selecting a global or most-recent session.

## Orchestration workflow

1. Call `subagents.types()` or `crux subagent types` before selecting a named preset. Omit the type for the generic task agent, which uses the parent session's model configuration. A named preset retains its configured provider, model, and tools; an invalid explicit preset fails rather than falling back.
2. On resumption, list existing tasks and inspect retained task IDs before launching replacements. Reuse active work instead of submitting duplicates.
3. Give each independent task a concrete prompt and expected result. Start all independent tasks before waiting so they can run concurrently. Launch dependent tasks only after examining the prerequisite results.
4. Retain each returned task `id`, `child_session_id`, and its assignment. `start` confirms launch, not completion.
5. Read `output` or `wait`, then inspect `task.state.status`, `output`, and any error fields. A successful CLI exit or a ready output response is not proof that the task succeeded.
6. Use `continue` to extend a terminal task's transcript. It returns a new task ID while retaining the child transcript. Track the new ID separately.
7. Use `stop` for tasks that should no longer run. Ending a script or CLI client does not implicitly stop already-launched tasks.

## JavaScript API

| Call | Behavior |
| --- | --- |
| `subagents.types()` | List available generic and named presets. |
| `subagents.list()` | List tasks visible to the bound session. |
| `subagents.start({prompt, subagent_type})` | Start a background task and return its task object; type is optional. |
| `subagents.output(id)` | Read the current task output without waiting. |
| `subagents.wait(id)` | Wait until terminal or caller cancellation. |
| `subagents.wait(id, {timeout_ms: 30000})` | Wait up to the explicit limit and return the current result. |
| `subagents.stop(id)` | Stop the task and return its state. |
| `subagents.continue(id, prompt)` | Start a new task continuing a terminal task's transcript. |

Pass this JavaScript as the `code` value of `subagent_script`, or save it for the CLI's `script` command:

```javascript
const prompts = [
  "Review parser behavior and report concrete defects",
  "Review error handling and report concrete defects"
];
const tasks = prompts.map(prompt => subagents.start({prompt}));
print({session: env.CRUX_SESSION_ID, task_ids: tasks.map(task => task.id)});
return tasks.map(task => subagents.wait(task.id, {timeout_ms: 30000}));
```

This starts both tasks before waiting. Either returned task may still be running after its bounded wait; retain its ID and retrieve it again. Do not treat the number of returned results as a completed-task count.

The final script response contains `result`, optional `output` from `print(value)`, and `tasks` recording successful starts and continuations. Started task IDs remain in `tasks` if later script code fails. `env` exposes only the session and parent-session IDs, not the capability token.

## CLI and JSON requests

All successful CLI responses are JSON. Use actual IDs returned by `start` or `list` in place of the illustrative ID below:

```sh
crux subagent types
crux subagent list
crux subagent start "Review the parser"
crux subagent start --prompt-file task.txt
crux subagent output a12345678
crux subagent wait a12345678 --timeout 30s
crux subagent stop a12345678
crux subagent continue a12345678 "Check the remaining edge cases"
```

Select a discovered preset with `--type <name>`. `--prompt-file -` reads the exact prompt from stdin; do not combine prompt-file input with positional prompt text.

For structured requests, send one JSON object to `crux subagent call`:

```sh
printf '%s\n' '{"action":"start","prompt":"Review the parser"}' | crux subagent call
```

The accepted fields are `action`, `task_id`, `prompt`, `subagent_type`, and `timeout_ms`. Actions are `types`, `list`, `start`, `output`, `wait`, `stop`, and `continue`. Unknown fields and fields inapplicable to the chosen action are rejected. Session IDs are not request parameters.

Python can inherit the binding and call the JSON interface directly:

```python
import json
import subprocess

def subagent(request):
    response = subprocess.run(
        ["crux", "subagent", "call"],
        input=json.dumps(request),
        text=True, capture_output=True, check=True,
    )
    return json.loads(response.stdout)

prompts = ["Review parser behavior", "Review error handling"]
tasks = []
for prompt in prompts:
    task = subagent({"action": "start", "prompt": prompt})
    tasks.append(task)
    print(json.dumps({"started": task}), flush=True)

for task in tasks:
    result = subagent({"action": "wait", "task_id": task["id"], "timeout_ms": 30000})
    print(json.dumps(result), flush=True)
```

## Results, timeouts, and recovery

- `start`, `stop`, and `continue` return a task object. `output` and `wait` return an object containing `task`, `output`, and `retrieval_status`.
- Task states `pending` and `running` are nonterminal. `completed`, `failed`, `killed`, and `lost` are terminal; only `completed` indicates successful task execution. Inspect the returned work against the requested result separately.
- A task failure can be returned successfully by the CLI. Inspect `task.state.status`, `error_code`, `error_message`, and `lost_reason` inside `task.state`; `subprocess.run(check=True)` only catches command/API failure.
- Omitting the wait timeout waits until terminal. An explicit timeout accepts zero through 600000 whole milliseconds, or CLI durations from `0` through `10m`. A retrieval timeout does not stop the task.
- The enclosing Bash or configured-script tool can have its own timeout. For long tasks, retain IDs and use bounded retrievals, or deliberately detach the orchestration shell when that workflow calls for it. Resume using the same task IDs rather than relaunching work after a caller timeout.
- An ownership rejection means the ID is not accessible through this binding. List this session's tasks and use the correct returned ID; do not alter session variables to reach another session.
- For an unknown preset or unavailable provider/model, inspect `types` and the selected configuration. Do not silently choose a different preset, provider, or model.
- If an orchestration script fails after starting tasks, recover the `tasks` array, inspect those tasks, and continue or stop them as appropriate before submitting replacements.

For creating or changing named presets, load the `custom-agents` skill. Preset definition and orchestration are separate operations.
