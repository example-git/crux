# Subagent scripting

Crux exposes the same managed subagents through the `crux subagent` CLI and the `subagent_script` agent tool. Both support starting concurrent tasks, reading results, waiting, stopping, and continuing a completed transcript.

## Session binding

Crux supplies these environment variables to its Bash commands and configured Python scripts:

| Variable | Meaning |
| --- | --- |
| `CRUX_SESSION_ID` | The session invoking the shell or script. |
| `CRUX_PARENT_SESSION_ID` | The parent of a child session; empty for a top-level session. |
| `CRUX_SUBAGENT_URL` | The coordinator's loopback task API. |
| `CRUX_SUBAGENT_TOKEN` | An opaque capability bound to that session. |

Commands inherit this environment. They do not select a session from the working directory, a global default, or the most recently used session. There is no `--session` override. Changing the session ID without its matching capability is rejected, and requests cannot supply another parent session. Each task operation checks ownership on the coordinator.

Child-agent shells and configured scripts receive their own child-session binding, not the parent's capability. They can inspect their own task, not sibling tasks or tasks belonging to another parent. Existing restrictions on subagents launching further subagents remain in effect.

The API lives for the coordinator's lifetime, including after a shell is detached. A stale environment fails after that coordinator closes. Tokens are not stored in task records or printed in command responses. This is task-API scoping, not an operating-system sandbox.

## CLI

Run these commands from a Crux-launched shell or script:

```sh
crux subagent types
crux subagent list
crux subagent start "Review the parser"
crux subagent start --type reviewer --prompt-file task.txt
crux subagent output a12345678
crux subagent wait a12345678
crux subagent wait a12345678 --timeout 30s
crux subagent stop a12345678
crux subagent continue a12345678 "Check the edge cases too"
```

`start` returns immediately with a JSON task object, including `id`, `child_session_id`, `ownership`, and `state`. The generic task agent uses the invoking session's model configuration. Named presets retain their configured model and tools. Results and completion notifications use the existing managed-task pipeline.

`output` and `wait` return a JSON output object containing `task`, `output`, and `retrieval_status`. Without `--timeout`, `wait` blocks until the task becomes terminal or the caller cancels. An explicit timeout must be whole milliseconds between zero and ten minutes; a timed-out retrieval returns the current task state without stopping the task. Task failure is represented in `task.state`; malformed requests and API failures exit nonzero.

Use `--prompt-file -` to read an exact prompt from stdin. Use `call` for a JSON request:

```sh
printf '%s\n' '{"action":"start","prompt":"Review the parser","subagent_type":"reviewer"}' | crux subagent call
```

The request fields are `action`, `task_id`, `prompt`, `subagent_type`, and `timeout_ms`. Unknown fields and fields inapplicable to the selected action are rejected. Supported actions are `types`, `list`, `start`, `output`, `wait`, `stop`, and `continue`.

Python can use the same CLI without constructing credentials or choosing a session:

```python
import json
import subprocess

def crux(request):
    result = subprocess.run(
        ["crux", "subagent", "call"],
        input=json.dumps(request), text=True, capture_output=True, check=True,
    )
    return json.loads(result.stdout)

tasks = [crux({"action": "start", "prompt": prompt}) for prompt in tasks_to_run]
results = [crux({"action": "wait", "task_id": task["id"]}) for task in tasks]
```

## JavaScript orchestration

Pass JavaScript in the `code` parameter of the `subagent_script` agent tool, or save it and run `crux subagent script workflow.js`. A filename of `-` reads stdin.

```javascript
const tasks = [
  "Review parser behavior",
  "Review error handling"
].map(prompt => subagents.start({prompt}));

print({session: env.CRUX_SESSION_ID, task_ids: tasks.map(task => task.id)});
return tasks.map(task => subagents.wait(task.id));
```

Starting all tasks before waiting allows them to run concurrently. Functions are synchronous; use ordinary JavaScript loops and arrays, not `async`/`await`.

| Function | Result |
| --- | --- |
| `subagents.types()` | Available generic and named presets. |
| `subagents.list()` | Tasks visible to the bound session. |
| `subagents.start({prompt, subagent_type})` | Newly started task; `subagent_type` is optional. |
| `subagents.output(id)` | Current task output and status. |
| `subagents.wait(id, {timeout_ms})` | Task output after completion or the optional timeout. |
| `subagents.stop(id)` | Updated task state. |
| `subagents.continue(id, prompt)` | New task continuing a terminal task's transcript. |

`env` exposes the session and parent-session IDs, not the capability token. `print(value)` retains structured output. The final JSON response contains `result`, optional `output`, and a `tasks` array recording tasks started or continued by the script. That array is retained if later script code fails, so already-started task IDs remain available.

The JavaScript runtime provides the task API and standard JavaScript, not Node.js modules, filesystem access, or arbitrary subprocess execution. Stopping a script or closing a CLI client does not implicitly stop tasks it already launched; use `stop` for those tasks.
