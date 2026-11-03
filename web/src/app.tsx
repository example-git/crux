import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from 'solid-js';
import { createCruxState, isPlanMode, type CruxState } from './state.ts';
import { MessageView, messageText, pairResults } from './conversation.tsx';
import { taskNotification } from './notifications.ts';
import { Composer, type ComposerHandle } from './composer.tsx';
import { Decisions } from './decisions.tsx';
import { defaultSections, Panel, type PanelSection } from './panel.tsx';
import { ShellDock } from './shell.tsx';
import { Settings, type SettingsSection } from './settings.tsx';
import { Palette, type Command } from './palette.tsx';
import { PathTranslation } from './privacy.tsx';
import { taskActive } from './tasks.tsx';
import { activeBrand, applyBrand, brandFor, Wordmark } from './brand.tsx';
import { readStored, writeStored } from './storage.ts';
import { errorText, Icon, Modal, pageHidden, Toasts } from './ui.tsx';
import type { Message, Session, Workspace } from './types.ts';
import { DesktopProjectSidebar, type DesktopProjects } from './desktop-projects.tsx';

export function sessionMarkdown(title: string, messages: Message[]) {
  const fenced = (value: unknown) => {
    const text = JSON.stringify(value, null, 2) ?? 'null';
    const length = (text.match(/`+/g) ?? []).reduce((maximum, run) => Math.max(maximum, run.length + 1), 3);
    const fence = '`'.repeat(length);
    return `${fence}json\n${text}\n${fence}`;
  };
  return `# ${title.replace(/[\r\n]+/g, ' ')}\n\n` + messages.map(message => {
    const label = message.is_summary_message ? 'Conversation summary' : message.role === 'user' ? 'You' : message.role === 'assistant' ? 'Crux' : message.role;
    const notification = taskNotification(message);
    const parts = message.parts.map(part => {
      if (part.type === 'text' && !notification) return String(part.data.text ?? '');
      if (part.type === 'reasoning') return `### Reasoning\n\n${part.data.thinking ?? ''}`;
      return `### ${part.type.replace(/[\r\n]+/g, ' ')}\n\n${fenced(part.data)}`;
    });
    return `## ${notification ? 'Task notification' : label}\n\n${parts.join('\n\n')}`;
  }).join('\n\n') + '\n';
}

declare global { interface Window { cruxDesktop?: { windowControl: (action: 'minimize' | 'maximize' | 'close') => Promise<void>; selectDirectory: () => Promise<string | null>; loadProjects: () => Promise<DesktopProjects>; saveProjects: (value: DesktopProjects) => Promise<DesktopProjects> } } }

export function App(props: { client?: CruxState } = {}) {
  const client = props.client ?? createCruxState();
  const { state, api, attempt } = client;
  const [panel, setPanel] = createSignal(window.innerWidth >= 860);
  const [sections, setSections] = createSignal<Record<PanelSection, boolean>>({ ...defaultSections, ...readStored<Partial<Record<PanelSection, boolean>>>('panel-sections') });
  const toggleSection = (section: PanelSection, open = !sections()[section]) => {
    setSections(value => ({ ...value, [section]: open }));
    writeStored('panel-sections', sections());
  };
  const [selectedTask, setSelectedTask] = createSignal('');
  const [settings, setSettings] = createSignal<SettingsSection | ''>('');
  const [palette, setPalette] = createSignal(false);
  const [shell, setShell] = createSignal(false);
  const [dockDetached, setDockDetached] = createSignal(false);
  const storedDockHeight = readStored<number>('dock-height');
  const [dockHeight, setDockHeight] = createSignal(Number.isFinite(storedDockHeight) ? Math.max(220, storedDockHeight!) : 360);
  const resizeDock = (height: number) => { setDockHeight(height); writeStored('dock-height', height); };
  const [taskRequest, setTaskRequest] = createSignal(0);
  const [privacy, setPrivacy] = createSignal(false);
  const [errors, setErrors] = createSignal(false);
  const [workspaceDialog, setWorkspaceDialog] = createSignal(false);
  const [renaming, setRenaming] = createSignal<Session>();
  const [removing, setRemoving] = createSignal<Session>();
  const [rewinding, setRewinding] = createSignal<Message | true>();
  const [find, setFind] = createSignal(false);
  const [search, setSearch] = createSignal('');
  const [match, setMatch] = createSignal(0);
  const [visible, setVisible] = createSignal(120);
  const [atBottom, setAtBottom] = createSignal(true);
  const [needsInit, setNeedsInit] = createSignal(false);
  const [initializing, setInitializing] = createSignal(false);
  let initGeneration = 0;
  let composer: ComposerHandle | undefined;
  let scroller!: HTMLDivElement;

  const session = () => state.currentSession;
  const busy = () => Boolean(session()?.is_busy);
  const workspaceName = () => state.workspace?.path.split(/[\\/]/).filter(Boolean).pop() ?? 'Workspace';
  const shown = createMemo(() => state.messageIDs.slice(-visible()));
  const pairing = createMemo(() => { state.messageRevision; return pairResults(shown().map(id => state.messages[id]).filter(Boolean)); });
  const matches = createMemo(() => search().trim() ? state.messageIDs.filter(id => state.messages[id] && messageText(state.messages[id]).toLowerCase().includes(search().toLowerCase())) : []);
  const runningTasks = createMemo(() => state.tasks.filter(taskActive).length);
  const toBottom = () => { if (scroller) scroller.scrollTop = scroller.scrollHeight; setAtBottom(true); };
  let openDesktopSessions: (() => void) | undefined;
  const openPanel = (section: PanelSection) => {
    if (section === 'sessions' && window.cruxDesktop) { openDesktopSessions?.(); return; }
    if (section === 'tasks') { setTaskRequest(value => value + 1); setShell(true); return; }
    toggleSection(section, true);
    setPanel(true);
    requestAnimationFrame(() => document.querySelector(`[data-section="${section}"]`)?.scrollIntoView?.({ block: 'start', behavior: 'smooth' }));
  };
  const messageTint = (message: Message) => message.role === 'assistant' && message.provider ? brandFor(state.workspace?.provider_surfaces?.find(item => item.id === message.provider)).accent : undefined;
  const openTask = (id: string) => { setSelectedTask(id); openPanel('tasks'); };
  const newSession = () => attempt(() => client.selectSession(''));
  const canRewind = (message: Message) => !busy() && !session()?.archived && message.role === 'user' && !taskNotification(message);

  createEffect(on(() => state.sessionID, () => { setVisible(120); setAtBottom(true); setSearch(''); setFind(false); setSelectedTask(''); }));
  createEffect(on(() => state.messageRevision, () => { if (atBottom()) requestAnimationFrame(toBottom); }));
  createEffect(on(search, () => setMatch(0)));
  createEffect(on(() => [state.connected, state.workspace?.id, state.agent?.is_ready], () => {
    const generation = ++initGeneration;
    setNeedsInit(false);
    setInitializing(false);
    if (!state.connected) return;
    const controller = new AbortController();
    onCleanup(() => controller.abort());
    void api.request<{ needs_init: boolean }>(api.route('/project/needs-init'), 'GET', undefined, controller.signal).then(value => {
      if (!controller.signal.aborted && generation === initGeneration) setNeedsInit(Boolean(value.needs_init));
    }).catch(reason => {
      if (!controller.signal.aborted && generation === initGeneration) client.toast(`Could not check project initialization: ${errorText(reason)}`, 'error');
    });
  }));

  async function initializeProject(skip = false) {
    if (initializing() || !state.connected) return;
    const generation = ++initGeneration;
    const workspaceID = state.workspace?.id;
    setInitializing(true);
    try {
      if (skip) await api.request(api.route('/project/init'), 'POST');
      else await client.initializeProject();
      if (generation === initGeneration && workspaceID === state.workspace?.id) setNeedsInit(false);
    } finally {
      if (generation === initGeneration && workspaceID === state.workspace?.id) setInitializing(false);
    }
  }
  createEffect(() => {
    if (!state.connected) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => { if (!pageHidden()) await client.refreshTasks().catch(() => {}); if (!stopped) timer = setTimeout(poll, runningTasks() ? 2500 : 8000); };
    timer = setTimeout(poll, 2500);
    const resume = () => { if (!pageHidden() && state.connected) void Promise.all([client.refreshSessions(), client.refreshQueue(), client.refreshTasks()]).catch(client.fail); };
    document.addEventListener('visibilitychange', resume);
    onCleanup(() => { stopped = true; clearTimeout(timer); document.removeEventListener('visibilitychange', resume); });
  });
  createEffect(() => { document.title = session()?.title ? `${session()!.title} · Crux` : `${workspaceName()} · Crux`; });
  const brand = createMemo(() => activeBrand(state));
  createEffect(() => applyBrand(brand()));

  function jump(index: number) {
    const list = matches();
    if (!list.length) return;
    const next = (index + list.length) % list.length;
    setMatch(next); setAtBottom(false);
    setVisible(Math.max(visible(), state.messageIDs.length - state.messageIDs.indexOf(list[next])));
    requestAnimationFrame(() => scroller.querySelector<HTMLElement>(`[data-message-id="${CSS.escape(list[next])}"]`)?.scrollIntoView({ block: 'center', behavior: 'smooth' }));
  }

  const [loadingOlder, setLoadingOlder] = createSignal(false);
  async function earlier(all: boolean) {
    if (loadingOlder()) return 0;
    setLoadingOlder(true);
    try { return await client.loadOlderMessages(all); }
    catch (reason) { client.fail(reason); return 0; }
    finally { setLoadingOlder(false); }
  }
  async function showEarlier() {
    const height = scroller.scrollHeight;
    const hidden = state.messageIDs.length - visible();
    const added = hidden > 0 ? 0 : await earlier(false);
    setVisible(count => count + (hidden > 0 ? 120 : added));
    requestAnimationFrame(() => { scroller.scrollTop += scroller.scrollHeight - height; });
  }
  async function searchAll() {
    await earlier(true);
  }

  async function exportSession() {
    const title = session()?.title || 'Crux session';
    const messages = await api.request<Message[]>(api.route(`/sessions/${state.sessionID}/messages`));
    const url = URL.createObjectURL(new Blob([sessionMarkdown(title, messages)], { type: 'text/markdown' }));
    const link = document.createElement('a');
    link.href = url; link.download = `${title.replace(/[^a-z0-9_-]+/gi, '_')}.md`; link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  const commands = createMemo<Command[]>(() => {
    const items: Command[] = [
      { id: 'new', name: 'New session', icon: 'plus', shortcut: '⌘N', group: 'Session', run: () => void newSession() },
      { id: 'model', name: 'Switch model', icon: 'layers', group: 'Model', run: () => setSettings('models') },
      { id: 'delivery', name: state.deliveryMode === 'queue' ? 'Switch to Steer mode' : 'Switch to Queue mode', detail: 'How messages sent while working are delivered', icon: state.deliveryMode === 'queue' ? 'steer' : 'queue', group: 'Behavior', run: () => void attempt(() => client.setDeliveryMode(state.deliveryMode === 'queue' ? 'steer' : 'queue')) },
      { id: 'yolo', name: state.autoApprove ? 'Turn off auto-approve' : 'Turn on auto-approve', detail: 'Approve every tool request in this workspace', icon: 'shield', group: 'Behavior', run: () => void attempt(async () => { await api.request(api.route('/permissions/skip'), 'POST', { skip: !state.autoApprove }); await client.refreshWorkspace(); }) },
      { id: 'shell', name: 'Toggle shell', icon: 'terminal', shortcut: '⌘J', group: 'View', run: () => setShell(!shell()) },
      { id: 'panel', name: 'Toggle side panel', icon: 'panel', shortcut: '⌘.', group: 'View', run: () => setPanel(!panel()) },
      { id: 'sessions', name: 'Sessions', detail: 'Browse, search and manage sessions', icon: 'chat', group: 'View', run: () => openPanel('sessions') },
      { id: 'files', name: 'Files and checkpoints', icon: 'file', group: 'View', run: () => openPanel('files') },
      { id: 'tasks', name: 'Background tasks', detail: `${runningTasks()} running`, icon: 'bolt', group: 'View', run: () => openPanel('tasks') },
      { id: 'errors', name: 'Recent errors', detail: `${state.recentErrors.length} retained`, icon: 'warning', group: 'View', run: () => setErrors(true) },
      { id: 'workspace', name: 'Open workspace…', icon: 'folder', group: 'Workspace', run: () => setWorkspaceDialog(true) },
      { id: 'init', name: 'Initialize project', detail: 'Have the agent write the project instructions file', icon: 'file', group: 'Workspace', run: () => void attempt(() => initializeProject()) },
      { id: 'settings', name: 'Settings', icon: 'settings', shortcut: '⌘,', group: 'Settings', run: () => setSettings('models') },
      { id: 'providers', name: 'Providers & accounts', detail: 'Sign in, switch accounts, API keys, usage', icon: 'key', group: 'Settings', run: () => setSettings('providers') },
      { id: 'instructions', name: 'Instructions', detail: 'Instruction sources, runtime controls, sections, preview', shortcut: 'Alt+I', icon: 'file', group: 'Settings', run: () => setSettings('instructions') },
      { id: 'summarization', name: 'Summarization settings', icon: 'database', group: 'Settings', run: () => setSettings('context') },
      { id: 'mcp', name: 'MCP servers & skills', icon: 'plug', group: 'Settings', run: () => setSettings('integrations') },
      { id: 'index', name: 'Codebase index', icon: 'search', group: 'Settings', run: () => setSettings('index') },
      { id: 'agents', name: 'Create agent definition', icon: 'user', group: 'Settings', run: () => setSettings('agents') },
    ];
    if (state.sessionID) items.splice(1, 0,
      { id: 'plan', name: isPlanMode(session()?.mode) ? 'Exit plan mode' : 'Enter plan mode', icon: 'plan', shortcut: '⇧Tab', group: 'Session', run: () => void attempt(() => client.togglePlan()) },
      { id: 'goal', name: session()?.goal?.objective ? 'Manage goal' : 'Set a goal', detail: session()?.goal?.objective, icon: 'target', group: 'Session', run: () => openPanel('session') },
      { id: 'goal-insert', name: 'Goal command', detail: '/goal set, status, pause, resume, stop, complete, clear', icon: 'target', group: 'Session', run: () => composer?.insert('/goal ') },
      { id: 'summarize', name: 'Summarize session', icon: 'refresh', group: 'Session', run: () => void attempt(client.summarize) },
      { id: 'rewind', name: 'Rewind session…', icon: 'undo', group: 'Session', run: () => setRewinding(true) },
      { id: 'find', name: 'Find in conversation', icon: 'search', shortcut: '⌘F', group: 'Session', run: () => setFind(true) },
      { id: 'branch', name: 'Branch session', icon: 'branch', group: 'Session', run: () => void attempt(client.forkSession) },
      { id: 'rename', name: 'Rename session', icon: 'edit', group: 'Session', run: () => setRenaming(session()) },
      { id: 'export', name: 'Export as Markdown', icon: 'file', group: 'Session', run: () => void attempt(exportSession) },
      { id: 'privacy', name: 'Path privacy', detail: 'Hide real paths from the model', icon: 'shield', group: 'Session', run: () => setPrivacy(true) },
      { id: 'archive', name: session()?.archived ? 'Restore session' : 'Archive session', icon: 'archive', group: 'Session', run: () => void attempt(client.archiveSession) },
      { id: 'delete', name: 'Delete session…', icon: 'trash', group: 'Session', run: () => setRemoving(session()) },
    );
    if (busy()) items.unshift({ id: 'stop', name: 'Stop the current turn', icon: 'stop', shortcut: 'Esc', group: 'Session', run: () => void attempt(client.cancelRun) });
    return items;
  });

  onMount(() => {
    const transparency = readStored<number>('backdrop-transparency');
    if (typeof transparency === 'number' && Number.isFinite(transparency)) document.documentElement.style.setProperty('--backdrop-opacity', String(1 - Math.max(0, Math.min(100, transparency)) / 100));
    if (!props.client) void attempt(() => client.connect(true));
    const keyboard = (event: KeyboardEvent) => {
      const inDialog = (event.target as HTMLElement)?.closest?.('[role="dialog"]');
      if ((event.target as HTMLElement)?.closest?.('.terminal-view') && !event.metaKey) return;
      if (event.altKey && event.code === 'KeyI' && !inDialog) { event.preventDefault(); setSettings('instructions'); return; }
      if (!(event.metaKey || event.ctrlKey)) return;
      const key = event.key.toLowerCase();
      const actions: Record<string, () => void> = {
        k: () => setPalette(!palette()), n: () => void newSession(), ',': () => setSettings(settings() ? '' : 'models'), j: () => setShell(!shell()),
        b: () => setPanel(!panel()), '.': () => setPanel(!panel()), f: () => { if (state.sessionID) setFind(!find()); },
      };
      if (actions[key] && (!inDialog || key === 'k')) { event.preventDefault(); actions[key](); }
    };
    window.addEventListener('keydown', keyboard);
    onCleanup(() => window.removeEventListener('keydown', keyboard));
  });
  onCleanup(() => { if (!props.client) client.dispose(); });

  const welcome = () => <section class="welcome">
    <div class="welcome-mark"><Wordmark title={brand().title} /></div>
    <h1>{state.agent?.is_ready ? 'New session' : 'Set up a model to start'}</h1>
    <Show when={!state.agent?.is_ready && state.connected}><div class="callout"><p>No model is configured for this workspace yet. Sign in to a provider and pick a model.</p><div class="actions"><button class="primary" onClick={() => setSettings('providers')}><Icon name="key" size={14} />Sign in to a provider</button><button onClick={() => setSettings('models')}><Icon name="layers" size={14} />Choose a model</button></div></div></Show>
    <Show when={needsInit() && state.agent?.is_ready}><div class="callout">
      <p><strong>Initialize this project?</strong> Crux can examine the codebase and write an instructions file it will use as context.</p>
      <div class="actions"><button class="primary" disabled={initializing() || !state.connected} onClick={() => attempt(() => initializeProject())}>Initialize</button><button disabled={initializing() || !state.connected} onClick={() => attempt(() => initializeProject(true))}>Not now</button></div>
    </div></Show>
    <Show when={state.agent?.is_ready}><div class="starters">
      <For each={[['Explain this codebase', 'Give me a tour of this repository: structure, entry points, and how the main pieces fit together.'], ['Find bugs', 'Review the most recently changed code for bugs and risky behavior.'], ['Write tests', 'Find important untested code and propose tests for it.'], ['Plan a change', 'Help me plan this change before writing code: ']]}>{([title, prompt]) => <button onClick={() => composer?.insert(prompt)}><strong>{title}</strong><small>{prompt}</small></button>}</For>
    </div></Show>
  </section>;

  const actions = <nav class="composer-actions" aria-label="Workspace actions">
    <button type="button" class="hint" aria-label="Command palette" title="Commands (⌘K)" onClick={() => setPalette(true)}><kbd>⌘K</kbd>commands</button>
    <button type="button" class="hint tasks-button" aria-label="Background tasks" onClick={() => { setSelectedTask(''); openPanel('tasks'); }}><Icon name="bolt" size={12} />tasks<Show when={runningTasks()}><span class="count">{runningTasks()}</span></Show></button>
    <Show when={state.sessionID}><button type="button" class="hint" aria-label="Find in conversation" title="Find (⌘F)" onClick={() => setFind(!find())}><kbd>⌘F</kbd>find</button></Show>
    <button type="button" class="hint" aria-label="Toggle shell" title="Shell (⌘J)" aria-pressed={shell()} onClick={() => setShell(!shell())}><kbd>⌘J</kbd>shell</button>
    <button type="button" class="hint" aria-label="Instructions" title="Instructions (Alt+I)" onClick={() => setSettings('instructions')}><kbd>⌥I</kbd>instructions</button>
    <Show when={state.recentErrors.length}><button type="button" class="hint bad" aria-label="Recent errors" title="Recent errors" onClick={() => setErrors(true)}><Icon name="warning" size={12} />errors<span class="count">{state.recentErrors.length}</span></button></Show>
    <button type="button" class="hint" aria-label="Settings" title="Settings (⌘,)" onClick={() => setSettings('models')}><kbd>⌘,</kbd>settings</button>
    <button type="button" class="hint" aria-label="Toggle details panel" title="Side panel (⌘.)" aria-pressed={panel()} onClick={() => setPanel(!panel())}><kbd>⌘.</kbd>{panel() ? 'hide' : 'show'} panel</button>
  </nav>;

  return <div class="app" classList={{ 'with-panel': panel() }}>
    <Show when={window.cruxDesktop}><DesktopProjectSidebar client={client} detailsOpen={panel()} toggleDetails={() => setPanel(!panel())} rename={value => setRenaming(value)} remove={value => setRemoving(value)} registerSessions={open => { openDesktopSessions = open; }} /></Show>
    <main class="main">
      <Show when={!state.connected && !state.loading}><div class="banner bad" role="alert"><Icon name="warning" size={14} /><span>Disconnected from Crux.</span><button class="small" onClick={() => attempt(() => client.connect(true))}><Icon name="refresh" size={13} />Reconnect</button></div></Show>
      <Show when={state.notice}><div class="banner" role="status"><Icon name="info" size={14} />{state.notice}</div></Show>
      <Show when={state.error}><div class="banner bad" role="alert"><Icon name="warning" size={14} /><span>{state.error}</span><button class="ghost small" onClick={() => setErrors(true)}>Recent errors</button><button class="ghost icon" aria-label="Dismiss error" onClick={() => client.set('error', '')}><Icon name="close" size={13} /></button></div></Show>
      <Show when={find()}><div class="find-bar"><Icon name="search" size={14} /><input autofocus aria-label="Find in conversation" placeholder="Find in conversation" value={search()} onInput={event => setSearch(event.currentTarget.value)} onKeyDown={event => { if (event.key === 'Enter') jump(match() + (event.shiftKey ? -1 : 1)); if (event.key === 'Escape') setFind(false); }} /><span class="muted">{matches().length ? `${match() + 1}/${matches().length}` : 'No matches'}</span><Show when={state.hasOlderMessages}><button class="ghost small" disabled={loadingOlder()} onClick={() => void searchAll()}>{loadingOlder() ? 'Loading…' : 'Search full history'}</button></Show><button class="ghost icon" aria-label="Previous match" disabled={!matches().length} onClick={() => jump(match() - 1)}><Icon name="down" size={13} class="flip" /></button><button class="ghost icon" aria-label="Next match" disabled={!matches().length} onClick={() => jump(match() + 1)}><Icon name="down" size={13} /></button><button class="ghost icon" aria-label="Close find" onClick={() => setFind(false)}><Icon name="close" size={13} /></button></div></Show>
      <div class="conversation" ref={scroller} onScroll={() => setAtBottom(scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120)}>
        <div class="thread">
          <Show when={state.messageIDs.length} fallback={state.loading ? <div class="loading"><span class="spinner" />Connecting to Crux…</div> : welcome()}>
            <Show when={state.messageIDs.length > visible() || state.hasOlderMessages}><button class="ghost load-more" disabled={loadingOlder()} onClick={() => void showEarlier()}>{loadingOlder() ? 'Loading earlier messages…' : state.messageIDs.length > visible() ? `Show ${Math.min(120, state.messageIDs.length - visible())} earlier messages` : 'Load earlier messages'}</button></Show>
            <For each={shown()}>{id => <Show when={state.messages[id]}>{message => <MessageView message={message()} results={pairing().results} calls={pairing().calls} tint={messageTint(message())} highlighted={matches()[match()] === id} openTask={openTask} rewind={canRewind(message()) ? () => setRewinding(message()) : undefined} />}</Show>}</For>
            <Show when={busy()}><div class="working"><span class="dots"><i /><i /><i /></span>Working<Show when={state.queued.length}><span class="muted"> · {state.queued.length} pending</span></Show><span class="muted"> · Esc to stop</span></div></Show>
          </Show>
        </div>
      </div>
      <Show when={!atBottom()}><button class="jump" onClick={toBottom} aria-label="Jump to latest"><Icon name="down" size={14} /></button></Show>
      <div class="bottom">
        <Decisions client={client} />
        <Composer client={client} sent={toBottom} openShell={() => setShell(true)} openErrors={() => setErrors(true)} openIndex={() => setSettings('index')} actions={actions} ref={handle => { composer = handle; }} />
      </div>
      <div class="shell-dock" classList={{ collapsed: !shell() || dockDetached() }} style={{ '--dock-height': `${dockHeight()}px` }}><ShellDock client={client} collapsed={!shell()} expand={() => setShell(true)} close={() => setShell(false)} detached={setDockDetached} resize={resizeDock} commands={commands()} taskRequest={taskRequest()} selectedTask={selectedTask()} /></div>
    </main>
    <Show when={panel()}><div class="scrim" onClick={() => setPanel(false)} /></Show>
    <div class="side-dock" classList={{ open: panel() }}>
      <Show when={!window.cruxDesktop}><button class="side-tab" aria-label={panel() ? 'Close details' : 'Open details'} aria-expanded={panel()} title={`${panel() ? 'Hide' : 'Show'} side panel (⌘.)`} onClick={() => setPanel(!panel())}><Icon name="right" size={14} class={panel() ? undefined : 'flip'} /></button></Show>
      <Show when={panel()}><Panel client={client} sections={sections()} toggle={section => toggleSection(section)} selectedTask={selectedTask()} close={() => setPanel(false)} openWorkspace={() => setWorkspaceDialog(true)} rename={value => setRenaming(value)} remove={value => setRemoving(value)} /></Show>
    </div>

    <Show when={settings()}><Settings client={client} section={settings() as SettingsSection} close={() => setSettings('')} /></Show>
    <Show when={palette()}><Palette client={client} commands={commands()} close={() => setPalette(false)} /></Show>
    <Show when={errors()}><ErrorsDialog client={client} close={() => setErrors(false)} /></Show>
    <Show when={privacy() && state.sessionID}><PathTranslation client={client} close={() => setPrivacy(false)} /></Show>
    <Show when={workspaceDialog()}><WorkspaceDialog client={client} close={() => setWorkspaceDialog(false)} /></Show>
    <Show when={renaming()}>{value => <RenameDialog client={client} session={value()} close={() => setRenaming(undefined)} />}</Show>
    <Show when={removing()}>{value => <DeleteDialog client={client} session={value()} close={() => setRemoving(undefined)} />}</Show>
    <Show when={rewinding()}>{value => <RewindDialog client={client} message={value() === true ? undefined : value() as Message} close={() => setRewinding(undefined)} />}</Show>
    <Toasts toasts={state.toasts} dismiss={client.dismissToast} />
  </div>;
}

function ErrorsDialog(props: { client: CruxState; close: () => void }) {
  const [query, setQuery] = createSignal('');
  const [copied, setCopied] = createSignal('');
  const [copyError, setCopyError] = createSignal('');
  const entries = createMemo(() => [...props.client.state.recentErrors].reverse().filter(item => `${item.text}\n${item.workspace ?? ''}\n${item.sessionID ?? ''}`.toLowerCase().includes(query().toLowerCase())));
  const allText = () => entries().map(entry => [new Date(entry.time).toLocaleString(), entry.workspace, entry.sessionID ? `Session ${entry.sessionID}` : '', entry.text].filter(Boolean).join('\n')).join('\n\n');
  let disposed = false;
  onCleanup(() => { disposed = true; });
  async function copy(text: string) {
    setCopyError('');
    try { await navigator.clipboard.writeText(text); if (!disposed) setCopied(text); }
    catch (reason) { if (!disposed) setCopyError(errorText(reason)); }
  }
  return <Modal title="Recent errors" subtitle="Last 50 errors seen in this browser tab. History is cleared on page reload." close={props.close} size="large">
    <div class="modal-body stack">
      <div class="actions"><input autofocus aria-label="Search recent errors" placeholder="Search errors, workspaces, or sessions" value={query()} onInput={event => setQuery(event.currentTarget.value)} /><span class="spacer" /><button disabled={!entries().length} onClick={() => copy(allText())}>{copied() && copied() === allText() ? 'Copied errors' : 'Copy listed errors'}</button><button disabled={!props.client.state.recentErrors.length} onClick={props.client.clearErrors}>Clear history</button></div>
      <Show when={copyError()}><p class="form-error" role="alert">{copyError()}</p></Show>
      <For each={entries()} fallback={<p class="muted">{props.client.state.recentErrors.length ? 'No matching errors.' : 'No recent errors.'}</p>}>{entry => <section class="card error-record">
        <div class="actions"><time dateTime={new Date(entry.time).toISOString()}>{new Date(entry.time).toLocaleString()}</time><span class="spacer" /><button class="ghost small" onClick={() => copy(entry.text)}><Icon name="copy" size={13} />{copied() === entry.text ? 'Copied' : 'Copy error'}</button></div>
        <Show when={entry.workspace}><p class="muted small-text">{entry.workspace}</p></Show>
        <Show when={entry.sessionID}><p class="muted small-text">Session {entry.sessionID}</p></Show>
        <pre>{entry.text}</pre>
      </section>}</For>
    </div>
  </Modal>;
}

function RenameDialog(props: { client: CruxState; session: Session; close: () => void }) {
  const [title, setTitle] = createSignal(props.session.title);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const workspaceID = props.client.state.workspace?.id;
  const route = props.client.api.route(`/sessions/${props.session.id}/title`);
  const connection = props.client.api.connection;
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && props.client.api.connection === connection && props.client.state.workspace?.id === workspaceID;
  createEffect(() => { if (props.client.state.workspace?.id !== workspaceID || (props.client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  return <Modal title="Rename session" close={props.close} size="small">
    <form class="modal-body stack" onSubmit={async event => {
      event.preventDefault();
      if (busy() || !current()) return;
      setBusy(true); setError('');
      try {
        await props.client.api.request(route, 'PUT', { title: title().trim() });
        if (!current()) return;
        await props.client.refreshSessions();
        if (current()) props.close();
      } catch (reason) { if (current()) setError(errorText(reason)); }
      finally { if (current()) setBusy(false); }
    }}>
      <input autofocus aria-label="Session title" required value={title()} onInput={event => setTitle(event.currentTarget.value)} />
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <div class="actions"><span class="spacer" /><button type="button" onClick={props.close}>Cancel</button><button class="primary" disabled={busy() || !title().trim()}>Rename</button></div>
    </form>
  </Modal>;
}

function DeleteDialog(props: { client: CruxState; session: Session; close: () => void }) {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const workspaceID = props.client.state.workspace?.id;
  const connection = props.client.api.connection;
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && props.client.api.connection === connection && props.client.state.workspace?.id === workspaceID;
  createEffect(() => { if (props.client.state.workspace?.id !== workspaceID || (props.client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  return <Modal title="Delete session" close={props.close} size="small">
    <div class="modal-body stack">
      <p>Permanently delete <strong>{props.session.title || 'this session'}</strong> and its child sessions? This cannot be undone.</p>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <div class="actions"><span class="spacer" /><button onClick={props.close}>Cancel</button><button class="danger solid" disabled={busy()} onClick={() => { if (busy() || !current()) return; setBusy(true); setError(''); void props.client.deleteSession(props.session.id).then(() => { if (current()) props.close(); }).catch(reason => { if (current()) setError(errorText(reason)); }).finally(() => { if (current()) setBusy(false); }); }}>Delete</button></div>
    </div>
  </Modal>;
}

function RewindDialog(props: { client: CruxState; message?: Message; close: () => void }) {
  const { client } = props;
  const sessionID = client.state.sessionID;
  const workspaceID = client.state.workspace?.id;
  const connection = client.api.connection;
  createEffect(() => { if (client.state.sessionID !== sessionID || client.state.workspace?.id !== workspaceID || (client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  const [selected, setSelected] = createSignal<Message | undefined>(props.message);
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && client.api.connection === connection && client.state.workspace?.id === workspaceID && client.state.sessionID === sessionID;
  const [operation, setOperation] = createSignal<'rewind' | 'rollback'>('rewind');
  const [summarizeFirst, setSummarizeFirst] = createSignal(false);
  const [restore, setRestore] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const candidates = createMemo(() => client.state.messageIDs.map(id => client.state.messages[id]).filter(message => message && message.role === 'user' && !taskNotification(message)).reverse());
  return <Modal title={operation() === 'rollback' ? 'Roll back session' : 'Rewind session'} subtitle="Remove a message and everything after it." close={props.close} size="medium">
    <div class="modal-body stack">
      <Show when={!props.message}><div class="list scroll-list"><For each={candidates()} fallback={<p class="muted">No messages to rewind to.</p>}>{message => <button class="list-item" classList={{ active: selected()?.id === message.id }} onClick={() => setSelected(message)}><Icon name="user" size={14} /><span><strong class="clamp">{messageText(message) || 'Attachment'}</strong><small>{new Date(message.created_at * 1000).toLocaleString()}</small></span></button>}</For></div>
        <Show when={client.state.hasOlderMessages}><button class="ghost small" disabled={busy()} onClick={() => { if (busy() || !current()) return; setBusy(true); void client.loadOlderMessages().catch(reason => { if (current()) setError(errorText(reason)); }).finally(() => { if (current()) setBusy(false); }); }}>Load earlier messages</button></Show>
      </Show>
      <Show when={selected()}>{message => <blockquote class="quote">{messageText(message()) || 'Attachment'}</blockquote>}</Show>
      <label class="field"><span>History operation</span><select aria-label="History operation" disabled={busy()} value={operation()} onChange={event => setOperation(event.currentTarget.value as 'rewind' | 'rollback')}><option value="rewind">Rewind with optional summary or file restoration</option><option value="rollback">Roll back conversation only</option></select></label>
      <Show when={operation() === 'rewind'} fallback={<p class="muted small-text">Remove the selected message and all later messages atomically. Files stay unchanged; removed turns lose their saved checkpoints. No new summary is created.</p>}>
      <label class="check"><input type="checkbox" checked={summarizeFirst()} disabled={busy()} onChange={event => setSummarizeFirst(event.currentTarget.checked)} /><span>Summarize before rewinding<small class="muted"> Preserve a summary of the conversation before removing later messages.</small></span></label>
      <label class="check"><input type="checkbox" checked={restore()} disabled={busy()} onChange={event => setRestore(event.currentTarget.checked)} /><span>Also restore files to their checkpoints<small class="muted"> Overwrites affected workspace files.</small></span></label>
      </Show>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <div class="actions"><span class="spacer" /><button onClick={props.close}>Cancel</button><button class="danger solid" disabled={busy() || !selected() || client.state.currentSession?.is_busy} onClick={() => { if (busy() || !current()) return; setBusy(true); setError(''); void (operation() === 'rollback' ? client.rollback(selected()!.id) : client.rewind(selected()!.id, restore(), summarizeFirst())).then(() => { if (current()) props.close(); }).catch(reason => { if (current()) setError(errorText(reason)); }).finally(() => { if (current()) setBusy(false); }); }}>{operation() === 'rollback' ? 'Roll back' : 'Rewind'}</button></div>
    </div>
  </Modal>;
}

function WorkspaceDialog(props: { client: CruxState; close: () => void }) {
  const { client } = props;
  const [path, setPath] = createSignal(client.state.workspace?.path ?? client.state.startup?.path ?? '');
  const [attached, setAttached] = createSignal<Workspace[]>([]);
  const [creating, setCreating] = createSignal(false);
  const [created, setCreated] = createSignal('');
  const [listError, setListError] = createSignal('');
  const [listing, setListing] = createSignal<{ roots: string[]; path: string; parent?: string; entries: { name: string; path: string; directory: boolean }[]; truncated?: boolean }>();
  const [browseError, setBrowseError] = createSignal('');
  const [browsing, setBrowsing] = createSignal(false);
  let browseController: AbortController | undefined;
  onCleanup(() => browseController?.abort());
  async function browse(directory: string) {
    browseController?.abort();
    const controller = new AbortController();
    browseController = controller;
    setBrowsing(true); setBrowseError('');
    try {
      const value = await client.api.request<NonNullable<ReturnType<typeof listing>>>(`/v1/browser?${new URLSearchParams({ path: directory })}`, 'GET', undefined, controller.signal);
      if (!controller.signal.aborted) setListing(value);
    } catch (reason) {
      if (!controller.signal.aborted) setBrowseError(errorText(reason));
    } finally {
      if (!controller.signal.aborted) setBrowsing(false);
    }
  }
  onMount(() => void client.api.request<Workspace[]>('/v1/workspaces').then(value => setAttached(value ?? [])).catch(reason => setListError(errorText(reason))));
  return <Modal title="Open workspace" subtitle="Load a directory's native Crux configuration and sessions." close={props.close} size="small">
    <form class="modal-body stack" onSubmit={event => { event.preventDefault(); void client.attempt(async () => { await client.switchWorkspace(path().trim()); props.close(); }); }}>
      <div class="inline-form"><input autofocus required aria-label="Workspace directory" placeholder="/absolute/path/to/project" value={path()} onInput={event => setPath(event.currentTarget.value)} /><Show when={window.cruxDesktop && !client.state.workspace?.connection_name}><button type="button" onClick={() => client.attempt(async () => { const chosen = await window.cruxDesktop!.selectDirectory(); if (chosen) setPath(chosen); })}><Icon name="folder" size={14} />Choose…</button></Show></div>
      <div class="actions"><button type="button" disabled={browsing()} onClick={() => void browse(path().trim())}><Icon name="folder" size={14} />Browse directories</button><button type="button" disabled={browsing()} onClick={() => void browse('')}>Browse roots</button><button type="button" onClick={() => setCreating(!creating())}>{creating() ? 'Hide creation form' : 'Create or clone…'}</button></div>
      <Show when={browsing()}><p class="muted" role="status">Loading directories…</p></Show>
      <Show when={browseError()}><p class="form-error" role="alert">{browseError()}</p></Show>
      <Show when={listing()}>{value => <section class="stack" aria-label="Workspace directories">
        <code>{value().path}</code>
        <div class="actions"><Show when={value().parent}><button type="button" disabled={browsing()} onClick={() => void browse(value().parent!)}>Parent directory</button></Show><button type="button" disabled={browsing()} onClick={() => setPath(value().path)}>Use this directory</button></div>
        <label class="field"><span>Root</span><select aria-label="Browse root" disabled={browsing()} value="" onChange={event => { if (event.currentTarget.value) void browse(event.currentTarget.value); }}><option value="">Choose a root…</option><For each={value().roots}>{root => <option value={root}>{root}</option>}</For></select></label>
        <div class="list"><For each={value().entries.filter(entry => entry.directory)} fallback={<p class="muted">No subdirectories in this listing.</p>}>{entry => <button type="button" class="list-item" disabled={browsing()} onClick={() => void browse(entry.path)}><Icon name="folder" size={14} /><span>{entry.name}</span></button>}</For></div>
        <Show when={value().truncated}><p class="muted">This directory listing is truncated. Enter a directory path directly if it is not shown.</p></Show>
      </section>}</Show>
      <Show when={created()}><p class="small-text" role="status">Created {created()}. Open it when ready.</p></Show>
      <Show when={listError()}><p class="form-error" role="alert">Could not list open workspaces: {listError()}. Enter a directory to open it directly.</p></Show>
      <Show when={attached().length}><div class="section-label">Open in this host</div><div class="list"><For each={attached()}>{workspace => <button type="button" class="list-item" onClick={() => setPath(workspace.path)}><Icon name="folder" size={14} /><span><strong>{workspace.path.split(/[\\/]/).filter(Boolean).pop()}</strong><small>{workspace.path}</small></span></button>}</For></div></Show>
      <Show when={client.state.error}><p class="form-error">{client.state.error}</p></Show>
      <div class="actions"><span class="spacer" /><button type="button" onClick={props.close}>Cancel</button><button class="primary" disabled={client.state.loading || !path().trim()}>Open</button></div>
    </form>
    <Show when={creating()}><CreateWorkspaceDirectory client={client} created={directory => { setPath(directory); setCreated(directory); setCreating(false); }} /></Show>
  </Modal>;
}

function CreateWorkspaceDirectory(props: { client: CruxState; created: (path: string) => void }) {
  const [roots, setRoots] = createSignal<string[]>([]);
  const [root, setRoot] = createSignal('');
  const [relativePath, setRelativePath] = createSignal('');
  const [mode, setMode] = createSignal('plain');
  const [cloneURL, setCloneURL] = createSignal('');
  const [loading, setLoading] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const controller = new AbortController();
  onCleanup(() => controller.abort());
  async function loadRoots() {
    if (loading() || controller.signal.aborted) return;
    setLoading(true); setError('');
    try {
      const listing = await props.client.api.request<{ roots: string[] }>('/v1/browser', 'GET', undefined, controller.signal);
      if (controller.signal.aborted) return;
      setRoots(listing.roots);
      if (!listing.roots.includes(root())) setRoot(listing.roots[0] ?? '');
    } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); }
    finally { if (!controller.signal.aborted) setLoading(false); }
  }
  onMount(() => void loadRoots());
  async function create(event: SubmitEvent) {
    event.preventDefault();
    if (busy() || loading() || controller.signal.aborted) return;
    setError('');
    const relative = relativePath();
    if (!root() || !relative || relative.split('/').some(segment => !segment || segment === '.' || segment === '..' || segment.trim() !== segment || /[\\\u0000]/.test(segment))) { setError('Choose a root and enter a relative directory path without empty, . or .. segments.'); return; }
    const selectedMode = mode();
    if (!['plain', 'git-init', 'git-clone'].includes(selectedMode)) { setError('Choose a supported creation mode.'); return; }
    setBusy(true);
    try {
      const result = await props.client.api.request<{ path: string }>('/v1/browser/create', 'POST', { root: root(), relative_path: relative, mode: selectedMode, ...(selectedMode === 'git-clone' ? { clone_url: cloneURL().trim() } : {}) }, controller.signal);
      if (controller.signal.aborted) return;
      if (!result.path) throw new Error('The host returned no created directory. Browse the selected root before trying again.');
      props.created(result.path);
    } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); }
    finally { if (!controller.signal.aborted) setBusy(false); }
  }
  return <form class="modal-body stack" aria-label="Create workspace directory" onSubmit={create}>
    <h3>Create or clone a workspace</h3>
    <p class="muted small-text">Create a directory on this host, then open it to load its native configuration and sessions.</p>
    <fieldset class="stack" disabled={busy() || loading()}>
      <label class="field"><span>Workspace root</span><select required aria-label="Creation root" value={root()} onChange={event => setRoot(event.currentTarget.value)}><option value="">Choose a root…</option><For each={roots()}>{directory => <option value={directory}>{directory}</option>}</For></select></label>
      <label class="field"><span>Relative directory path</span><input required aria-label="New workspace directory" placeholder="projects/my-project" value={relativePath()} onInput={event => setRelativePath(event.currentTarget.value)} /></label>
      <label class="field"><span>Create</span><select aria-label="Workspace creation mode" value={mode()} onChange={event => setMode(event.currentTarget.value)}><option value="plain">Empty directory</option><option value="git-init">Initialize Git repository</option><option value="git-clone">Clone Git repository</option></select></label>
      <Show when={mode() === 'git-clone'}><label class="field"><span>Repository URL</span><input required aria-label="Repository clone URL" placeholder="https://host/owner/repository.git" value={cloneURL()} onInput={event => setCloneURL(event.currentTarget.value)} /><small>HTTP(S), SSH, or git@host:path. Git uses this host's credentials; interactive credential prompts are disabled.</small></label></Show>
      <div class="actions"><button type="button" onClick={() => void loadRoots()}>Refresh roots</button><span class="spacer" /><button class="primary" disabled={!root() || !relativePath() || mode() === 'git-clone' && !cloneURL().trim()}>{busy() ? 'Creating…' : 'Create directory'}</button></div>
    </fieldset>
    <Show when={loading()}><p class="muted" role="status">Loading workspace roots…</p></Show>
    <Show when={busy()}><p class="muted" role="status">Creating the directory. Git operations can take several minutes.</p></Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <p class="muted small-text">Existing directories are not replaced. Failed or canceled Git operations can leave files behind; inspect the directory before retrying. Closing this form cancels an in-flight request.</p>
  </form>;
}
