Capture HTTPS traffic from a user-specified executable or a safely relaunched process in an isolated tmux session backed by Crux's embedded mitmproxy runtime.

<usage>
- Exactly one of executable or pid is required
- Arguments are passed directly to executable without shell parsing
- PID capture relaunches the detected command and does not modify the original process
- The capture file must be a new .mitm path; existing files are never overwritten
- Every capture requires explicit user approval because decrypted traffic may contain credentials or private data
- The tool returns bounded launch metadata, not captured request or response bodies
- The authenticated viewer binds only to loopback and returns a directly usable tokenized URL that remains valid only while the capture is running
- The selected executable and arguments run silently in the background as `main`; no wrapper command or alternate target is required
- Use the Crux tmux sessions menu to attach to an independent management shell, not to the target's terminal
- In that shell, use `cap list`, `cap status main`, `cap logs main -f`, `cap stop main`, and `cap restart main`; `cap help` lists all controls
- `cap start NAME -- EXECUTABLE [ARGS]` starts another quiet process with the shell's directory and environment; `cap send NAME TEXT` sends a line to a process's private terminal
- Ctrl+C stops log following without stopping the target. F12 or Ctrl+B then D detaches without stopping the shell or processes
- Target exit does not close the shell or viewer. Exiting the management shell stops its processes and ends the capture
- `wait: true` waits for the initially selected target to finish, not for the management shell to close; its exit status is reported while the shell and viewer remain available
- Target stdout/stderr is retained in private log files, separate from the management shell's pane log
- TLS verification is relaxed only inside the opt-in capture: mitmproxy accepts upstream certificates; Node and Git receive verification-disable settings; normal Python startup receives a session-local TLS hook. CA bundles remain available for clients that require them
- Host trust stores and uncaptured processes are unchanged. Certificate-pinned clients, Go/macOS-native TLS without configurable trust, and Python isolated/no-site startup may still require application-specific support
- Explicit unset_env entries are applied after capture TLS settings
</usage>

<tips>
- Build Crux with `--embedded-mitmproxy` to include traffic capture support
- Native per-app WireGuard capture currently requires macOS, tmux, and the Swift compiler/SDK available through xcrun. The launcher and packet provider are signed ad hoc; macOS may request approval of the per-app VPN configuration
- The capture app and its spawned helpers route through the embedded Go WireGuard client; the proxy runs outside that app rule to avoid routing loops. There is no proxy-environment fallback when native policy activation fails
- Use unset_env to remove proxy or provider variables that should not reach the relaunched target
- Set wait only for targets expected to terminate; otherwise the tool returns after startup
- Store and share .mitm files as sensitive artifacts
</tips>
