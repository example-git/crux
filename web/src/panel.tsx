import { createEffect, createMemo, createSignal, For, on, onCleanup, Show, type JSX } from 'solid-js';
import { isPlanMode, type CruxState } from './state.ts';
import type { Checkpoint, ProjectInfo, Session } from './types.ts';
import { Markdown } from './conversation.tsx';
import { PathTranslation } from './privacy.tsx';
import { SessionList } from './sidebar.tsx';
import { Usage } from './providers.tsx';
import { activeBrand, Wordmark } from './brand.tsx';
import { Badge, compact, Empty, errorText, Icon } from './ui.tsx';

const goalTone: Record<string, 'good' | 'warn' | 'bad' | 'accent' | 'neutral'> = { active: 'accent', paused: 'warn', blocked: 'bad', complete: 'good' };

function GoalCard(props: { client: CruxState }) {
  const { client } = props;
  const [draft, setDraft] = createSignal('');
  const [editing, setEditing] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const goal = () => client.state.currentSession?.goal;
  let generation = 0;
  createEffect(on(() => [client.state.workspace?.id, client.state.sessionID], () => {
    generation++;
    setDraft(''); setEditing(false); setBusy(false);
  }));
  async function run(command: string) {
    if (busy() || !client.state.connected) return;
    const current = generation;
    setBusy(true);
    try {
      const notice = await client.runGoal(command);
      if (current === generation) { client.toast(notice, 'success'); setEditing(false); setDraft(''); }
    } catch (reason) { client.toast(errorText(reason), 'error'); }
    finally { if (current === generation) setBusy(false); }
  }
  return <section class="card">
    <header class="card-head"><Icon name="target" size={15} /><strong>Goal</strong><Show when={goal()?.objective}><Badge tone={goalTone[goal()?.status ?? ''] ?? 'neutral'}>{goal()?.status}</Badge></Show></header>
    <Show when={goal()?.objective && !editing()} fallback={<form class="stack" onSubmit={event => { event.preventDefault(); if (draft().trim()) void run(`/goal set ${draft().trim()}`); }}>
      <textarea rows={3} aria-label="Goal objective" placeholder="What should Crux keep working toward in this session?" disabled={busy()} value={draft()} onInput={event => setDraft(event.currentTarget.value)} />
      <div class="actions"><Show when={editing()}><button type="button" onClick={() => setEditing(false)}>Cancel</button></Show><span class="spacer" /><button class="primary" disabled={busy() || !draft().trim() || !client.state.connected}>Set goal</button></div>
    </form>}>
      <p class="goal-text">{goal()!.objective}</p>
      <Show when={goal()?.status === 'blocked' && goal()?.error}><p class="bad small-text">{goal()!.error}</p></Show>
      <p class="muted small-text">Generation {goal()?.generation}</p>
      <div class="button-grid">
        <Show when={goal()?.status === 'active'} fallback={<button disabled={busy() || goal()?.status === 'complete'} onClick={() => run('/goal resume')}><Icon name="send" size={13} />Resume</button>}><button disabled={busy()} onClick={() => run('/goal pause')} title="Finish this turn, then pause">Pause</button></Show>
        <button disabled={busy()} onClick={() => run('/goal stop')} title="Cancel now; resume later">Stop</button>
        <button disabled={busy() || goal()?.status === 'complete'} onClick={() => run('/goal complete')}>Complete</button>
        <button disabled={busy()} onClick={() => { setDraft(goal()?.objective ?? ''); setEditing(true); }}>Replace</button>
        <button class="danger" disabled={busy()} onClick={() => run('/goal clear')}>Clear</button>
      </div>
    </Show>
  </section>;
}

function ModelContext(props: { client: CruxState }) {
  const { client } = props;
  const { state } = client;
  const session = () => state.currentSession;
  const selected = () => client.selectedModel('large');
  const provider = () => state.workspace?.provider_surfaces?.find(item => item.id === selected()?.provider);
  const info = () => provider()?.models?.find(item => item.id === selected()?.model);
  const contextWindow = () => info()?.context_window ?? state.agent?.model?.context_window ?? 0;
  const used = () => (session()?.prompt_tokens ?? 0) + (session()?.completion_tokens ?? 0) + (session()?.unseen_local_tokens ?? 0);
  const estimated = () => Boolean(session()?.estimated_usage || (session()?.unseen_local_tokens ?? 0) > 0);
  const percent = () => contextWindow() ? Math.min(100, used() / contextWindow() * 100) : 0;
  const reasoning = () => selected()?.reasoning_effort ? `Reasoning ${selected()!.reasoning_effort}` : selected()?.think ? 'Thinking on' : '';
  return <div class="stack">
    <div class="model-summary">
      <strong><span class="diamond">◇</span>{info()?.name || selected()?.model || 'No model selected'}</strong>
      <Show when={provider()}><small>via {provider()!.name}</small></Show>
      <Show when={reasoning()}><small>{reasoning()}</small></Show>
      <Show when={session()}><small>{Math.round(percent())}% ({estimated() ? '~' : ''}{compact(used())}) ${(session()?.cost ?? 0).toFixed(2)}</small></Show>
    </div>
    <Show when={state.sessionID}>
      <section class="card">
        <header class="card-head"><Icon name="database" size={15} /><strong>Context</strong><span class="spacer" /><span class="muted">{estimated() ? '~' : ''}{compact(used())}{contextWindow() ? ` / ${compact(contextWindow())}` : ''}</span></header>
        <div class="meter" classList={{ warn: percent() > 70, bad: percent() > 90 }}><span style={{ width: `${percent()}%` }} /></div>
        <div class="stats">
          <div><strong>{compact(session()?.prompt_tokens ?? 0)}</strong><span>Input</span></div>
          <div><strong>{compact(session()?.completion_tokens ?? 0)}</strong><span>Output</span></div>
          <div><strong>${(session()?.cost ?? 0).toFixed(3)}</strong><span>Cost</span></div>
          <div><strong>{session()?.message_count ?? 0}</strong><span>Messages</span></div>
        </div>
        <Show when={estimated()}><p class="muted small-text">Context usage is estimated.</p></Show>
        <button class="wide" disabled={session()?.is_busy || !state.agent?.is_ready} onClick={() => client.attempt(async () => { await client.summarize(); client.toast('Session summarized.', 'success'); })}><Icon name="refresh" size={14} />Summarize session</button>
      </section>
    </Show>
  </div>;
}

function Overview(props: { client: CruxState; openPrivacy: () => void; openWorkspace: () => void; rename: () => void }) {
  const { client } = props;
  const { state } = client;
  const session = () => state.currentSession;
  return <div class="panel-scroll">
    <button class="directory" title={state.workspace?.path} onClick={props.openWorkspace}>{state.workspace?.path ?? 'Connecting…'}</button>
    <Show when={state.sessionID} fallback={<Empty icon="chat" title="No session selected">Start a conversation to see its goal, plan, and context.</Empty>}>
      <div class="session-identity">
        <button class="title-button" title="Rename" onClick={props.rename}>{session()?.title || 'Untitled session'}</button>
        <small class="muted">ID: {state.sessionID.slice(0, 8)}</small>
      </div>
      <GoalCard client={client} />
      <section class="card">
        <header class="card-head"><Icon name="plan" size={15} /><strong>Plan</strong><Show when={isPlanMode(session()?.mode)}><Badge tone="accent">{session()?.mode.replaceAll('_', ' ')}</Badge></Show><span class="spacer" />
          <Show when={isPlanMode(session()?.mode)} fallback={<button class="ghost small" onClick={() => client.attempt(() => client.togglePlan())}>Enter plan mode</button>}><button class="ghost small" onClick={() => client.attempt(() => client.togglePlan(true))}>Exit plan mode</button></Show>
        </header>
        <Show when={session()?.plan} fallback={<p class="muted small-text">No plan yet.</p>}><Markdown text={session()!.plan!} class="compact" /></Show>
      </section>
      <section class="card">
        <header class="card-head"><Icon name="check" size={15} /><strong>Todos</strong><span class="spacer" /><Show when={session()?.todos?.length}><span class="muted">{session()!.todos!.filter(todo => todo.status === 'completed').length}/{session()!.todos!.length}</span></Show></header>
        <For each={session()?.todos} fallback={<p class="muted small-text">No todos.</p>}>{todo => <div class="todo" classList={{ done: todo.status === 'completed', active: todo.status === 'in_progress' }}><span class="todo-mark">{todo.status === 'completed' ? '✓' : todo.status === 'in_progress' ? '◐' : '○'}</span><span>{todo.status === 'in_progress' && todo.active_form ? todo.active_form : todo.content}</span></div>}</For>
      </section>
      <section class="card">
        <header class="card-head"><Icon name="settings" size={15} /><strong>Session</strong></header>
        <label class="field inline"><span>Analysis effort</span><select aria-label="Analysis effort" value={session()?.analysis_effort ?? ''} disabled={session()?.is_busy || session()?.archived} onChange={event => { const value = event.currentTarget.value; void client.attempt(() => client.updateSession('analysis-effort', value)); }}>
          <option value="">Default</option><For each={['none', 'low', 'medium', 'high', 'xhigh', 'max']}>{value => <option value={value}>{value}</option>}</For>
        </select></label>
        <button class="row-button" onClick={props.openPrivacy}><Icon name="shield" size={14} /><span>Path privacy</span><Icon name="right" size={13} /></button>
        <Show when={session()?.parent_session_id}><button class="row-button" aria-label="Open parent session" disabled={!state.connected} onClick={() => client.attempt(() => client.selectSession(session()!.parent_session_id!))}><Icon name="user" size={14} /><span>Parent: {state.sessions.find(item => item.id === session()?.parent_session_id)?.title || session()?.parent_session_id}</span><Icon name="right" size={13} /></button></Show>
        <Show when={session()?.forked_from_session_id}><button class="row-button" onClick={() => client.attempt(() => client.selectSession(session()!.forked_from_session_id!))}><Icon name="branch" size={14} /><span>Branched from {state.sessions.find(item => item.id === session()?.forked_from_session_id)?.title ?? 'another session'}</span><Icon name="right" size={13} /></button></Show>
        <dl class="meta"><dt>Viewers</dt><dd>{session()?.attached_clients ?? 0}</dd></dl>
      </section>
    </Show>
  </div>;
}

function Project(props: { client: CruxState }) {
  const { client } = props;
  const { state } = client;
  const [projects, setProjects] = createSignal<ProjectInfo[]>([]);
  const [projectError, setProjectError] = createSignal('');
  const [projectBusy, setProjectBusy] = createSignal(false);
  let projectGeneration = 0;
  createEffect(on(() => state.workspace?.id, () => {
    projectGeneration++;
    setProjects([]);
    setProjectError('');
    setProjectBusy(false);
  }));
  createEffect(on(() => [state.workspace?.id, state.activityRevision], () => {
    const id = state.workspace?.id;
    if (!id || projectBusy()) return;
    const generation = ++projectGeneration;
    void client.api.request<ProjectInfo[]>(client.api.route('/projects')).then(value => {
      if (generation === projectGeneration && id === state.workspace?.id) { setProjects(value ?? []); setProjectError(''); }
    }).catch(reason => { if (generation === projectGeneration && id === state.workspace?.id) setProjectError(errorText(reason)); });
  }));
  async function selectProject(slug: string) {
    const id = state.workspace?.id;
    if (!id || projectBusy()) return;
    const route = client.api.route('/projects');
    const generation = ++projectGeneration;
    setProjectBusy(true);
    setProjectError('');
    try {
      await client.api.request(`${route}/selection`, 'POST', { slug });
      const value = await client.api.request<ProjectInfo[]>(route);
      if (generation === projectGeneration && id === state.workspace?.id) setProjects(value ?? []);
    } catch (reason) {
      if (generation === projectGeneration && id === state.workspace?.id) setProjectError(errorText(reason));
    } finally {
      if (generation === projectGeneration && id === state.workspace?.id) setProjectBusy(false);
    }
  }
  return <div class="stack">
    <Show when={projectError()}><p class="form-error" role="alert">{projectError()}</p></Show>
    <select aria-label="Active project" disabled={projectBusy() || !state.connected} value={projects().find(project => project.selected)?.slug ?? ''} onChange={event => { const element = event.currentTarget; const id = state.workspace?.id; void selectProject(element.value).finally(() => { if (id === state.workspace?.id) element.value = projects().find(project => project.selected)?.slug ?? ''; }); }}>
      <option value="">No active project</option>
      <For each={projects()}>{project => <option value={project.slug} disabled={project.status === 'completed'}>{project.name} · {project.completed}/{project.total}{project.status ? ` · ${project.status}` : ''}</option>}</For>
    </select>
    <p class="muted small-text">Durable project context the agent uses in this workspace.</p>
  </div>;
}

function Files(props: { client: CruxState }) {
  const { client } = props;
  const { state } = client;
  const [read, setRead] = createSignal<string[]>([]);
  const [checkpoints, setCheckpoints] = createSignal<Checkpoint[]>([]);
  const [selected, setSelected] = createSignal<Checkpoint>();
  const [readSelection, setReadSelection] = createSignal<{ path: string; workspaceID?: string; connection: string; sessionID: string }>();
  const readPath = () => readSelection()?.path ?? '';
  const setReadPath = (path: string) => setReadSelection(path ? { path, workspaceID: state.workspace?.id, connection: client.api.connection, sessionID: state.sessionID } : undefined);
  const [readTime, setReadTime] = createSignal('');
  const [readError, setReadError] = createSignal('');
  const [readBusy, setReadBusy] = createSignal(false);
  createEffect(on(() => [readPath(), state.workspace?.id, state.workspace?.connection_name, state.sessionID, state.fileRevision, state.connected], () => {
    const path = readPath();
    const sessionID = state.sessionID;
    setReadTime(''); setReadError(''); setReadBusy(false);
    if (!state.connected || !path || !sessionID || readSelection()?.workspaceID !== state.workspace?.id || readSelection()?.connection !== client.api.connection || readSelection()?.sessionID !== sessionID) return;
    const controller = new AbortController();
    onCleanup(() => controller.abort());
    const query = new URLSearchParams({ session_id: sessionID, path });
    setReadBusy(true);
    void client.api.request<string>(client.api.route(`/filetracker/lastread?${query}`), 'GET', undefined, controller.signal).then(value => {
      if (!controller.signal.aborted) setReadTime(value);
    }).catch(reason => { if (!controller.signal.aborted) setReadError(errorText(reason)); }).finally(() => { if (!controller.signal.aborted) setReadBusy(false); });
  }));
  const [filesError, setFilesError] = createSignal('');
  const [historyError, setHistoryError] = createSignal('');
  createEffect(on(() => [state.workspace?.id, state.workspace?.connection_name, state.sessionID], () => {
    setRead([]);
    setReadPath('');
    setCheckpoints([]);
    setSelected(undefined);
    setFilesError('');
    setHistoryError('');
  }));
  createEffect(on(() => [state.workspace?.id, state.workspace?.connection_name, state.sessionID, state.fileRevision, state.activityRevision, state.connected], () => {
    const id = state.sessionID;
    if (!id) { setRead([]); setCheckpoints([]); return; }
    if (!state.connected) return;
    const controller = new AbortController();
    onCleanup(() => controller.abort());
    void client.api.request<string[]>(client.api.route(`/sessions/${id}/filetracker/files`), 'GET', undefined, controller.signal).then(files => {
      if (controller.signal.aborted) return;
      setRead(files ?? []); setFilesError('');
    }).catch(reason => { if (!controller.signal.aborted) setFilesError(errorText(reason)); });
    void client.api.request<Checkpoint[]>(client.api.route(`/sessions/${id}/history`), 'GET', undefined, controller.signal).then(history => {
      if (controller.signal.aborted) return;
      setCheckpoints(history ?? []); setHistoryError('');
      setSelected(current => current ? history?.find(item => item.id === current.id) : undefined);
    }).catch(reason => { if (!controller.signal.aborted) setHistoryError(errorText(reason)); });
  }));
  const grouped = createMemo(() => {
    const map = new Map<string, Checkpoint[]>();
    for (const item of checkpoints()) map.set(item.path, [...(map.get(item.path) ?? []), item]);
    return [...map.entries()].map(([path, versions]) => ({ path, versions: versions.sort((a, b) => b.version - a.version) }));
  });
  const relative = (path: string) => state.workspace && path.startsWith(state.workspace.path + '/') ? path.slice(state.workspace.path.length + 1) : path;
  return <div class="panel-scroll">
    <Show when={state.sessionID} fallback={<Empty icon="file" title="No session selected" />}>
      <section class="card">
        <Show when={historyError()}><p class="form-error" role="alert">Could not refresh checkpoints: {historyError()}</p></Show>
        <header class="card-head"><Icon name="clock" size={15} /><strong>Checkpoints</strong><span class="spacer" /><span class="muted">{checkpoints().length}</span></header>
        <p class="muted small-text">Saved file versions from this session. Rewind can restore them.</p>
        <For each={grouped()} fallback={<p class="muted small-text">No checkpoints.</p>}>{file => <div class="file-group">
          <div class="file-path" title={file.path}><Icon name="file" size={13} />{relative(file.path)}</div>
          <div class="versions"><For each={file.versions}>{version => <button classList={{ active: selected()?.id === version.id }} onClick={() => setSelected(selected()?.id === version.id ? undefined : version)}>v{version.version}{version.exists ? '' : ' (absent)'}</button>}</For></div>
        </div>}</For>
        <Show when={selected()}>{item => <pre class="code-preview">{item().exists ? item().content : 'The file did not exist at this checkpoint.'}</pre>}</Show>
      </section>
      <section class="card">
        <Show when={filesError()}><p class="form-error" role="alert">Could not refresh files read: {filesError()}</p></Show>
        <header class="card-head"><Icon name="eye" size={15} /><strong>Files read</strong><span class="spacer" /><span class="muted">{read().length}</span></header>
        <For each={read()} fallback={<p class="muted small-text">No files read yet.</p>}>{file => <button class="file-path" title={file} aria-label={`Last read: ${file}`} aria-expanded={readPath() === file} onClick={() => setReadPath(readPath() === file ? '' : file)}><Icon name="file" size={13} />{relative(file)}</button>}</For>
        <Show when={readPath()}><div class="stack" aria-label="File read details">
          <code>{relative(readPath())}</code>
          <Show when={readBusy()}><p class="muted small-text" role="status">Loading read time…</p></Show>
          <Show when={readError()}><p class="form-error" role="alert">{readError()}</p></Show>
          <Show when={readTime()}><p class="small-text">{readTime().startsWith('0001-') ? 'No recorded read time.' : <>Last read <time dateTime={readTime()}>{new Date(readTime()).toLocaleString()}</time></>}</p></Show>
        </div></Show>
      </section>
    </Show>
  </div>;
}

export type PanelSection = 'model' | 'session' | 'sessions' | 'tasks' | 'files' | 'project';

export const defaultSections: Record<PanelSection, boolean> = { model: true, session: true, sessions: true, tasks: false, files: false, project: false };

function Section(props: { id: PanelSection; title: string; count?: number; open: boolean; toggle: () => void; children: JSX.Element }) {
  return <section class="side-section" classList={{ open: props.open }} data-section={props.id}>
    <div class="side-section-head">
      <button class="side-section-toggle" aria-expanded={props.open} onClick={props.toggle}><span class="side-section-title">{props.title}</span><span class="side-rule" aria-hidden="true" /></button>
      <Show when={props.count}><span class="side-count">{props.count}</span></Show>
    </div>
    <Show when={props.open}><div class="side-section-body">{props.children}</div></Show>
  </section>;
}

export function Panel(props: {
  client: CruxState; sections: Record<PanelSection, boolean>; toggle: (section: PanelSection) => void; selectedTask?: string; close: () => void;
  openWorkspace: () => void; rename: (session: Session) => void; remove: (session: Session) => void;
}) {
  const [privacy, setPrivacy] = createSignal(false);
  const { state } = props.client;
  const brand = () => activeBrand(state);
  const provider = () => state.workspace?.provider_surfaces?.find(item => item.id === props.client.selectedModel('large')?.provider);
  return <aside class="panel side" aria-label="Session details">
    <header class="panel-head">
      <div class="side-brand"><small class="side-version" title={state.startup?.version}>{state.startup?.version}</small><Wordmark title={brand().title} compact /></div>
    </header>
    <div class="side-scroll">
      <Show when={provider()?.usage_available && provider()?.owner}>{owner => <Usage client={props.client} owner={owner()} compact />}</Show>
      <Section id="model" title="Model / Context" open={props.sections.model} toggle={() => props.toggle('model')}><ModelContext client={props.client} /></Section>
      <Section id="session" title="Session" open={props.sections.session} toggle={() => props.toggle('session')}><Overview client={props.client} openPrivacy={() => setPrivacy(true)} openWorkspace={props.openWorkspace} rename={() => { if (state.currentSession) props.rename(state.currentSession); }} /></Section>
      <Show when={!window.cruxDesktop}><Section id="sessions" title="Sessions" count={state.sessions.filter(item => !item.parent_session_id).length} open={props.sections.sessions} toggle={() => props.toggle('sessions')}><SessionList client={props.client} rename={props.rename} remove={props.remove} close={() => { if (window.innerWidth < 860) props.close(); }} /></Section></Show>
      <Section id="files" title="Files" open={props.sections.files} toggle={() => props.toggle('files')}><Files client={props.client} /></Section>
      <Section id="project" title="Project" open={props.sections.project} toggle={() => props.toggle('project')}><Project client={props.client} /></Section>
    </div>
    <footer class="side-foot"><span class="status-dot" classList={{ online: state.connected }} />{state.connected ? 'Connected' : state.loading ? 'Connecting…' : 'Disconnected'}<span class="spacer" /><span class="muted">⌘. toggle</span></footer>
    <Show when={privacy() && state.sessionID}><PathTranslation client={props.client} close={() => setPrivacy(false)} /></Show>
  </aside>;
}
