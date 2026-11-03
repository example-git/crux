import { createEffect, createSignal, For, on, onCleanup, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { terminalLabel, type TerminalTarget } from './terminal-protocol.ts';
import { TerminalView, TmuxPicker } from './terminal.tsx';
import { errorText, Icon, Kbd } from './ui.tsx';
import type { Command } from './palette.tsx';
import { delegateEvents } from 'solid-js/web';
import { taskActive, TasksPanel } from './tasks.tsx';

type Entry = { command: string; output: string; exit: number; path: string };
type OpenTerminal = { id: number; workspaceID: string; connection: string; target: TerminalTarget };
let terminalID = 0;

export function ShellDock(props: { client: CruxState; collapsed: boolean; expand: () => void; close: () => void; detached?: (value: boolean) => void; resize?: (height: number) => void; commands?: Command[]; taskRequest: number; selectedTask?: string }) {
  const { client } = props;
  const [command, setCommand] = createSignal('');
  const [running, setRunning] = createSignal(false);
  const [entries, setEntries] = createSignal<Entry[]>([]);
  const [past, setPast] = createSignal<string[]>([]);
  const [terminals, setTerminals] = createSignal<OpenTerminal[]>([]);
  const [tab, setTab] = createSignal(0);
  const [picking, setPicking] = createSignal(false);
  const [popped, setPopped] = createSignal(false);
  let mount!: HTMLDivElement;
  let panel!: HTMLElement;
  let popup: Window | undefined;
  let styleObserver: MutationObserver | undefined;
  let sheetObserver: MutationObserver | undefined;
  const dock = (closeWindow = true, expand = true) => {
    if (!popup) return;
    const previous = popup;
    popup = undefined;
    styleObserver?.disconnect();
    styleObserver = undefined;
    sheetObserver?.disconnect();
    sheetObserver = undefined;
    mount.append(panel);
    setPopped(false);
    props.detached?.(false);
    if (expand) props.expand();
    previous.removeEventListener('beforeunload', returnPanel);
    if (closeWindow && !previous.closed) previous.close();
    window.dispatchEvent(new Event('resize'));
  };
  const returnPanel = () => dock(false);
  const popOut = () => {
    if (popup && !popup.closed) { popup.focus(); return; }
    const child = window.open('about:blank', 'crux-task-panel', 'popup,width=900,height=600');
    if (!child) { client.fail(new Error('The task window was blocked. Allow pop-ups to detach the panel.')); return; }
    popup = child;
    child.document.title = 'Crux tasks and terminal';
    const base = child.document.createElement('base');
    base.href = document.baseURI;
    child.document.head.append(base);
    let sheets: Node[] = [];
    const syncSheets = () => {
      for (const node of sheets) node.parentNode?.removeChild(node);
      sheets = [...document.querySelectorAll('link[rel="stylesheet"], style')].map(node => node.cloneNode(true));
      child.document.head.append(...sheets);
    };
    syncSheets();
    sheetObserver = new MutationObserver(syncSheets);
    sheetObserver.observe(document.head, { childList: true, subtree: true, characterData: true });
    const syncStyle = () => {
      child.document.documentElement.className = `${document.documentElement.className} task-popout`;
      child.document.documentElement.style.cssText = document.documentElement.style.cssText;
    };
    syncStyle();
    styleObserver = new MutationObserver(syncStyle);
    styleObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['style', 'class'] });
    delegateEvents(['click', 'input', 'change', 'keydown', 'keyup', 'submit', 'pointerdown', 'pointermove', 'pointerup', 'focusin', 'focusout'], child.document);
    child.document.body.append(panel);
    child.addEventListener('beforeunload', returnPanel);
    setPopped(true);
    props.detached?.(true);
    props.expand();
    child.focus();
    window.dispatchEvent(new Event('resize'));
  };
  onCleanup(() => dock(true, false));
  let resizing: { y: number; height: number } | undefined;
  const resize = (height: number) => props.resize?.(Math.max(220, Math.min(window.innerHeight * .8, height)));
  const activeTasks = () => client.state.tasks.filter(taskActive).length;
  createEffect(on(() => props.taskRequest, value => { if (value) setTab(-1); }));
  const openTerminal = (target: TerminalTarget) => {
    const workspaceID = client.state.workspace?.id;
    if (!workspaceID) return;
    const id = ++terminalID;
    setTerminals(items => [...items, { id, workspaceID, connection: client.api.connection, target }]);
    setTab(id); setPicking(false); props.expand();
  };
  const closeTerminal = (id: number) => {
    setTerminals(items => items.filter(item => item.id !== id));
    if (tab() === id) setTab(terminals().at(-1)?.id ?? 0);
  };
  let position = 0;
  let log!: HTMLDivElement;
  let active: AbortController | undefined;
  onCleanup(() => active?.abort());
  async function run() {
    const value = command().trim();
    const workspace = client.state.workspace;
    if (!value || running() || !workspace || !client.state.connected) return;
    const workspaceID = workspace.id;
    const path = workspace.path;
    const root = new URL(client.api.route(''), location.origin).href;
    const connection = client.api.connection;
    const current = () => client.state.workspace?.id === workspaceID && client.api.connection === connection;
    const controller = new AbortController();
    active = controller;
    setRunning(true);
    setPast(items => [...items, value]); position = past().length;
    try {
      const id = client.state.sessionID || await client.newSession();
      controller.signal.throwIfAborted();
      if (!current()) throw new Error('The workspace changed before the command could start.');
      const result = await client.api.request<{ output: string; exit_code: number }>(`${root}/agent/sessions/${id}/shell`, 'POST', { command: value, term_width: 120 }, controller.signal);
      setEntries(items => [...items, { command: value, output: result.output, exit: result.exit_code, path }]);
      setCommand('');
      if (current()) await client.refreshSessions().catch(client.fail);
      if (current() && client.state.sessionID === id) await client.loadMessages(id).catch(client.fail);
    } catch (reason) { setEntries(items => [...items, { command: value, output: controller.signal.aborted ? 'Cancellation requested. Partial output may not have been saved to the session.' : errorText(reason), exit: -1, path }]); }
    finally { if (active === controller) active = undefined; setRunning(false); requestAnimationFrame(() => { if (log.isConnected) log.scrollTop = log.scrollHeight; }); }
  }
  return <div class="shell-mount" ref={mount}><Show when={popped()}><button class="shell-return" onClick={() => dock()}>Return tasks and terminal to this window</button></Show><section ref={panel} class="shell" aria-label="Shell" classList={{ 'has-terminal': tab() !== 0 }}>
    <Show when={!props.collapsed && !popped()}><div class="shell-resize" role="separator" aria-label="Resize task and terminal panel" aria-orientation="horizontal" tabIndex={0}
      onPointerDown={event => { resizing = { y: event.clientY, height: panel.getBoundingClientRect().height }; event.currentTarget.setPointerCapture(event.pointerId); event.preventDefault(); }}
      onPointerMove={event => { if (resizing) resize(resizing.height + resizing.y - event.clientY); }}
      onPointerUp={event => { resizing = undefined; event.currentTarget.releasePointerCapture(event.pointerId); }} onPointerCancel={() => { resizing = undefined; }}
      onKeyDown={event => { if (event.key === 'ArrowUp' || event.key === 'ArrowDown') { event.preventDefault(); resize(panel.getBoundingClientRect().height + (event.key === 'ArrowUp' ? 20 : -20)); } }} /></Show>
    <header>
      <div class="shell-tabs" role="tablist" aria-label="Shell views">
        <button role="tab" aria-selected={tab() === 0} onClick={() => { setTab(0); props.expand(); }}><Icon name="terminal" size={13} />Commands</button>
        <button role="tab" aria-selected={tab() === -1} onClick={() => { setTab(-1); props.expand(); }}>Tasks{activeTasks() ? ` · ${activeTasks()}` : ''}</button>
        <button role="tab" aria-selected={tab() === -2} onClick={() => { setTab(-2); props.expand(); }}>Help</button>
        <For each={terminals()}>{item => <span class="shell-tab" classList={{ stale: item.workspaceID !== client.state.workspace?.id || item.connection !== client.api.connection }}>
          <button role="tab" aria-selected={tab() === item.id} onClick={() => { setTab(item.id); props.expand(); }}>{terminalLabel(item.target)}</button>
          <button class="ghost icon" aria-label={`Close ${terminalLabel(item.target)}`} onClick={() => closeTerminal(item.id)}><Icon name="close" size={11} /></button>
        </span>}</For>
        <button class="ghost small shell-action" disabled={!client.state.workspace} onClick={() => openTerminal({ kind: 'shell' })}><Icon name="plus" size={12} />Terminal</button>
        <button class="ghost small shell-action" disabled={!client.state.workspace} onClick={() => setPicking(!picking())}>Attach tmux…</button>
      </div>
      <span class="spacer" />
      <Show when={!props.collapsed && tab() === 0}><button class="ghost small" onClick={() => setEntries([])}>Clear</button></Show>
      <button class="ghost small" onClick={() => popped() ? dock() : popOut()}>{popped() ? 'Dock panel' : 'Pop out'}</button>
      <button class="ghost icon" aria-label={props.collapsed ? 'Expand shell' : 'Collapse shell'} aria-expanded={!props.collapsed} onClick={() => props.collapsed ? props.expand() : props.close()}><Icon name="down" class={props.collapsed ? 'flip' : undefined} size={14} /></button>
    </header>
    <div class="shell-body" inert={props.collapsed}>
    <Show when={tab() === -1 && !props.collapsed}><div class="shell-tasks"><TasksPanel client={client} selected={props.selectedTask} /></div></Show>
    <Show when={tab() === -2 && !props.collapsed}><div class="shell-help">
      <h3>Keyboard shortcuts</h3>
      <p class="muted small-text">Use ⌘ on macOS or Ctrl elsewhere for application shortcuts. Composer shortcuts apply while typing.</p>
      <div class="shell-help-keys">
        <span>Command palette</span><Kbd>⌘K</Kbd>
        <span>Toggle side panel</span><Kbd>⌘B / ⌘.</Kbd>
        <span>Send / queue / steer</span><Kbd>Enter</Kbd>
        <span>New line</span><Kbd>Shift+Enter</Kbd>
        <span>Toggle plan mode</span><Kbd>Shift+Tab</Kbd>
        <span>Accept suggestion</span><Kbd>Tab</Kbd>
        <span>Prompt history at text boundaries</span><Kbd>↑ / ↓</Kbd>
        <span>Stop turn with an empty composer</span><Kbd>Esc</Kbd>
      </div>
      <h3>Using the workspace</h3>
      <p>Queue saves a follow-up for the next turn. Steer redirects the active turn. Use the interaction-mode selector to change plan mode or tool auto-approval while working.</p>
      <p>Type <code>/</code> for commands or <code>!command</code> to run a shell command. Use Terminal for interactive programs, and Tasks to inspect agent or command output and message a child agent.</p>
      <h3>Commands</h3>
      <div class="shell-help-commands"><For each={props.commands ?? []}>{item => <button class="ghost" onClick={() => item.run()}><span><strong>{item.name}</strong><Show when={item.detail}><small>{item.detail}</small></Show></span><Show when={item.shortcut}><Kbd>{item.shortcut!}</Kbd></Show></button>}</For></div>
    </div></Show>
    <Show when={picking()}><TmuxPicker client={client} choose={openTerminal} close={() => setPicking(false)} /></Show>
    <For each={terminals()}>{item => <TerminalView client={client} workspaceID={item.workspaceID} connection={item.connection} target={item.target} active={!props.collapsed && tab() === item.id} />}</For>
    <div class="shell-commands" hidden={tab() !== 0}>
    <p class="muted small-text shell-note">Runs in {client.state.workspace?.path} · output is saved to the session</p>
    <div class="shell-log" ref={log} role="log">
      <For each={entries()} fallback={<p class="muted">Each command runs to completion and is saved to the session. Open a Terminal tab for interactive programs. Tip: type <code>!command</code> in the composer to do the same.</p>}>{entry => <div class="shell-entry-row">
        <div class="muted small-text">{entry.path}</div>
        <div class="shell-cmd"><span>$ {entry.command}</span><span classList={{ bad: entry.exit !== 0 }}>{entry.exit === -1 ? 'error' : `exit ${entry.exit}`}</span></div>
        <pre>{entry.output || '(no output)'}</pre>
      </div>}</For>
      <Show when={running()}><div class="actions"><p class="muted pulse">Running…</p><span class="spacer" /><button class="danger" onClick={() => active?.abort()}>Stop command</button></div></Show>
    </div>
    <form onSubmit={event => { event.preventDefault(); void run(); }}>
      <span class="prompt">$</span>
      <input aria-label="Shell command" value={command()} disabled={running() || !client.state.connected} placeholder="Run a command…" onInput={event => setCommand(event.currentTarget.value)} onKeyDown={event => {
        if (!['ArrowUp', 'ArrowDown'].includes(event.key) || !past().length) return;
        event.preventDefault();
        position = Math.max(0, Math.min(past().length, position + (event.key === 'ArrowUp' ? -1 : 1)));
        setCommand(past()[position] ?? '');
      }} />
      <button class="primary" disabled={running() || !command().trim() || !client.state.connected}>Run</button>
    </form>
    </div>
    </div>
  </section></div>;
}
