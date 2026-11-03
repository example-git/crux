import { batch } from 'solid-js';
import { createStore, reconcile, unwrap } from 'solid-js/store';
import { CruxClient } from './api.ts';
import { coalesceEvents, resolveSession } from './events.ts';
import { taskNotification } from './notifications.ts';
import { readStored, writeStored, writeStoredSoon } from './storage.ts';
import type { Agent, DeliveryMode, Event, InputPart, Message, Models, ModelSlot, Permission, Provider, Question, QueuedPrompt, SelectedModel, Session, Startup, Task, UsageSnapshot, Workspace } from './types.ts';

export const planModes = ['plan', 'plan_revision', 'plan_execution'];
export const isPlanMode = (mode?: string) => planModes.includes(mode ?? '');
export type Toast = { id: number; kind: 'info' | 'error' | 'success'; text: string };
export type RecentError = { id: number; text: string; time: number; key?: string; workspace?: string; sessionID?: string };
const errorRetention = 7 * 24 * 60 * 60 * 1000;

export type GoalCommand = { kind: 'set'; objective: string } | { kind: 'status' | 'pause' | 'stop' | 'resume' | 'complete' | 'clear' };

export function parseGoalCommand(input: string): GoalCommand | undefined {
  const trimmed = input.trim();
  if (trimmed !== '/goal' && !trimmed.startsWith('/goal ')) return undefined;
  let args = trimmed.slice('/goal'.length).trim();
  if (!args || args === 'status') return { kind: 'status' };
  if (['pause', 'stop', 'resume', 'complete', 'clear'].includes(args)) return { kind: args as 'pause' };
  if (args === 'set') throw new Error('Goal objective is required.');
  if (args.startsWith('set ')) args = args.slice(4).trim();
  if (!args) throw new Error('Goal objective is required.');
  return { kind: 'set', objective: args };
}

export function goalNotice(command: GoalCommand, session: Session) {
  if (command.kind === 'stop') return 'Goal stopped. Use /goal resume to continue.';
  if (command.kind === 'clear') return 'Goal cleared.';
  const goal = session.goal;
  if (!goal?.objective) return 'No active goal.';
  return `Goal ${goal.status} (generation ${goal.generation}): ${goal.objective}${goal.status === 'blocked' && goal.error ? `\n${goal.error}` : ''}`;
}

export function createCruxState() {
  const api = new CruxClient();
  const [state, set] = createStore({
    startup: undefined as Startup | undefined,
    workspace: undefined as Workspace | undefined,
    sessions: [] as Session[],
    tasks: [] as Task[],
    tasksError: '',
    providerUsage: {} as Record<string, UsageSnapshot>,
    messages: {} as Record<string, Message>,
    messageIDs: [] as string[],
    hasOlderMessages: false,
    messageRevision: 0,
    sessionID: '',
    createdSessionID: '',
    currentSession: undefined as Session | undefined,
    deliveryMode: 'queue' as DeliveryMode, deliverySaving: false, autoApprove: false, modeSaving: false,
    queued: [] as QueuedPrompt[],
    activityRevision: 0,
    agent: undefined as Agent | undefined,
    permission: null as Permission | null,
    question: null as Question | null,
    connected: false, loading: true, archived: false, error: '', notice: '', sending: false,
    toasts: [] as Toast[],
    recentErrors: [] as RecentError[],
    notificationsEnabled: false, notificationsBusy: false, notificationError: '',
    notificationPermission: (typeof Notification === 'undefined' ? 'unsupported' : Notification.permission) as NotificationPermission | 'unsupported',
    servicesRevision: 0, skillsRevision: 0, fileRevision: 0,
  });
  const restoredErrors = readStored<RecentError[]>('errors', errorRetention) ?? [];
  if (restoredErrors.length) set('recentErrors', restoredErrors.slice(-50));
  let toastID = 0;
  let errorID = restoredErrors.reduce((highest, item) => Math.max(highest, item.id), 0);
  function saveErrors() { writeStoredSoon('errors', () => state.recentErrors.length ? unwrap(state.recentErrors) : undefined); }
  if (readStored<boolean>('notifications')) {
    if (typeof Notification !== 'undefined' && Notification.permission === 'granted') set('notificationsEnabled', true);
    else set('notificationError', 'Notifications were enabled before this page reloaded, but browser permission is not granted. Enable them again to retry.');
  }
  function recordError(text: string, key?: string, time = Date.now(), sessionID?: string) {
    if (!text.trim()) return;
    const previous = key ? state.recentErrors.find(item => item.key === key) : undefined;
    if (previous?.text === text) return;
    const record: RecentError = { id: previous?.id ?? ++errorID, text, time, key, workspace: state.workspace?.path, sessionID };
    set('recentErrors', items => [...items.filter(item => item.id !== record.id).slice(-49), record]);
    saveErrors();
  }
  function recordMessageError(message: Message) {
    if (message.role !== 'assistant') return;
    const finish = message.parts.find(part => part.type === 'finish')?.data;
    if (!finish || !['error', 'content_filter'].includes(finish.reason)) return;
    const text = [finish.message, finish.details].filter(value => typeof value === 'string' && value.trim()).join('\n') || finish.reason;
    recordError(text, JSON.stringify([state.workspace?.path, message.session_id, message.id]), finish.time > 0 ? finish.time * 1000 : Date.now(), message.session_id);
  }
  const clearErrors = () => { set('recentErrors', []); saveErrors(); };
  function toast(text: string, kind: Toast['kind'] = 'info') {
    if (kind === 'error') recordError(text);
    const id = ++toastID;
    set('toasts', items => [...items.slice(-3), { id, kind, text }]);
    setTimeout(() => set('toasts', items => items.filter(item => item.id !== id)), kind === 'error' ? 9000 : 4500);
  }
  const dismissToast = (id: number) => set('toasts', items => items.filter(item => item.id !== id));
  let queue: Event[] = [];
  let frame = 0;
  let syncing = false;
  let snapshots = 0;
  let selection = 0;
  let epoch = 0;
  let stopped = false;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let retryDelay = 500;
  let requestedPath: string | undefined;
  let requestedDataDir: string | undefined;
  let requestedConnection = '';
  let connecting: Promise<void> | undefined;
  let switching = false;
  let messageRead = 0;
  let pendingRead = 0;
  let sessionRead = 0;
  let queueRead = 0;
  let workspaceRead = 0;
  let taskRead = 0;

  const fail = (error: unknown) => {
    const text = error instanceof Error ? error.message : String(error);
    recordError(text);
    set('error', text);
  };
  const attempt = async (action: () => Promise<unknown>) => {
    set('error', '');
    try { await action(); } catch (error) { fail(error); }
  };

  let notificationGeneration = 0;
  const notifications = new Set<Notification>();
  const notified = new Set<string>();
  function closeNotifications() {
    for (const notification of notifications) notification.close();
    notifications.clear();
  }
  function closeNotification(identity: string) {
    const tag = JSON.stringify([state.workspace?.id, identity]);
    for (const notification of notifications) if (notification.tag === tag) { notification.close(); notifications.delete(notification); }
  }
  async function setNotifications(enabled: boolean) {
    const generation = ++notificationGeneration;
    set('notificationError', '');
    set('notificationsEnabled', false);
    set('notificationsBusy', false);
    if (!enabled) { writeStored('notifications', false); closeNotifications(); return; }
    if (typeof Notification === 'undefined') {
      set('notificationPermission', 'unsupported');
      set('notificationError', 'This browser does not support desktop notifications.');
      return;
    }
    set('notificationsBusy', true);
    try {
      const permission = Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission();
      if (stopped || generation !== notificationGeneration) return;
      set('notificationPermission', permission);
      set('notificationsEnabled', permission === 'granted');
      writeStored('notifications', permission === 'granted');
      if (permission !== 'granted') set('notificationError', permission === 'denied' ? 'Notifications are blocked. Allow them in your browser or desktop app settings, then enable them here.' : 'Notification permission was not granted.');
    } catch (error) {
      if (!stopped && generation === notificationGeneration) set('notificationError', error instanceof Error ? error.message : String(error));
    } finally { if (!stopped && generation === notificationGeneration) set('notificationsBusy', false); }
  }
  function notify(title: string, body: string, identity: string | undefined, sessionID?: string, preview = false) {
    if (stopped || !state.notificationsEnabled) return;
    if (typeof Notification === 'undefined' || Notification.permission !== 'granted') {
      set('notificationsEnabled', false);
      writeStored('notifications', false);
      set('notificationPermission', typeof Notification === 'undefined' ? 'unsupported' : Notification.permission);
      set('notificationError', 'Notification permission is no longer available. Enable notifications again to retry.');
      closeNotifications();
      return;
    }
    const workspaceID = state.workspace?.id;
    const workspacePath = state.workspace?.path;
    const key = identity ? JSON.stringify([workspaceID, identity]) : undefined;
    if (!preview) {
      if (key && notified.has(key)) return;
      if (key) notified.add(key);
      if (notified.size > 500) notified.delete(notified.values().next().value!);
      if (document.visibilityState === 'visible' && document.hasFocus()) return;
    }
    try {
      const notification = new Notification(title, { body, tag: preview ? undefined : key });
      notifications.add(notification);
      notification.onclose = () => notifications.delete(notification);
      notification.onerror = () => {
        notification.close(); notifications.delete(notification);
        if (!preview && key) notified.delete(key);
        if (!stopped) set('notificationError', 'The browser could not display the notification. Check system notification settings.');
      };
      notification.onclick = () => {
        notification.close(); notifications.delete(notification);
        if (stopped) return;
        window.focus();
        if (workspaceID !== state.workspace?.id || workspaceID !== api.workspace?.id) {
          toast(`This notification belongs to ${workspacePath || 'a different workspace'}. Open that workspace to inspect it.`);
          return;
        }
        if (sessionID && sessionID !== state.sessionID) void attempt(() => selectSession(sessionID));
      };
      set('notificationError', '');
    } catch (error) {
      if (!preview && key) notified.delete(key);
      set('notificationError', error instanceof Error ? error.message : String(error));
    }
  }
  const testNotification = () => notify('Crux notifications', 'Notifications are enabled for this tab.', 'preview', undefined, true);
  const notifyPermission = (permission: Permission) => notify('Crux needs permission', `Permission required to execute ${permission.tool_name}.`, `permission:${permission.tool_call_id || permission.id}`, permission.session_id);
  const notifyQuestion = (question: Question) => notify('Crux needs your input', `${question.questions.length} questions need your input.`, `question:${question.id}`, question.session_id);

  function notifyEvent(event: Event) {
    const value = event.payload.payload;
    if (event.type === 'permission_request') notifyPermission(value);
    if (event.type === 'question_batch_request') notifyQuestion(value);
    if (event.type === 'permission_notification' && (value.granted || value.denied)) closeNotification(`permission:${value.tool_call_id}`);
    if (event.type === 'question_batch_notification') closeNotification(`question:${value.batch_id}`);
    if (event.type === 'task_notification' && event.payload.type !== 'deleted') notify('Crux background task finished', value.summary || `Task ${value.task_id}: ${value.status}`, `task:${value.notification_id || `${value.task_id}:${value.status}`}`, value.parent_session_id);
    if (event.type === 'run_complete') notify(value.cancelled ? 'Crux turn stopped' : value.error ? 'Crux turn failed' : 'Crux turn complete', value.error || (value.cancelled ? 'The turn was stopped.' : state.sessions.find(session => session.id === value.session_id)?.title || 'Your response is ready.'), value.run_id || value.message_id ? `run:${value.session_id}:${value.run_id || value.message_id}` : undefined, value.session_id);
  }

  function apply(event: Event) {
    const value = event.payload.payload;
    switch (event.type) {
      case 'message':
        if (event.payload.type !== 'deleted') recordMessageError(value);
        if (event.payload.type === 'created' && ['user', 'assistant'].includes(value.role)) refreshSoon('sessions', refreshSessions);
        if (value.session_id !== state.sessionID) break;
        if (event.payload.type === 'deleted') {
          set('messageIDs', ids => ids.filter(id => id !== value.id));
          set('messages', value.id, undefined!);
        } else {
          if (!state.messages[value.id]) set('messageIDs', ids => [...ids, value.id]);
          set('messages', value.id, reconcile(structuredClone(value)));
        }
        set('messageRevision', revision => revision + 1);
        break;
      case 'session': {
        if (value.id === state.sessionID && state.currentSession) {
          const { is_busy: _, attached_clients: __, ...persisted } = value;
          if (event.payload.type === 'deleted') { void selectSession('').catch(fail); }
          else set('currentSession', persisted);
        }
        const index = state.sessions.findIndex(item => item.id === value.id);
        if (event.payload.type === 'deleted' || value.parent_session_id || Boolean(value.archived) !== state.archived) {
          if (index >= 0) set('sessions', items => items.filter(item => item.id !== value.id));
        } else if (index >= 0) {
          const { is_busy: _, attached_clients: __, ...persisted } = value;
          set('sessions', index, persisted);
        } else set('sessions', items => [...items, value]);
        break;
      }
      case 'session_busy': {
        const index = state.sessions.findIndex(item => item.id === value.session_id);
        if (index >= 0) set('sessions', index, 'is_busy', Boolean(value.busy));
        if (state.currentSession?.id === value.session_id) {
          set('currentSession', 'is_busy', Boolean(value.busy));
          refreshSoon('queue', refreshQueue);
        }
        break;
      }
      case 'permission_request':
        set('permission', value);
        break;
      case 'permission_notification':
        if ((value.granted || value.denied) && state.permission?.tool_call_id === value.tool_call_id) set('permission', null);
        break;
      case 'question_batch_request':
        set('question', reconcile(value));
        break;
      case 'question_batch_notification':
        if (state.question?.id === value.batch_id) set('question', null);
        break;
      case 'task_notification':
        refreshSoon('tasks', refreshTasks);
        set('activityRevision', revision => revision + 1);
        break;
      case 'agent_event':
        if (value.type === 'usage' && value.provider_id && value.owner?.provider_id === value.provider_id && value.usage && state.workspace) {
          set('providerUsage', value.provider_id, reconcile({ connection: api.connection, workspaceID: state.workspace.id, owner: value.owner, revision: value.usage.revision ?? 0, digest: value.usage.digest ?? '', usage: value.usage.usage ?? undefined }));
        }
        if (value.error) fail(value.error);
        else if (value.type === 'summarize' && value.session_id === state.sessionID) { refreshSoon(`messages:${value.session_id}`, () => reloadCurrent(value.session_id)); refreshSoon('sessions', refreshSessions); }
        break;
      case 'mcp_event': case 'lsp_event': set('servicesRevision', revision => revision + 1); break;
      case 'skills_event': set('skillsRevision', revision => revision + 1); break;
      case 'file': set('fileRevision', revision => revision + 1); set('activityRevision', revision => revision + 1); break;
      case 'client_refresh': refreshSoon('workspace', refreshWorkspace); break;
      case 'run_complete':
        refreshSoon('tasks', refreshTasks);
        set('activityRevision', revision => revision + 1);
        refreshSoon('queue', refreshQueue);
        if (value.error) fail(value.error);
        refreshSoon('sessions', refreshSessions);
        if (value.session_id === state.sessionID) {
          refreshSoon(`messages:${value.session_id}`, () => reloadCurrent(value.session_id));
          refreshSoon('pending', refreshPending);
        }
        break;
      case 'config_changed': set('providerUsage', reconcile({})); refreshSoon('workspace', refreshWorkspace); break;
    }
  }

  const scheduledRefreshes = new Set<string>();
  function refreshSoon(key: string, run: () => Promise<unknown>) {
    if (scheduledRefreshes.has(key)) return;
    scheduledRefreshes.add(key);
    const currentEpoch = epoch;
    queueMicrotask(() => {
      scheduledRefreshes.delete(key);
      if (!stopped && currentEpoch === epoch) void run().catch(fail);
    });
  }
  const reloadCurrent = async (id: string) => { if (id === state.sessionID) await loadMessages(id); };

  function flush() {
    frame = 0;
    if (syncing || snapshots) return;
    const events = queue;
    queue = [];
    batch(() => coalesceEvents(events).forEach(apply));
  }

  function enqueue(event: Event) {
    notifyEvent(event);
    const mirrored = mirrorEvent(event);
    if (!mirrored) return;
    queue.push(mirrored);
    if (!frame && !syncing && !snapshots) frame = requestAnimationFrame(flush);
  }

  type Mirrored = { message: Message; revision: number };
  const mirrorLimit = 1024;
  let mirror = new Map<string, Mirrored>();
  const mirrorKey = (sessionID: string, id: string) => `${sessionID}\u0000${id}`;
  function remember(key: string, message: Message, revision: number) {
    mirror.delete(key);
    if (message.parts.some(part => part.type === 'finish')) return;
    mirror.set(key, { message, revision });
    if (mirror.size > mirrorLimit) mirror.delete(mirror.keys().next().value!);
  }
  function mirrorEvent(event: Event): Event | undefined {
    if (event.type === 'message') {
      const value = event.payload.payload;
      const key = mirrorKey(value.session_id, value.id);
      if (event.payload.type === 'deleted') { mirror.delete(key); return event; }
      const { delta_revision: revision, ...message } = value;
      if (typeof revision === 'number') remember(key, message, revision);
      return { type: 'message', payload: { type: event.payload.type, payload: message } };
    }
    if (event.type !== 'message_delta') return event;
    const delta = event.payload.payload as { id: string; session_id: string; updated_at: number; base: number; revision: number; length: number; parts?: { index: number; part?: Message['parts'][number]; field?: string; append?: string }[] };
    const key = mirrorKey(delta.session_id, delta.id);
    const current = mirror.get(key);
    const resync = () => {
      mirror.delete(key);
      if (delta.session_id === state.sessionID) refreshSoon(`messages:${delta.session_id}`, () => reloadCurrent(delta.session_id));
      return undefined;
    };
    if (!current || current.revision !== delta.base || delta.length < current.message.parts.length) return resync();
    const parts = current.message.parts.slice();
    for (const change of delta.parts ?? []) {
      if (change.part) { parts[change.index] = change.part; continue; }
      const previous = parts[change.index];
      const field = change.field ?? '';
      if (!previous || typeof previous.data?.[field] !== 'string' || typeof change.append !== 'string') return resync();
      parts[change.index] = { ...previous, data: { ...previous.data, [field]: previous.data[field] + change.append } };
    }
    if (parts.length !== delta.length || [...parts].some(part => !part)) return resync();
    const message = { ...current.message, updated_at: delta.updated_at, parts };
    remember(key, message, delta.revision);
    return { type: 'message', payload: { type: 'updated', payload: message } };
  }

  async function snapshot<T>(read: () => Promise<T>): Promise<T> {
    snapshots++;
    try { return await read(); }
    finally { snapshots--; if (!syncing && !snapshots && queue.length && !frame) frame = requestAnimationFrame(flush); }
  }

  async function refreshTasks() {
    const workspace = api.workspace?.id;
    if (!workspace) return;
    const read = ++taskRead;
    const currentEpoch = epoch;
    const current = () => !stopped && read === taskRead && currentEpoch === epoch && workspace === api.workspace?.id;
    try {
      const tasks = await api.request<Task[]>(`/v1/workspaces/${workspace}/tasks`);
      if (current()) batch(() => { set('tasks', reconcile(tasks ?? [])); set('tasksError', ''); });
    } catch (reason) { if (current()) set('tasksError', reason instanceof Error ? reason.message : String(reason)); }
  }

  async function refreshWorkspace() {
    const read = ++workspaceRead;
    const id = api.workspace?.id;
    if (!id) return;
    const [workspace, agent, permissions] = await Promise.all([
      api.request<Workspace>(`/v1/workspaces/${id}`),
      api.request<Agent>(`/v1/workspaces/${id}/agent`),
      api.request<{ skip: boolean }>(`/v1/workspaces/${id}/permissions/skip`),
    ]);
    if (api.workspace?.id !== id || read !== workspaceRead) return;
    const delivery = workspace.config.options?.tui?.delivery_mode ?? 'queue';
    if (delivery !== 'queue' && delivery !== 'steer') throw new Error(`Invalid native delivery mode: ${delivery}`);
    workspace.connection_name = api.connection;
    api.workspace = workspace;
    batch(() => { set('workspace', reconcile(workspace)); set('agent', reconcile(agent)); set('autoApprove', Boolean(permissions.skip)); if (!state.deliverySaving) set('deliveryMode', delivery); });
  }

  async function refreshSessions() {
    const read = ++sessionRead;
    const id = api.workspace?.id;
    const archived = state.archived;
    if (!id) return;
    const sessions = await api.request<Session[]>(`/v1/workspaces/${id}/sessions?archived=${archived}`);
    if (read === sessionRead && id === api.workspace?.id && archived === state.archived) {
      set('sessions', reconcile(sessions ?? []));
      const current = sessions?.find(item => item.id === state.sessionID);
      if (current) set('currentSession', reconcile(current));
      else if (state.sessionID) {
        const selected = state.sessionID;
        const current = await api.request<Session>(api.route(`/sessions/${selected}`));
        if (read === sessionRead && id === api.workspace?.id && selected === state.sessionID) set('currentSession', reconcile(current));
      }
    }
  }

  async function refreshPending() {
    const read = ++pendingRead;
    return snapshot(async () => {
      const id = api.workspace?.id;
      if (!id) return;
      const pending = await api.request<{ permission: Permission | null; question: Question | null }>(`/ui/workspaces/${id}/pending`);
      if (read === pendingRead && id === api.workspace?.id) batch(() => {
        set('permission', pending.permission); set('question', reconcile(pending.question));
        if (pending.permission) notifyPermission(pending.permission);
        if (pending.question) notifyQuestion(pending.question);
      });
    });
  }

  const messagePageSize = 120;
  let olderRead = 0;
  async function loadMessages(id: string) {
    const read = ++messageRead;
    return snapshot(async () => {
      const currentSelection = selection;
      const workspaceID = api.workspace?.id;
      const { value, headers } = await api.exchange<Message[]>(api.route(`/sessions/${id}/messages?limit=${messagePageSize}`));
      if (read !== messageRead || id !== state.sessionID || workspaceID !== api.workspace?.id || currentSelection !== selection) return;
      const latest = value ?? [];
      const more = headers.get('Crux-Messages-More') === 'true';
      const anchor = latest.length ? state.messageIDs.indexOf(latest[0].id) : -1;
      const kept = more && anchor > 0 ? state.messageIDs.slice(0, anchor) : [];
      batch(() => {
        latest.forEach(recordMessageError);
        set('messages', reconcile(Object.fromEntries([...kept.map(key => [key, unwrap(state.messages[key])]), ...latest.map(message => [message.id, message])])));
        set('messageIDs', [...kept, ...latest.map(message => message.id)]);
        if (!kept.length) set('hasOlderMessages', more);
        set('messageRevision', revision => revision + 1);
      });
    });
  }

  async function loadOlderMessages(all = false) {
    const id = state.sessionID;
    const before = state.messageIDs[0];
    if (!id || !before || !state.hasOlderMessages) return 0;
    const read = ++olderRead;
    const currentSelection = selection;
    const workspaceID = api.workspace?.id;
    const query = all ? '' : `?limit=${messagePageSize}&before=${encodeURIComponent(before)}`;
    const { value, headers } = await api.exchange<Message[]>(api.route(`/sessions/${id}/messages${query}`));
    if (read !== olderRead || id !== state.sessionID || workspaceID !== api.workspace?.id || currentSelection !== selection || state.messageIDs[0] !== before) return 0;
    let older = value ?? [];
    if (all) {
      const index = older.findIndex(message => message.id === before);
      if (index < 0) throw new Error('The loaded conversation no longer matches the native history. Reload the session.');
      older = older.slice(0, index);
    }
    const known = new Set(state.messageIDs);
    older = older.filter(message => !known.has(message.id));
    batch(() => {
      older.forEach(recordMessageError);
      for (const message of older) set('messages', message.id, message);
      set('messageIDs', ids => [...older.map(message => message.id), ...ids]);
      set('hasOlderMessages', all ? false : headers.get('Crux-Messages-More') === 'true');
      set('messageRevision', revision => revision + 1);
    });
    return older.length;
  }

  async function selectSession(id: string, created = false) {
    const workspaceID = api.workspace?.id;
    const currentSelection = ++selection;
    const current = () => currentSelection === selection && workspaceID === api.workspace?.id;
    batch(() => { set('createdSessionID', created ? id : ''); set('currentSession', state.sessions.find(item => item.id === id)); set('sessionID', id); set('messageIDs', []); set('hasOlderMessages', false); set('messages', {}); set('queued', []); });
    history.replaceState(null, '', id ? `#session=${encodeURIComponent(id)}` : location.pathname);
    await api.selectSession(id);
    if (id && current()) {
      if (!state.currentSession) {
        const session = await api.request<Session>(api.route(`/sessions/${id}`));
        if (!current()) return;
        set('currentSession', session);
      }
      await Promise.all([loadMessages(id), refreshQueue()]);
    }
  }

  function scheduleReconnect() {
    if (stopped || reconnectTimer) return;
    set('connected', false);
    set('providerUsage', reconcile({}));
    set('notice', 'Connection lost. Reconnecting and restoring current state…');
    reconnectTimer = setTimeout(() => {
      reconnectTimer = undefined;
      void connect(false).catch(error => { fail(error); scheduleReconnect(); });
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 5000);
  }

  function connect(initial: boolean, path?: string, dataDir?: string, connection?: string): Promise<void> {
    if (connecting) return connecting;
    connecting = establish(initial, path, dataDir, connection).finally(() => { connecting = undefined; });
    return connecting;
  }

  async function establish(initial: boolean, path?: string, dataDir?: string, connection?: string) {
    clearTimeout(reconnectTimer);
    reconnectTimer = undefined;
    const currentEpoch = ++epoch;
    set('deliverySaving', false);
    set('modeSaving', false);
    set('sending', false);
    syncing = true;
    queue = [];
    set('loading', true);
    set('connected', false);
    set('providerUsage', reconcile({}));
    try {
      const startup = state.startup ?? await api.request<Startup>('/ui/bootstrap');
      set('startup', startup);
      if (path !== undefined) {
        requestedPath = path;
        requestedConnection = connection ?? requestedConnection;
        requestedDataDir = dataDir ?? (!requestedConnection && path === startup.path ? startup.data_dir : '');
      }
      requestedPath ??= startup.path;
      requestedDataDir ??= startup.data_dir ?? '';
      const workspace = await api.attach(startup, requestedPath, requestedDataDir, requestedConnection);
      if (currentEpoch !== epoch) return;
      set('workspace', reconcile(workspace));
      mirror = new Map();
      let eventStreamClosed = false;
      await api.connect(event => { if (currentEpoch === epoch) enqueue(event); }, () => {
        eventStreamClosed = true;
        if (currentEpoch === epoch) scheduleReconnect();
      });
      await refreshWorkspace();
      if (!state.agent?.is_ready && workspace.config.models?.large) {
        try { await api.request(api.route('/agent/init'), 'POST', { interactive: true }); }
        catch (error) { fail(error); }
        await refreshWorkspace();
      }
      await Promise.all([refreshSessions(), refreshPending(), refreshTasks()]);
      let sessionID = state.sessionID;
      if (initial && !requestedConnection && requestedPath === startup.path) {
        const hash = new URLSearchParams(location.hash.slice(1)).get('session');
        const requested = startup.session_id || hash;
        if (requested) {
          if (state.sessions.some(session => session.id.startsWith(requested))) sessionID = resolveSession(state.sessions, requested);
          else {
            const session = await api.request<Session>(api.route(`/sessions/${encodeURIComponent(requested)}`));
            if (session.id !== requested) throw new Error(`Session ${requested} was not found in this workspace.`);
            sessionID = session.id;
          }
        } else if (startup.continue) sessionID = [...state.sessions].sort((a, b) => b.updated_at - a.updated_at)[0]?.id ?? '';
      }
      await selectSession(sessionID);
      if (currentEpoch !== epoch) return;
      if (eventStreamClosed) throw new Error('Workspace event stream closed while restoring state.');
      retryDelay = 500;
      set('connected', true);
      set('notice', '');
    } finally {
      if (currentEpoch === epoch) {
        syncing = false;
        set('loading', false);
        flush();
      }
    }
  }

  async function newSession() {
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const previousSelection = selection;
    const session = await api.request<Session>(api.route('/sessions'), 'POST', { title: 'New session' });
    if (currentEpoch === epoch && workspaceID === api.workspace?.id) {
      if (state.archived && previousSelection === selection) set('archived', false);
      await refreshSessions();
      if (previousSelection === selection) await selectSession(session.id, true);
    }
    return session.id;
  }

  async function send(prompt: string, attachments: unknown[] = [], inputParts: InputPart[] = []) {
    const deliveryMode = state.deliveryMode;
    if (deliveryMode !== 'queue' && deliveryMode !== 'steer') throw new Error(`Invalid delivery mode: ${deliveryMode}`);
    if (!state.connected) throw new Error('Wait for the workspace to reconnect before sending.');
    if (!prompt.trim() && !attachments.length && !inputParts.length) return;
    const workspaceRoot = api.route('');
    const agentPath = `${workspaceRoot}/agent`;
    const workspaceID = api.workspace?.id;
    const connection = api.connection;
    const currentEpoch = epoch;
    const current = () => currentEpoch === epoch && workspaceID === api.workspace?.id && connection === api.connection;
    set('sending', true);
    try {
      const id = state.sessionID || await newSession();
      if (currentEpoch !== epoch) throw new Error('The workspace changed before the message could be sent.');
      const index = state.sessions.findIndex(session => session.id === id);
      if (workspaceID === api.workspace?.id && index >= 0) set('sessions', index, 'is_busy', true);
      if (workspaceID === api.workspace?.id && state.currentSession?.id === id) set('currentSession', 'is_busy', true);
      try {
        await api.request(agentPath, 'POST', { session_id: id, prompt, attachments, delivery_mode: deliveryMode, input_parts: inputParts.length ? [{ type: 'text', text: prompt }, ...inputParts] : undefined, submission_id: crypto.randomUUID(), run_id: deliveryMode === 'queue' ? crypto.randomUUID() : undefined, wait_for_admission: false });
      } finally { if (current()) await Promise.all([refreshSessions(), refreshQueue()]).catch(error => { if (current()) fail(error); }); }
      const paths = [...new Set(attachments.flatMap(attachment => attachment && typeof attachment === 'object' && 'file_path' in attachment && typeof attachment.file_path === 'string' && attachment.file_path ? [attachment.file_path] : []))];
      await Promise.all(paths.map(async path => {
        await Promise.all([
          api.request(`${workspaceRoot}/filetracker/read`, 'POST', { session_id: id, path }),
          api.request(`${workspaceRoot}/lsps/start`, 'POST', { path }),
        ]);
      })).catch(error => { if (current()) fail(error); });
      if (current() && paths.length) set('fileRevision', revision => revision + 1);
      return id;
    } finally { if (current()) set('sending', false); }
  }

  async function setDeliveryMode(mode: DeliveryMode) {
    if (mode !== 'queue' && mode !== 'steer') throw new Error(`Invalid delivery mode: ${mode}`);
    if (state.deliverySaving) throw new Error('Delivery preference is still saving.');
    const currentEpoch = epoch;
    set('deliverySaving', true);
    try {
      await api.request(api.route('/config/set'), 'POST', { scope: 0, key: 'options.tui.delivery_mode', value: mode });
      if (currentEpoch !== epoch) return;
      workspaceRead++;
      set('deliveryMode', mode);
    } finally { if (currentEpoch === epoch) set('deliverySaving', false); }
    await refreshWorkspace();
    if (currentEpoch === epoch && state.deliveryMode !== mode) throw new Error('The native delivery preference did not match the selected mode.');
  }

  async function refreshQueue() {
    const read = ++queueRead;
    const id = state.sessionID;
    const workspaceID = api.workspace?.id;
    if (!id || !workspaceID) { set('queued', []); return; }
    const prompts = await api.request<QueuedPrompt[]>(api.route(`/agent/sessions/${id}/prompts/list`));
    if (read === queueRead && id === state.sessionID && workspaceID === api.workspace?.id) set('queued', reconcile(prompts ?? []));
  }

  async function clearQueue() {
    const currentEpoch = epoch;
    const id = state.sessionID;
    const workspaceID = api.workspace?.id;
    await api.request(api.route(`/agent/sessions/${id}/prompts/clear`), 'POST');
    if (currentEpoch !== epoch) return;
    if (id === state.sessionID && workspaceID === api.workspace?.id) { queueRead++; set('queued', []); }
    await refreshQueue();
  }

  async function updateSession(field: 'title' | 'mode' | 'analysis-effort', value: string) {
    const currentEpoch = epoch;
    const base = api.route('/sessions');
    const id = state.sessionID || await newSession();
    if (currentEpoch !== epoch) throw new Error('The workspace changed during the session update.');
    await api.request(`${base}/${id}/${field}`, 'PUT', { [field.replace('-', '_')]: value });
    if (currentEpoch === epoch) await refreshSessions();
  }

  async function forkSession(id = state.sessionID) {
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const previousSelection = selection;
    const session = await api.request<Session>(api.route(`/sessions/${id}/fork?record_origin=true`), 'POST');
    if (currentEpoch !== epoch || workspaceID !== api.workspace?.id) return;
    if (previousSelection === selection) set('archived', false);
    await refreshSessions();
    if (currentEpoch === epoch && workspaceID === api.workspace?.id && previousSelection === selection) await selectSession(session.id);
  }

  async function archiveSession(id = state.sessionID, archived = state.currentSession?.archived) {
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const previousSelection = selection;
    await api.request(api.route(`/sessions/${id}/${archived ? 'unarchive' : 'archive'}`), 'POST');
    if (currentEpoch !== epoch || workspaceID !== api.workspace?.id) return;
    if (id === state.sessionID && previousSelection === selection) await selectSession('');
    if (currentEpoch === epoch && workspaceID === api.workspace?.id) await refreshSessions();
  }

  async function rewind(messageID: string, restoreFiles: boolean, summarizeFirst = false) {
    const currentEpoch = epoch;
    const id = state.sessionID;
    const workspaceID = state.workspace?.id;
    await api.request(api.route(`/sessions/${id}/rewind`), 'POST', { message_id: messageID, summarize: summarizeFirst, restore_files: restoreFiles });
    if (currentEpoch !== epoch || state.workspace?.id !== workspaceID || state.sessionID !== id) return;
    await Promise.all([loadMessages(id), refreshSessions(), refreshQueue()]);
    if (currentEpoch === epoch && state.workspace?.id === workspaceID && state.sessionID === id) set('activityRevision', revision => revision + 1);
  }

  async function rollback(messageID: string) {
    const currentEpoch = epoch;
    const id = state.sessionID;
    const workspaceID = api.workspace?.id;
    if (!id) throw new Error('Open a session to roll back.');
    await api.request(api.route(`/sessions/${id}/rollback`), 'POST', { message_id: messageID });
    if (currentEpoch !== epoch || api.workspace?.id !== workspaceID || state.sessionID !== id) return;
    await Promise.all([loadMessages(id), refreshSessions(), refreshQueue()]);
    if (currentEpoch === epoch && api.workspace?.id === workspaceID && state.sessionID === id) set('activityRevision', revision => revision + 1);
  }

  async function bindSessionModels(id: string, slot: 'large' | 'small', chosen: ModelSlot, returned: Models) {
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const route = api.route(`/sessions/${id}`);
    const session = state.currentSession?.id === id ? state.currentSession : await api.request<Session>(route);
    const base: Models = session.models && (session.models.large || session.models.small) ? { ...session.models } : { ...returned };
    base[slot] = chosen;
    try {
      const saved = await api.request<Session>(route, 'PUT', { model_selection: { models: base, revision: session.model_revision ?? 0 } });
      if (currentEpoch === epoch && api.workspace?.id === workspaceID && state.sessionID === id) set('currentSession', reconcile(saved));
    } catch (error) {
      if ((error as { status?: number }).status === 409) throw new Error('This session’s model changed elsewhere. The latest state was reloaded; choose the model again.');
      throw error;
    }
  }

  async function chooseModel(provider: Provider, model: SelectedModel, scope: number | 'runtime', slot: 'large' | 'small' = 'large') {
    if (!provider.owner) throw new Error('The provider has no native owner.');
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const sessionID = state.sessionID;
    const root = api.route('');
    const models = scope === 'runtime'
      ? await api.request<Models>(`${root}/config/model-overrides`, 'POST', { state: { [slot]: { model, owner: provider.owner } } })
      : await api.request<Models>(`${root}/config/model`, 'POST', { scope, model_type: slot, model, owner: provider.owner });
    try {
      if (currentEpoch !== epoch || api.workspace?.id !== workspaceID) throw new Error('The original workspace is no longer connected. Reopen it to apply the selected model.');
      if (!state.agent?.is_ready) await api.request(`${root}/agent/init`, 'POST', { interactive: true });
      if (currentEpoch !== epoch || api.workspace?.id !== workspaceID) throw new Error('The original workspace is no longer connected. Reopen it to apply the selected model.');
      if (sessionID) await bindSessionModels(sessionID, slot, models[slot] ?? { model, owner: provider.owner }, models);
      else if (state.agent?.is_ready) await api.request(`${root}/agent/update`, 'POST', { state: models });
    } catch (error) {
      if (currentEpoch === epoch) await Promise.all([refreshWorkspace(), refreshSessions()]).catch(fail);
      throw new Error(`Model ${scope === 'runtime' ? 'selected for this runtime' : 'saved'}, but it could not be applied: ${error instanceof Error ? error.message : error}`);
    }
    if (currentEpoch === epoch) await refreshWorkspace();
  }

  function selectedModel(slot: 'large' | 'small' = 'large') {
    return state.currentSession?.models?.[slot]?.model ?? state.workspace?.config.models?.[slot];
  }

  async function setModelControl(patch: Partial<SelectedModel>, slot: 'large' | 'small' = 'large') {
    if (state.currentSession?.is_busy) throw new Error('The agent is busy. Wait for the current turn to finish.');
    const current = selectedModel(slot);
    if (!current) throw new Error('Choose a model first.');
    const provider = state.workspace?.provider_surfaces?.find(item => item.id === current.provider);
    if (!provider) throw new Error(`Provider ${current.provider} is not available.`);
    await chooseModel(provider, { ...current, ...patch }, 0, slot);
  }

  async function runGoal(input: string) {
    const currentEpoch = epoch;
    const command = parseGoalCommand(input);
    if (!command) throw new Error('Not a goal command.');
    const workspaceID = state.workspace?.id;
    const root = api.route('/sessions');
    let session: Session | undefined;
    if (state.sessionID) session = await api.request<Session>(`${root}/${state.sessionID}`);
    else if (command.kind === 'set') {
      const id = await newSession();
      if (currentEpoch !== epoch) throw new Error('The workspace changed during the goal update.');
      session = await api.request<Session>(`${root}/${id}`);
    }
    if (currentEpoch !== epoch) throw new Error('The workspace changed during the goal update.');
    if (!session) return 'No active goal.';
    const route = `${root}/${session.id}/goal`;
    const generation = session.goal?.generation ?? 0;
    const needsGoal = command.kind !== 'set' && command.kind !== 'status';
    if (needsGoal && !session.goal?.objective) return 'No active goal.';
    let updated = session;
    switch (command.kind) {
      case 'set': updated = await api.request<Session>(route, 'PUT', { action: 'set', objective: command.objective }); break;
      case 'pause': case 'resume': case 'complete':
        updated = await api.request<Session>(route, 'PUT', { action: 'status', generation, status: command.kind === 'pause' ? 'paused' : command.kind === 'resume' ? 'active' : 'complete' }); break;
      case 'stop': updated = await api.request<Session>(route, 'PUT', { action: 'stop', generation }); break;
      case 'clear': updated = await api.request<Session>(route, 'PUT', { action: 'clear' }); break;
    }
    if (currentEpoch === epoch && state.workspace?.id === workspaceID) {
      if (state.sessionID === updated.id) set('currentSession', reconcile(updated));
      await refreshSessions();
    }
    return goalNotice(command, updated);
  }

  async function togglePlan(cancel = false) {
    if (state.modeSaving) throw new Error('Wait for the current mode change.');
    if (!state.connected) throw new Error('Wait for the workspace to reconnect before toggling plan mode.');
    await updateSession('mode', cancel || isPlanMode(state.currentSession?.mode) ? 'default' : 'plan');
  }

  async function setInteractionMode(mode: string) {
    if (!['normal', 'plan', 'yolo', 'plan-yolo'].includes(mode)) throw new Error(`Invalid interaction mode: ${mode}`);
    if (!state.connected || state.modeSaving) throw new Error('Wait for the current mode change or workspace connection.');
    if (state.currentSession?.archived) throw new Error('Restore this session before changing mode.');
    const workspaceID = api.workspace?.id;
    const root = api.route('');
    const currentEpoch = epoch;
    const plan = mode === 'plan' || mode === 'plan-yolo';
    const skip = mode === 'yolo' || mode === 'plan-yolo';
    set('modeSaving', true);
    try {
      const id = state.sessionID || await newSession();
      const requireOrigin = () => { if (currentEpoch !== epoch || workspaceID !== api.workspace?.id || id !== state.sessionID) throw new Error('The workspace or session changed during the mode update.'); };
      requireOrigin();
      if (plan !== isPlanMode(state.currentSession?.mode)) {
        await api.request(`${root}/sessions/${id}/mode`, 'PUT', { mode: plan ? 'plan' : 'default' });
        requireOrigin();
      }
      if (skip !== state.autoApprove) await api.request(`${root}/permissions/skip`, 'POST', { skip });
      requireOrigin();
    } finally {
      if (currentEpoch === epoch) {
        await Promise.all([refreshSessions(), refreshWorkspace()]).finally(() => { if (currentEpoch === epoch) set('modeSaving', false); });
      }
    }
  }

  async function suggest(id: string, signal?: AbortSignal) {
    const result = await api.request<{ suggestion: string }>(api.route(`/agent/sessions/${id}/suggest`), 'POST', undefined, signal);
    return result?.suggestion?.trim() ?? '';
  }

  async function promptHistory() {
    const route = state.sessionID ? api.route(`/sessions/${state.sessionID}/messages/user`) : api.route('/messages/user');
    const messages = await api.request<Message[]>(route);
    const texts: string[] = [];
    for (const message of messages ?? []) {
      if (taskNotification(message)) continue;
      const text = message.parts.filter(part => part.type === 'text').map(part => part.data.text ?? '').join('\n');
      if (text) texts.push(text);
      for (const part of message.parts) if (part.type === 'shell_command' && part.data.command) texts.push(`!${part.data.command}`);
    }
    return texts;
  }

  async function summarize() {
    const currentEpoch = epoch;
    const id = state.sessionID;
    const workspaceID = api.workspace?.id;
    const currentSelection = selection;
    if (!id) throw new Error('Open a session to summarize.');
    await api.request(api.route(`/agent/sessions/${id}/summarize`), 'POST');
    if (currentEpoch !== epoch || workspaceID !== api.workspace?.id) return;
    await Promise.all([refreshSessions(), ...(currentSelection === selection ? [loadMessages(id)] : [])]);
  }

  async function cancelRun() {
    const currentEpoch = epoch;
    const id = state.sessionID;
    if (!id) return;
    await api.request(api.route(`/agent/sessions/${id}/cancel`), 'POST');
    if (currentEpoch !== epoch) return;
    await Promise.all([refreshSessions(), refreshQueue()]);
  }

  async function deleteSession(id = state.sessionID) {
    const workspaceID = api.workspace?.id;
    const currentEpoch = epoch;
    const previousSelection = selection;
    await api.request(api.route(`/sessions/${id}/delete`), 'POST');
    if (currentEpoch !== epoch || workspaceID !== api.workspace?.id) return;
    if (id === state.sessionID && previousSelection === selection) await selectSession('');
    if (currentEpoch === epoch && workspaceID === api.workspace?.id) await refreshSessions();
  }

  async function initializeProject() {
    if (state.currentSession?.is_busy) throw new Error('The agent is busy. Wait before initializing the project.');
    const workspaceID = api.workspace?.id;
    const root = api.route('/project');
    const currentEpoch = epoch;
    const requireWorkspace = () => { if (currentEpoch !== epoch || workspaceID !== api.workspace?.id) throw new Error('The workspace changed during project initialization. Reopen the original workspace to initialize it.'); };
    const { prompt } = await api.request<{ prompt: string }>(`${root}/init-prompt`);
    requireWorkspace();
    await api.request(`${root}/init`, 'POST');
    requireWorkspace();
    await selectSession('');
    requireWorkspace();
    await send(prompt);
  }

  async function switchWorkspace(path: string, dataDir?: string, connection = api.connection) {
    if (connecting || switching) throw new Error('Wait for the current workspace connection to finish.');
    if (!path.startsWith('/') && !/^[A-Za-z]:[\\/]/.test(path)) throw new Error('Enter an absolute workspace directory.');
    switching = true;
    clearTimeout(reconnectTimer);
    reconnectTimer = undefined;
    epoch++;
    workspaceRead++;
    sessionRead++;
    pendingRead++;
    messageRead++;
    olderRead++;
    queueRead++;
    taskRead++;
    selection++;
    set('deliverySaving', false);
    set('modeSaving', false);
    set('sending', false);
    set('connected', false);
    set('providerUsage', reconcile({}));
    try {
      try { await api.release(); }
      catch (error) { scheduleReconnect(); throw error; }
      closeNotifications();
      batch(() => { set('workspace', undefined); set('sessionID', ''); set('currentSession', undefined); set('queued', []); set('messages', {}); set('messageIDs', []); set('sessions', []); set('tasks', []); set('tasksError', ''); set('permission', null); set('question', null); });
      await connect(false, path, dataDir, connection);
    } finally { switching = false; }
  }

  function dispose() {
    stopped = true;
    notificationGeneration++;
    closeNotifications();
    epoch++;
    clearTimeout(reconnectTimer);
    cancelAnimationFrame(frame);
    api.disconnect();
  }

  return {
    state, set, api, fail, attempt, toast, dismissToast, clearErrors, setNotifications, testNotification, connect, dispose, selectSession, newSession, send, chooseModel, selectedModel, setModelControl,
    refreshWorkspace, refreshSessions, refreshTasks, refreshPending, switchWorkspace, loadMessages, loadOlderMessages, setDeliveryMode, refreshQueue, clearQueue,
    updateSession, forkSession, archiveSession, rewind, rollback, runGoal, togglePlan, setInteractionMode, suggest, promptHistory, summarize, cancelRun, deleteSession, initializeProject,
  };
}

export type CruxState = ReturnType<typeof createCruxState>;
