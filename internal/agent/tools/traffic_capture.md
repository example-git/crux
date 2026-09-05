Capture HTTPS traffic from a user-specified executable or a safely relaunched process in an isolated tmux session backed by Crux's embedded mitmproxy runtime.

<usage>
- Exactly one of executable or pid is required
- Arguments are passed directly to executable without shell parsing
- PID capture relaunches the detected command and does not modify the original process
- The capture file must be a new .mitm path; existing files are never overwritten
- Every capture requires explicit user approval because decrypted traffic may contain credentials or private data
- The tool returns bounded launch metadata, not captured request or response bodies
- The authenticated viewer binds only to loopback and returns a directly usable tokenized URL that remains valid only while the capture is running
- Use the Crux tmux sessions menu to attach to the live capture and detach with the shortcut shown in tmux's status bar
</usage>

<tips>
- Build Crux with `--embedded-mitmproxy` to include traffic capture support
- Use unset_env to remove proxy or provider variables that should not reach the relaunched target
- Set wait only for targets expected to terminate; otherwise the tool returns after startup
- Store and share .mitm files as sensitive artifacts
</tips>
