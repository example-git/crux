---
name: crux-ui-info
description: Use when inspecting Crux UI visuals or using the UI preview screenshot and control API.
---

Fetch `GET /help` on the running preview server using the fetch tool, for example `http://127.0.0.1:8767/help`. Use the user's supplied address when different.

If no server is running, launch `crux demo --port 8767` with the bash tool's `run_in_background: true`, then fetch `/help`. Reuse an existing server rather than restarting it.

The endpoint returns a Markdown index; follow its links for usage and generated schema references. No browser is needed to read the help.
