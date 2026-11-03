import { createEffect, createMemo, createSignal, For, on, onCleanup, Show, untrack } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Task } from './types.ts';
import { TaskLogTerminal, type TaskTerminalHandle } from './terminal.tsx';
import { Badge, Empty, errorText, Icon, pageHidden, Segmented } from './ui.tsx';

export const taskActive = (task: Task) => ['running', 'pending'].includes(task.state.status);
const tone = (status: string) => status === 'completed' ? 'good' : ['failed', 'lost'].includes(status) ? 'bad' : taskActive({ state: { status } } as Task) ? 'accent' : 'neutral';

function duration(task: Task) {
  if (!task.state.started_at || task.state.started_at.startsWith('0001')) return '';
  const start = Date.parse(task.state.started_at);
  const end = task.state.ended_at && !task.state.ended_at.startsWith('0001') ? Date.parse(task.state.ended_at) : Date.now();
  const seconds = Math.max(0, Math.round((end - start) / 1000));
  return seconds < 60 ? `${seconds}s` : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

type TaskNotice = {
  notification_id: string; task_id: string; status: string; summary: string;
  error_message?: string; lost_reason?: string; final_output?: string; output_ref?: string;
  output_truncated?: boolean; exit_code?: number; interrupted?: boolean;
};

function TaskTerminal(props: { text: string }) {
  return <pre class="task-output" role="region" aria-label="Task terminal output" tabIndex={0}>{props.text}</pre>;
}

function TaskNotices(props: { client: CruxState; task: Task }) {
  const [notices, setNotices] = createSignal<TaskNotice[]>([]);
  const [error, setError] = createSignal('');
  const [revision, setRevision] = createSignal(0);
  createEffect(on(() => [props.client.state.workspace?.id, props.task.id], () => { setNotices([]); setError(''); }));
  createEffect(on(() => [props.client.state.workspace?.id, props.task.id, props.task.state.status, props.client.state.activityRevision, props.client.state.connected, revision()], () => {
    if (!props.client.state.connected) return;
    const controller = new AbortController();
    const root = props.client.api.route('/task-notifications');
    const taskID = props.task.id;
    const parent = props.task.ownership.parent_session_id;
    void (async () => {
      try {
        const entries = await props.client.api.request<TaskNotice[]>(`${root}?parent_session_id=${encodeURIComponent(parent)}&unread_only=true`, 'GET', undefined, controller.signal);
        if (controller.signal.aborted) return;
        const matching = (entries ?? []).filter(entry => entry.task_id === taskID);
        setNotices(previous => [...previous.filter(entry => !matching.some(next => next.notification_id === entry.notification_id)), ...matching]);
        for (const entry of matching) {
          if (controller.signal.aborted) return;
          await props.client.api.request(`${root}/${encodeURIComponent(entry.notification_id)}/read`, 'POST', undefined, controller.signal);
        }
        if (!controller.signal.aborted) setError('');
      } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); }
    })();
    onCleanup(() => controller.abort());
  }));
  return <>
    <For each={notices()}>{notice => <section class="card stack" aria-label="Task completion notice">
      <div class="actions"><strong>{notice.summary || 'Task notification'}</strong><Badge tone={tone(notice.status)}>{notice.status}</Badge></div>
      <Show when={notice.error_message}><p class="bad small-text">{notice.error_message}</p></Show>
      <Show when={notice.lost_reason && notice.lost_reason !== notice.error_message}><p class="bad small-text">{notice.lost_reason}</p></Show>
      <Show when={notice.exit_code !== undefined}><p class="muted small-text">Exit code: {notice.exit_code}</p></Show>
      <Show when={notice.interrupted}><p class="muted small-text">Interrupted</p></Show>
      <Show when={notice.final_output}><details><summary>Notification result</summary><TaskTerminal text={notice.final_output!} /></details></Show>
      <Show when={notice.output_ref}><code>{notice.output_ref}</code></Show>
      <Show when={notice.output_truncated}><p class="muted small-text">Notification output truncated by the task service.</p></Show>
    </section>}</For>
    <Show when={error()}><div class="callout"><p class="form-error" role="alert">Task notifications: {error()}</p><button onClick={() => setRevision(value => value + 1)}>Retry notifications</button></div></Show>
  </>;
}

function TaskItem(props: { client: CruxState; task: Task; back: () => void; select: (task: Task) => void; refresh: () => Promise<void> }) {
  const { client } = props;
  const [terminal, setTerminal] = createSignal<TaskTerminalHandle>();
  const [prompt, setPrompt] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [truncated, setTruncated] = createSignal(false);
  const [revision, setRevision] = createSignal(0);
  const [live, setLive] = createSignal<Task>();
  const [tick, setTick] = createSignal(0);
  const task = () => live() ?? props.task;
  const timer = setInterval(() => setTick(value => value + 1), 1000);
  let disposed = false;
  onCleanup(() => { disposed = true; clearInterval(timer); });
  createEffect(on(() => props.task, value => setLive(value)));
  createEffect(on(() => [props.task.id, client.state.workspace?.id, client.api.connection, client.state.connected, terminal(), revision()] as const, ([id, workspace, connection, connected, viewport]) => {
    if (!connected || !viewport) return;
    const controller = new AbortController();
    const route = client.api.route(`/tasks/${encodeURIComponent(id)}/log`);
    let timer: ReturnType<typeof setTimeout>;
    let offset = 0;
    let generation = '';
    let previous: Uint8Array = new Uint8Array(0);
    const current = () => !controller.signal.aborted && client.state.workspace?.id === workspace && client.api.connection === connection;
    const poll = async () => {
      try {
        const result = await client.api.request<{ task: Task; data: string | null; next_offset: number; generation: string; snapshot: boolean; truncated: boolean }>(route, 'POST', { offset, generation }, controller.signal);
        if (!current()) return;
        const data = Uint8Array.from(atob(result.data ?? ''), character => character.charCodeAt(0));
        const reset = generation !== result.generation;
        if (result.snapshot) {
          const append = !reset && previous.length <= data.length && previous.every((value, index) => value === data[index]);
          viewport.write(append ? data.slice(previous.length) : data, !append);
          previous = data;
        } else {
          viewport.write(data, reset);
        }
        offset = result.next_offset;
        generation = result.generation;
        setLive(result.task); setTruncated(result.truncated); setError('');
        if (data.length >= 65536 && !result.snapshot) timer = setTimeout(poll, 0);
        else if (taskActive(result.task)) timer = setTimeout(poll, 250);
      } catch (reason) {
        if (current()) { setError(errorText(reason)); timer = setTimeout(poll, 1500); }
      }
    };
    void poll();
    onCleanup(() => { controller.abort(); clearTimeout(timer); });
  }));
  async function action(name: 'stop' | 'restart' | 'message') {
    if (busy() || !client.state.connected || (name === 'message' && !prompt().trim())) return;
    const workspaceID = client.state.workspace?.id;
    const connection = client.api.connection;
    const taskID = task().id;
    const current = () => !disposed && client.state.workspace?.id === workspaceID && client.api.connection === connection && task().id === taskID;
    setBusy(true); setError('');
    try {
      const result = await client.api.request<Task>(client.api.route(`/tasks/${encodeURIComponent(taskID)}/${name}`), 'POST', name === 'message' ? { parent_session_id: task().ownership.parent_session_id, prompt: prompt() } : undefined);
      if (!current()) return;
      setPrompt('');
      if (result.id !== taskID) props.select(result); else { setLive(result); setRevision(value => value + 1); }
      await props.refresh();
    } catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  return <article class="task-detail" data-task-id={task().id}>
    <header class="task-info">
      <div class="actions"><button class="ghost icon" aria-label="Back to tasks" onClick={props.back}><Icon name="right" class="flip" size={15} /></button><strong class="truncate">{task().description || task().agent_type || task().command || task().type}</strong><span class="spacer" /><Badge tone={tone(task().state.status)}>{task().state.status}</Badge><time>{(tick(), duration(task()))}</time></div>
      <Show when={task().command}><div class="task-command" title={task().command}>$ {task().command}</div></Show>
      <div class="actions wrap">
        <span class="muted small-text">{task().agent_type || task().type}</span>
        <Show when={taskActive(task())}><button class="danger small" disabled={busy() || !client.state.connected} onClick={() => void action('stop')}>Stop</button></Show>
        <Show when={!taskActive(task()) && task().type === 'shell'}><button class="small" disabled={busy() || !client.state.connected} onClick={() => void action('restart')}>Restart</button></Show>
        <Show when={task().child_session_id}><button class="small" onClick={() => client.attempt(() => client.selectSession(task().child_session_id!))}>Open session</button></Show>
        <Show when={task().state.exit_code !== undefined}><span class="muted small-text">Exit {task().state.exit_code}</span></Show>
      </div>
      <Show when={task().state.error_message || task().state.lost_reason}><p class="form-error">{task().state.error_message || task().state.lost_reason}</p></Show>
      <Show when={truncated()}><p class="muted small-text">The task service reached its output retention limit.</p></Show>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <details class="task-notices"><summary>Task notifications</summary><TaskNotices client={client} task={task()} /></details>
    </header>
    <TaskLogTerminal ready={setTerminal} />
    <Show when={task().type === 'agent' && task().child_session_id}><form class="task-chat" onSubmit={event => { event.preventDefault(); void action('message'); }}>
      <input aria-label="Task follow-up" placeholder="Message this agent…" disabled={busy()} value={prompt()} onInput={event => setPrompt(event.currentTarget.value)} />
      <button class="primary" disabled={busy() || !client.state.connected || !prompt().trim()}>Send</button>
    </form></Show>
  </article>;
}

export function TasksPanel(props: { client: CruxState; selected?: string }) {
  const { client } = props;
  const { state, api } = client;
  const [scope, setScope] = createSignal<'session' | 'workspace'>('session');
  const [filter, setFilter] = createSignal<'all' | 'active'>('all');
  const [query, setQuery] = createSignal('');
  const [open, setOpen] = createSignal('');
  const [tick, setTick] = createSignal(0);
  const clock = setInterval(() => setTick(value => value + 1), 1000);
  onCleanup(() => clearInterval(clock));
  const [detachable, setDetachable] = createSignal(0);
  const [detachError, setDetachError] = createSignal('');
  const [detaching, setDetaching] = createSignal(false);
  const [retrieved, setRetrieved] = createSignal<Task>();
  const [selectedError, setSelectedError] = createSignal('');
  const [selectedLoading, setSelectedLoading] = createSignal(false);
  const [selectedRevision, setSelectedRevision] = createSignal(0);
  let generation = 0;
  onCleanup(() => { generation++; });
  async function refreshDetachable() {
    const current = generation;
    const id = state.sessionID;
    if (!id) { setDetachable(0); return; }
    try {
      const result = await api.request<{ count: number }>(api.route(`/agent/jobs/detach?session_id=${encodeURIComponent(id)}`));
      if (current === generation) { setDetachable(result.count); setDetachError(''); }
    } catch (reason) {
      if (current === generation) { setDetachable(0); setDetachError(errorText(reason)); }
    }
  }
  const refresh = async () => { await Promise.all([client.refreshTasks(), refreshDetachable()]); };
  createEffect(on(() => `${state.workspace?.id}:${state.sessionID}`, () => {
    generation++;
    setOpen(''); setScope(state.sessionID ? 'session' : 'workspace'); setRetrieved(undefined); setDetachable(0); setDetachError(''); setDetaching(false);
    untrack(() => void refresh());
  }));
  createEffect(on(() => state.activityRevision, () => { if (state.connected) void refresh(); }, { defer: true }));
  createEffect(() => {
    if (!state.connected || !state.currentSession?.is_busy) return;
    const timer = setInterval(() => { if (!pageHidden()) void refreshDetachable(); }, 2500);
    onCleanup(() => clearInterval(timer));
  });
  createEffect(on(() => [props.selected, state.workspace?.id, state.connected, selectedRevision()] as const, ([id, workspace, connected]) => {
    setRetrieved(undefined); setSelectedError(''); setSelectedLoading(false);
    if (!id || !workspace) return;
    setScope('workspace'); setFilter('all'); setQuery(''); setOpen(id);
    const existing = state.tasks.find(task => task.id === id);
    if (existing) { setRetrieved(existing); return; }
    if (!connected) return;
    const controller = new AbortController();
    setSelectedLoading(true);
    void api.request<{ task?: Task }>(api.route(`/tasks/${encodeURIComponent(id)}/output`), 'POST', { wait: false, timeout: 0 }, controller.signal).then(result => {
      if (controller.signal.aborted) return;
      if (result.task?.id !== id) throw new Error(`The task service did not return task ${id}.`);
      setRetrieved(result.task);
    }).catch(reason => { if (!controller.signal.aborted) setSelectedError(errorText(reason)); }).finally(() => { if (!controller.signal.aborted) setSelectedLoading(false); });
    onCleanup(() => controller.abort());
  }));
  const all = createMemo(() => [...state.tasks, ...(retrieved() && !state.tasks.some(task => task.id === retrieved()!.id) ? [retrieved()!] : [])]);
  const scoped = createMemo(() => all().filter(task => scope() === 'workspace' || task.ownership.parent_session_id === state.sessionID));
  const tasks = createMemo(() => scoped()
    .filter(task => (filter() === 'all' || taskActive(task)) && `${task.description} ${task.command ?? ''} ${task.id}`.toLowerCase().includes(query().toLowerCase()))
    .sort((a, b) => Number(taskActive(b)) - Number(taskActive(a)) || (b.state.started_at ?? '').localeCompare(a.state.started_at ?? '')));
  const selected = () => all().find(task => task.id === open());
  const select = (task: Task) => { setRetrieved(task); setOpen(task.id); };
  return <div class="tasks" classList={{ 'task-selected': Boolean(selected()) }}>
    <Show when={selected()?.id} keyed>{id => <TaskItem client={client} task={all().find(task => task.id === id)!} back={() => setOpen('')} select={select} refresh={refresh} />}</Show>
    <Show when={!selected()}><div class="panel-scroll task-list">
    <div class="toolbar">
      <Segmented label="Task scope" value={scope()} onChange={setScope} options={[{ value: 'session', label: 'Session' }, { value: 'workspace', label: 'Workspace' }]} />
      <Segmented label="Task filter" value={filter()} onChange={setFilter} options={[{ value: 'all', label: `All ${scoped().length}` }, { value: 'active', label: `Running ${scoped().filter(taskActive).length}` }]} />
    </div>
    <div class="search"><Icon name="search" size={14} /><input aria-label="Search tasks" placeholder="Search tasks" value={query()} onInput={event => setQuery(event.currentTarget.value)} /></div>
    <Show when={selectedLoading()}><p class="muted" role="status">Loading selected task…</p></Show>
    <Show when={selectedError()}><div class="callout"><p class="form-error" role="alert">{selectedError()}</p><button disabled={!state.connected} onClick={() => setSelectedRevision(value => value + 1)}>Retry selected task</button></div></Show>
    <Show when={state.tasksError}><p class="form-error" role="alert">{state.tasksError}</p></Show>
    <Show when={detachError()}><p class="form-error" role="alert">Foreground jobs: {detachError()}</p></Show>
    <Show when={detachable() > 0}><button class="wide" disabled={detaching() || !state.connected} onClick={() => {
      if (detaching() || !state.connected) return;
      const current = generation;
      const route = api.route(`/agent/jobs/detach?session_id=${encodeURIComponent(state.sessionID)}`);
      setDetaching(true);
      void api.request(route, 'POST').then(async () => { if (current === generation) await refresh(); }).catch(reason => { if (current === generation) setDetachError(errorText(reason)); }).finally(() => { if (current === generation) setDetaching(false); });
    }}><Icon name="bolt" size={14} />Move {detachable()} foreground {detachable() === 1 ? 'job' : 'jobs'} to the background</button></Show>
    <For each={tasks()} fallback={<Empty icon="bolt" title={query() || filter() !== 'all' ? 'No matching tasks' : scope() === 'session' ? 'No tasks in this session' : 'No workspace tasks'}>Background shells and agents appear here.</Empty>}>{task => <button class="task-row" data-task-id={task.id} onClick={() => select(task)}><span class={`status-dot ${task.state.status}`} /><span class="truncate">{task.type === 'shell' ? task.command?.trim().match(/^(?:"[^"]+"|'[^']+'|\S+)/)?.[0] || task.description || 'Shell' : task.description || task.agent_type || task.type}</span><time>{(tick(), duration(task))}</time></button>}</For>
    </div></Show>
  </div>;
}
