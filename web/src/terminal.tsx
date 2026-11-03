import { createEffect, createSignal, For, on, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { exitText, parseTerminalExit, resizeMessage, terminalLabel, terminalURL, type TerminalTarget, type TmuxSession } from './terminal-protocol.ts';
import { errorText, Icon } from './ui.tsx';

type TerminalHandle = { fit: () => void; focus: () => void };

function cssValue(name: string, fallback: string) {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return value || fallback;
}

export function TerminalView(props: { client: CruxState; workspaceID: string; connection: string; target: TerminalTarget; active: boolean }) {
  let host!: HTMLDivElement;
  let handle: TerminalHandle | undefined;
  const [status, setStatus] = createSignal<'loading' | 'connecting' | 'open' | 'ended'>('loading');
  const [message, setMessage] = createSignal('');
  onMount(() => {
    let disposed = false;
    let socket: WebSocket | undefined;
    let observer: ResizeObserver | undefined;
    let dispose: (() => void) | undefined;
    void (async () => {
      try {
        const [{ Terminal }, { FitAddon }] = await Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit'), import('@xterm/xterm/css/xterm.css')]);
        if (disposed) return;
        const terminal = new Terminal({
          cursorBlink: true, scrollback: 5000, fontSize: 13, allowProposedApi: false,
          fontFamily: cssValue('--mono', 'ui-monospace, SFMono-Regular, Menlo, monospace'),
          theme: { background: cssValue('--surface', '#151619'), foreground: cssValue('--text', '#e6e6e6'), cursor: cssValue('--accent', '#7aa2ff') },
        });
        const fit = new FitAddon();
        terminal.loadAddon(fit);
        terminal.open(host);
        if (host.offsetParent) fit.fit();
        handle = { fit: () => { if (host.offsetParent) fit.fit(); }, focus: () => terminal.focus() };
        dispose = () => terminal.dispose();
        setStatus('connecting');
        const url = new URL(terminalURL(location.origin, props.workspaceID, props.target, terminal.cols, terminal.rows));
        url.pathname = props.client.api.endpoint(url.pathname, props.connection);
        url.searchParams.set('client_id', props.client.api.clientID);
        socket = new WebSocket(url);
        socket.binaryType = 'arraybuffer';
        const encoder = new TextEncoder();
        const send = (data: string | Uint8Array<ArrayBuffer>) => { if (socket?.readyState === WebSocket.OPEN) socket.send(data); };
        socket.onopen = () => { if (!disposed) { setStatus('open'); if (props.active) terminal.focus(); } };
        socket.onmessage = ({ data }) => {
          if (typeof data !== 'string') { terminal.write(new Uint8Array(data as ArrayBuffer)); return; }
          try { setMessage(exitText(parseTerminalExit(data))); }
          catch (reason) { setMessage(errorText(reason)); }
          setStatus('ended');
        };
        socket.onclose = () => {
          if (disposed || status() === 'ended') return;
          const opened = status() === 'open';
          setStatus('ended');
          setMessage(opened ? 'The terminal connection closed.' : 'The terminal could not be opened. Check that the workspace is still open.');
        };
        terminal.onData(value => send(encoder.encode(value)));
        terminal.onBinary(value => send(Uint8Array.from(value, character => character.charCodeAt(0) & 255)));
        terminal.onResize(({ cols, rows }) => send(resizeMessage(cols, rows)));
        observer = new ResizeObserver(() => handle?.fit());
        observer.observe(host);
      } catch (reason) {
        if (!disposed) { setStatus('ended'); setMessage(`The terminal could not be loaded: ${errorText(reason)}`); }
      }
    })();
    onCleanup(() => { disposed = true; observer?.disconnect(); socket?.close(); dispose?.(); });
  });
  createEffect(() => { if (props.active) requestAnimationFrame(() => { handle?.fit(); handle?.focus(); }); });
  return <div class="terminal-view" hidden={!props.active} aria-label={terminalLabel(props.target)}>
    <div class="terminal-surface" ref={host} />
    <Show when={status() !== 'open'}><p class="terminal-status" role={status() === 'ended' ? 'alert' : 'status'}>{status() === 'loading' ? 'Loading terminal…' : status() === 'connecting' ? 'Connecting…' : message()}</p></Show>
  </div>;
}

export type TaskTerminalHandle = { write: (data: Uint8Array, reset: boolean) => void };

export function TaskLogTerminal(props: { ready: (handle: TaskTerminalHandle | undefined) => void }) {
  let host!: HTMLDivElement;
  const [error, setError] = createSignal('');
  onMount(() => {
    let disposed = false;
    let observer: ResizeObserver | undefined;
    let dispose: (() => void) | undefined;
    void (async () => {
      try {
        const [{ Terminal }, { FitAddon }] = await Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit'), import('@xterm/xterm/css/xterm.css')]);
        if (disposed) return;
        const terminal = new Terminal({
          disableStdin: true, cursorBlink: false, scrollback: 100000, fontSize: 13, convertEol: true,
          fontFamily: cssValue('--mono', 'ui-monospace, SFMono-Regular, Menlo, monospace'),
          theme: { background: cssValue('--surface', '#151619'), foreground: cssValue('--text', '#e6e6e6') },
        });
        const fit = new FitAddon();
        terminal.loadAddon(fit);
        terminal.open(host);
        const resize = () => { if (host.offsetParent) fit.fit(); };
        resize();
        observer = new ResizeObserver(resize);
        observer.observe(host);
        dispose = () => terminal.dispose();
        props.ready({ write: (data, reset) => {
          const buffer = terminal.buffer.active;
          const following = buffer.viewportY >= buffer.baseY;
          const position = buffer.viewportY;
          if (reset) terminal.reset();
          terminal.write(data, () => { if (!disposed) { if (following) terminal.scrollToBottom(); else terminal.scrollToLine(position); } });
        } });
      } catch (reason) { if (!disposed) setError(errorText(reason)); }
    })();
    onCleanup(() => { disposed = true; props.ready(undefined); observer?.disconnect(); dispose?.(); });
  });
  return <div class="task-terminal" role="region" aria-label="Task terminal output">
    <div class="terminal-surface" ref={host} />
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
  </div>;
}

export function TmuxPicker(props: { client: CruxState; choose: (target: TerminalTarget) => void; close: () => void }) {
  const [sessions, setSessions] = createSignal<TmuxSession[]>();
  const [error, setError] = createSignal('');
  let loadedWorkspaceID: string | undefined;
  let loadedConnection: string | undefined;
  createEffect(on(() => [props.client.state.workspace?.id, props.client.state.workspace?.connection_name, props.client.state.connected] as const, ([workspaceID, , connected]) => {
    setSessions(undefined);
    setError('');
    loadedWorkspaceID = undefined;
    loadedConnection = undefined;
    if (!workspaceID || !connected) return;
    const connection = props.client.api.connection;
    const controller = new AbortController();
    const current = () => !controller.signal.aborted && props.client.state.connected && props.client.state.workspace?.id === workspaceID && props.client.api.connection === connection;
    void props.client.api.request<TmuxSession[]>('/ui/tmux-sessions', 'GET', undefined, controller.signal)
      .then(value => {
        if (!current()) return;
        loadedWorkspaceID = workspaceID;
        loadedConnection = connection;
        setSessions(value ?? []);
      })
      .catch(reason => { if (current()) setError(errorText(reason)); });
    onCleanup(() => controller.abort());
  }));
  return <div class="tmux-picker" role="dialog" aria-label="Attach to a tmux session">
    <div class="actions"><strong>Crux tmux sessions</strong><span class="spacer" /><button class="ghost icon" aria-label="Close tmux sessions" onClick={props.close}><Icon name="close" size={13} /></button></div>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <Show when={sessions()} fallback={<Show when={!error()}><p class="muted small-text">Loading sessions…</p></Show>}>
      <For each={sessions()} fallback={<p class="muted small-text">No Crux tmux sessions are running.</p>}>{session => <button class="list-item" onClick={() => { if (props.client.state.connected && props.client.state.workspace?.id === loadedWorkspaceID && props.client.api.connection === loadedConnection) props.choose({ kind: 'tmux', socket: session.socket, id: session.id, name: session.name }); }}>
        <Icon name="terminal" size={14} /><span><strong>{session.name}</strong><small>{session.windows} window{session.windows === 1 ? '' : 's'} · {session.attached} attached{session.socket ? ` · ${session.socket}` : ''}</small></span>
      </button>}</For>
    </Show>
  </div>;
}
