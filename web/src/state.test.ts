import assert from 'node:assert/strict';
import test from 'node:test';
import { createCruxState } from './state.ts';
import type { Event, Message, Workspace } from './types.ts';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

class MemoryStorage {
  private values = new Map<string, string>();
  get length() { return this.values.size; }
  key(index: number) { return [...this.values.keys()][index] ?? null; }
  getItem(name: string) { return this.values.get(name) ?? null; }
  setItem(name: string, value: string) { this.values.set(name, String(value)); }
  removeItem(name: string) { this.values.delete(name); }
  clear() { this.values.clear(); }
}

function setup(t: test.TestContext, extraGlobals: Record<string, unknown> = {}) {
  t.after(() => client.dispose());
  const frames = new Map<number, FrameRequestCallback>();
  let frameID = 0;
  const workspace: Workspace = { id: 'workspace', path: '/project', data_dir: '/custom', requested_data_dir: '/custom', authority: { mode: 'server', revision: 3, digest: 'native' }, config: { models: {}, options: {} } };
  const session = { id: 'session', title: 'Test', updated_at: 1, is_busy: false };
  const requests: { path: string; method: string; body: any; headers: Record<string, string> }[] = [];
  const overrides = new Map<string, (body: any) => unknown>();
  class Socket {
    static current: Socket;
    onopen?: () => void;
    onmessage?: (event: { data: string }) => void;
    onclose?: () => void;
    constructor() { Socket.current = this; queueMicrotask(() => this.onopen?.()); }
    close() { this.onclose?.(); }
  }
  const globals: Record<string, unknown> = {
    localStorage: new MemoryStorage(),
    location: new URL('http://127.0.0.1:8080'),
    history: { replaceState() {} },
    requestAnimationFrame: (callback: FrameRequestCallback) => { frames.set(++frameID, callback); return frameID; },
    cancelAnimationFrame: (id: number) => frames.delete(id),
    WebSocket: Socket,
    fetch: async (url: URL, init: RequestInit) => {
      const path = url.pathname;
      const body = init.body ? JSON.parse(String(init.body)) : undefined;
      requests.push({ path, method: init.method!, body, headers: init.headers as Record<string, string> });
      const custom = overrides.get(path);
      let value: unknown;
      if (custom) value = await custom(body);
      else if (path === '/ui/bootstrap') value = { path: '/project', data_dir: '/custom', version: 'test' };
      else if (path === '/v1/workspaces' || path === '/v1/workspaces/workspace') value = workspace;
      else if (path.endsWith('/agent')) value = { is_ready: true };
      else if (path.endsWith('/sessions')) value = [session];
      else if (path.endsWith('/messages') || path.endsWith('/prompts/list') || path.endsWith('/tasks')) value = [];
      else if (path.endsWith('/pending')) value = { permission: null, question: null };
      else value = {};
      return value instanceof Response ? value : Response.json(value);
    },
  };
  Object.assign(globals, extraGlobals);
  for (const [name, value] of Object.entries(globals)) {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, name);
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    t.after(() => { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else Reflect.deleteProperty(globalThis, name); });
  }
  const client = createCruxState();
  const flush = () => { const pending = [...frames.values()]; frames.clear(); pending.forEach(callback => callback(0)); };
  const event = (value: Event) => Socket.current.onmessage?.({ data: JSON.stringify({ type: 'event', event: value }) });
  return { client, workspace, session, requests, overrides, flush, event };
}

function notificationSetup(t: test.TestContext) {
  const delivered: DesktopNotification[] = [];
  let requests = 0;
  let focused = 0;
  const document = { visibilityState: 'hidden', hasFocus: () => true };
  class DesktopNotification {
    static permission = 'default';
    static async requestPermission() { requests++; return DesktopNotification.permission = 'granted'; }
    closed = false;
    onclick?: () => void;
    onclose?: () => void;
    onerror?: () => void;
    constructor(readonly title: string, readonly options: { body: string; tag?: string }) { delivered.push(this); }
    get tag() { return this.options.tag ?? ''; }
    close() { this.closed = true; this.onclose?.(); }
  }
  const fixture = setup(t, { Notification: DesktopNotification, document, window: { focus() { focused++; } } });
  return { ...fixture, delivered, DesktopNotification, document, permissionRequests: () => requests, focusCount: () => focused };
}

test('browser alerts require explicit permission and follow native event identities', async t => {
  const { client, delivered, event, flush, permissionRequests } = notificationSetup(t);
  await client.connect(true);
  assert.equal(permissionRequests(), 0);
  const complete = { type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'run', message_id: 'reply' } } };
  event(complete); flush();
  assert.equal(delivered.length, 0);
  await client.setNotifications(true);
  assert.equal(permissionRequests(), 1);
  event(complete);
  assert.equal(delivered.length, 1);
  flush(); event(complete); flush();
  assert.equal(delivered.length, 1);
  assert.equal(delivered[0].title, 'Crux turn complete');
  assert.equal(delivered[0].options.body, 'Test');
  event({ type: 'permission_request', payload: { type: 'created', payload: { id: 'permission', tool_call_id: 'call', tool_name: 'bash', session_id: 'session' } } }); flush();
  event({ type: 'question_batch_request', payload: { type: 'created', payload: { id: 'question', session_id: 'session', questions: [{ question: 'Which option?' }] } } }); flush();
  event({ type: 'task_notification', payload: { type: 'created', payload: { notification_id: 'notice', task_id: 'task', status: 'failed', summary: 'Checks failed', parent_session_id: 'session' } } }); flush();
  assert.deepEqual(delivered.slice(1).map(item => item.title), ['Crux needs permission', 'Crux needs your input', 'Crux background task finished']);
  assert.equal(delivered[3].options.body, 'Checks failed');
  event({ type: 'permission_notification', payload: { type: 'updated', payload: { tool_call_id: 'call', granted: true } } });
  event({ type: 'question_batch_notification', payload: { type: 'updated', payload: { batch_id: 'question' } } });
  assert.equal(delivered[1].closed, true);
  assert.equal(delivered[2].closed, true);
  await client.setNotifications(false);
  assert.ok(delivered.every(item => item.closed));
  event({ ...complete, payload: { ...complete.payload, payload: { ...complete.payload.payload, run_id: 'later' } } }); flush();
  assert.equal(delivered.length, 4);
});

test('browser alerts suppress focused events but an explicit preview is delivered', async t => {
  const { client, delivered, document, event, flush } = notificationSetup(t);
  await client.connect(true); await client.setNotifications(true);
  document.visibilityState = 'visible';
  const complete = { type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'run', error: 'Native failure' } } };
  event(complete); flush();
  assert.equal(delivered.length, 0);
  document.visibilityState = 'hidden';
  event(complete); flush();
  assert.equal(delivered.length, 0);
  client.testNotification();
  assert.equal(delivered[0].title, 'Crux notifications');
  event({ ...complete, payload: { ...complete.payload, payload: { ...complete.payload.payload, run_id: 'next' } } }); flush();
  assert.equal(delivered[1].title, 'Crux turn failed');
  assert.equal(delivered[1].options.body, 'Native failure');
  event({ type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'stopped', cancelled: true } } }); flush();
  assert.equal(delivered[2].title, 'Crux turn stopped');
});

test('restored pending decisions notify once and clicks stay bound to their workspace', async t => {
  const { client, delivered, overrides, requests, focusCount, workspace } = notificationSetup(t);
  await client.connect(true); await client.setNotifications(true);
  overrides.set('/ui/workspaces/workspace/pending', () => ({ permission: { id: 'p', tool_call_id: 'call', tool_name: 'view', session_id: 'session' }, question: null }));
  await client.refreshPending(); await client.refreshPending();
  assert.equal(delivered.length, 1);
  delivered[0].onclick?.();
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(focusCount(), 1);
  assert.equal(client.state.sessionID, 'session');
  assert.ok(requests.some(item => item.path.endsWith('/current-session') && item.body.session_id === 'session'));
  client.testNotification();
  const latest = delivered.at(-1)!;
  client.set('workspace', { ...workspace, id: 'other', path: '/other' });
  client.api.workspace = client.state.workspace;
  const count = requests.length;
  latest.onclick?.();
  assert.equal(requests.length, count);
  assert.match(client.state.toasts.at(-1)!.text, /belongs to \/project/);
});

test('notification denial, revocation, delivery failure, and late permission results remain visible', async t => {
  const { client, DesktopNotification, delivered } = notificationSetup(t);
  DesktopNotification.requestPermission = async () => 'denied';
  await client.setNotifications(true);
  assert.equal(client.state.notificationsEnabled, false);
  assert.match(client.state.notificationError, /blocked/);
  DesktopNotification.permission = 'granted';
  await client.setNotifications(true);
  client.testNotification();
  delivered[0].onerror?.();
  assert.match(client.state.notificationError, /could not display/);
  DesktopNotification.permission = 'denied';
  client.testNotification();
  assert.equal(client.state.notificationsEnabled, false);
  assert.match(client.state.notificationError, /no longer available/);
  DesktopNotification.permission = 'default';
  const pending = deferred<string>();
  DesktopNotification.requestPermission = () => pending.promise;
  const enabling = client.setNotifications(true);
  await client.setNotifications(false);
  pending.resolve('granted'); await enabling;
  assert.equal(client.state.notificationsEnabled, false);
  assert.equal(client.state.notificationsBusy, false);
});

test('failed alert delivery can retry and completions without IDs are not suppressed', async t => {
  const { client, delivered, event } = notificationSetup(t);
  await client.connect(true); await client.setNotifications(true);
  const complete = { type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'run' } } };
  event(complete);
  delivered[0].onerror?.();
  assert.equal(delivered[0].closed, true);
  assert.match(client.state.notificationError, /could not display/);
  event(complete);
  assert.equal(delivered.length, 2);
  assert.equal(client.state.notificationError, '');
  const withoutID = { type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session' } } };
  event(withoutID); event(withoutID);
  assert.equal(delivered.length, 4);
});

test('unsupported notification enable clears an earlier pending request', async t => {
  const { client, DesktopNotification } = notificationSetup(t);
  const pending = deferred<string>();
  DesktopNotification.requestPermission = () => pending.promise;
  const enabling = client.setNotifications(true);
  assert.equal(client.state.notificationsBusy, true);
  Object.defineProperty(globalThis, 'Notification', { configurable: true, writable: true, value: undefined });
  await client.setNotifications(true);
  pending.resolve('granted'); await enabling;
  assert.equal(client.state.notificationsBusy, false);
  assert.equal(client.state.notificationsEnabled, false);
  assert.equal(client.state.notificationPermission, 'unsupported');
});

test('event bursts in one frame share one refresh per native resource', async t => {
  const { client, event, flush, requests } = setup(t);
  await client.connect(true);
  const count = (suffix: string) => requests.filter(item => item.method === 'GET' && item.path.endsWith(suffix)).length;
  const before = { sessions: count('/sessions'), messages: count('/sessions/session/messages'), tasks: count('/tasks'), pending: count('/pending') };
  for (let index = 0; index < 5; index++) event({ type: 'message', payload: { type: 'created', payload: { id: `m${index}`, session_id: 'session', role: 'assistant', parts: [] } } });
  event({ type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'run' } } });
  event({ type: 'task_notification', payload: { type: 'created', payload: { task_id: 'task', parent_session_id: 'session', status: 'completed' } } });
  event({ type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'other' } } });
  flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(count('/sessions') - before.sessions, 1);
  assert.equal(count('/sessions/session/messages') - before.messages, 1);
  assert.equal(count('/tasks') - before.tasks, 1);
  assert.equal(count('/pending') - before.pending, 1);
  event({ type: 'message', payload: { type: 'created', payload: { id: 'later', session_id: 'session', role: 'user', parts: [] } } });
  flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(count('/sessions') - before.sessions, 2);
});

test('scheduled event refreshes do not run after disposal', async t => {
  const { client, event, flush, requests } = setup(t);
  await client.connect(true);
  const before = requests.length;
  event({ type: 'run_complete', payload: { type: 'updated', payload: { session_id: 'session', run_id: 'run' } } });
  flush(); client.dispose();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(requests.length, before);
});

test('message deltas rebuild native snapshots and resync on a missed revision', async t => {
  const { client, event, flush, requests, overrides } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  const full = { id: 'reply', session_id: 'session', role: 'assistant', parts: [{ type: 'text', data: { text: 'Hel' } }], created_at: 1, updated_at: 1, model: 'm', provider: 'p', delta_revision: 1 };
  event({ type: 'message', payload: { type: 'created', payload: full } }); flush();
  assert.equal('delta_revision' in client.state.messages.reply, false);
  event({ type: 'message_delta', payload: { type: 'updated', payload: { id: 'reply', session_id: 'session', updated_at: 2, base: 1, revision: 2, length: 2, parts: [{ index: 0, field: 'text', append: 'lo' }, { index: 1, part: { type: 'tool_call', data: { id: 'c', name: 'bash' } } }] } } });
  event({ type: 'message_delta', payload: { type: 'updated', payload: { id: 'reply', session_id: 'session', updated_at: 3, base: 2, revision: 3, length: 2, parts: [{ index: 0, field: 'text', append: '!' }] } } });
  flush();
  assert.equal(client.state.messages.reply.parts[0].data.text, 'Hello!');
  assert.equal(client.state.messages.reply.parts[1].type, 'tool_call');
  assert.equal(client.state.messages.reply.updated_at, 3);
  const reloads = () => requests.filter(item => item.path.endsWith('/sessions/session/messages')).length;
  const before = reloads();
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => [{ ...full, delta_revision: undefined, parts: [{ type: 'text', data: { text: 'Hello! world' } }] }]);
  event({ type: 'message_delta', payload: { type: 'updated', payload: { id: 'reply', session_id: 'session', updated_at: 5, base: 4, revision: 5, length: 1, parts: [{ index: 0, field: 'text', append: ' world' }] } } });
  flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(reloads(), before + 1);
  assert.equal(client.state.messages.reply.parts[0].data.text, 'Hello! world');
  event({ type: 'message_delta', payload: { type: 'updated', payload: { id: 'reply', session_id: 'session', updated_at: 6, base: 5, revision: 6, length: 1, parts: [{ index: 0, field: 'text', append: '?' }] } } });
  flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(reloads(), before + 2, 'an untracked message after resync is reloaded rather than guessed');
});

test('native busy events update the session list and current session without polling', async t => {
  const { client, event, flush, requests } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  const lists = () => requests.filter(item => item.path.endsWith('/prompts/list')).length;
  const before = lists();
  event({ type: 'session_busy', payload: { type: 'updated', payload: { session_id: 'session', busy: true } } }); flush();
  assert.equal(client.state.sessions[0].is_busy, true);
  assert.equal(client.state.currentSession?.is_busy, true);
  event({ type: 'session_busy', payload: { type: 'updated', payload: { session_id: 'session', busy: false } } }); flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(client.state.sessions[0].is_busy, false);
  assert.equal(client.state.currentSession?.is_busy, false);
  assert.equal(lists(), before + 1);
});

test('the workspace channel opts in to compact native features', async t => {
  let url: URL | undefined;
  class Socket { constructor(value: URL) { url = value; queueMicrotask(() => (this as any).onopen?.()); } close() {} }
  const { client } = setup(t, { WebSocket: Socket });
  await client.connect(true);
  assert.equal(url?.searchParams.get('features'), 'message_delta,session_busy');
});

test('long conversations load the newest page, prepend older pages, and keep them across reloads', async t => {
  const { client, overrides } = setup(t);
  const message = (id: string) => ({ id, session_id: 'session', role: 'user', parts: [{ type: 'text', data: { text: id } }], created_at: 1, updated_at: 1, model: '', provider: '' });
  const page = (ids: string[], more?: boolean) => new Response(JSON.stringify(ids.map(message)), { headers: more === undefined ? {} : { 'Crux-Messages-More': String(more) } });
  const urls: string[] = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (url: URL, init: RequestInit) => { if (url.pathname.endsWith('/sessions/session/messages')) urls.push(url.search.replace(/[?&]client_id=[^&]*/, '')); return originalFetch(url, init); }) as typeof fetch;
  const responses = [page(['c', 'd'], true), page(['a', 'b'], false), page(['c', 'd', 'e'], true)];
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => responses.shift());
  await client.connect(true);
  await client.selectSession('session');
  assert.deepEqual(client.state.messageIDs, ['c', 'd']);
  assert.equal(client.state.hasOlderMessages, true);
  assert.equal(await client.loadOlderMessages(), 2);
  assert.deepEqual(client.state.messageIDs, ['a', 'b', 'c', 'd']);
  assert.equal(client.state.hasOlderMessages, false);
  await client.loadMessages('session');
  assert.deepEqual(client.state.messageIDs, ['a', 'b', 'c', 'd', 'e']);
  assert.equal(client.state.hasOlderMessages, false);
  assert.match(urls[0], /limit=120/);
  assert.match(urls[1], /limit=120&before=c/);
});

test('searching full history loads every older message once', async t => {
  const { client, overrides } = setup(t);
  const message = (id: string) => ({ id, session_id: 'session', role: 'user', parts: [], created_at: 1, updated_at: 1, model: '', provider: '' });
  const responses = [new Response(JSON.stringify(['c'].map(message)), { headers: { 'Crux-Messages-More': 'true' } }), Response.json(['a', 'b', 'c'].map(message))];
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => responses.shift());
  await client.connect(true);
  await client.selectSession('session');
  assert.equal(await client.loadOlderMessages(true), 2);
  assert.deepEqual(client.state.messageIDs, ['a', 'b', 'c']);
  assert.equal(client.state.hasOlderMessages, false);
  assert.equal(await client.loadOlderMessages(true), 0);
});

test('recent errors and enabled notifications survive a client restart in the same browser', async t => {
  const storage = new MemoryStorage();
  const { client, DesktopNotification } = notificationSetup(t);
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, writable: true, value: storage });
  t.mock.timers.enable({ apis: ['setTimeout'] });
  client.fail('Provider failed\nfull details');
  DesktopNotification.permission = 'granted';
  await client.setNotifications(true);
  t.mock.timers.tick(1000);
  client.dispose();
  const restored = createCruxState();
  t.after(() => restored.dispose());
  assert.deepEqual(restored.state.recentErrors.map(item => item.text), ['Provider failed\nfull details']);
  assert.equal(restored.state.notificationsEnabled, true);
  restored.fail('Later failure');
  assert.notEqual(restored.state.recentErrors[1].id, restored.state.recentErrors[0].id);
  restored.clearErrors();
  t.mock.timers.tick(1000);
  DesktopNotification.permission = 'denied';
  const revoked = createCruxState();
  t.after(() => revoked.dispose());
  assert.deepEqual(revoked.state.recentErrors, []);
  assert.equal(revoked.state.notificationsEnabled, false);
  assert.match(revoked.state.notificationError, /Enable them again/);
});

test('pending session selection cannot read history from a different workspace', async t => {
  const { client, workspace, overrides, requests } = setup(t);
  await client.connect(true);
  const pending = deferred<object>();
  overrides.set('/v1/workspaces/workspace/current-session', () => pending.promise);
  const selected = client.selectSession('session');
  client.api.workspace = { ...workspace, id: 'other', path: '/other' };
  client.set('workspace', client.api.workspace);
  client.set('sessionID', '');
  pending.resolve({}); await selected;
  assert.equal(requests.some(item => item.path.startsWith('/v1/workspaces/other/')), false);
  assert.equal(client.state.sessionID, '');
});

test('notification permission completion cannot revive a disposed client', async t => {
  const { client, DesktopNotification, delivered } = notificationSetup(t);
  const pending = deferred<string>();
  DesktopNotification.requestPermission = () => pending.promise;
  const enabling = client.setNotifications(true);
  client.dispose(); pending.resolve('granted'); await enabling;
  client.testNotification();
  assert.equal(client.state.notificationsEnabled, false);
  assert.equal(delivered.length, 0);
});

test('recent errors retain banners and dismissed toasts without recording successful notifications', async t => {
  const { client } = setup(t);
  await client.connect(true);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  client.fail(new Error('Full banner\nProvider details'));
  client.toast('Full toast\nTransport details', 'error');
  client.toast('Saved', 'success');
  client.toast('Loading', 'info');
  client.dismissToast(client.state.toasts[0].id);
  client.set('error', '');
  t.mock.timers.tick(10000);
  assert.deepEqual(client.state.toasts, []);
  assert.deepEqual(client.state.recentErrors.map(item => item.text), ['Full banner\nProvider details', 'Full toast\nTransport details']);
  assert.equal(client.state.recentErrors[0].workspace, '/project');
  client.clearErrors();
  assert.deepEqual(client.state.recentErrors, []);
});

test('recent error history retains only the latest 50 full records', t => {
  const { client } = setup(t);
  for (let index = 0; index < 55; index++) client.fail(`Failure ${index}`);
  assert.equal(client.state.recentErrors.length, 50);
  assert.equal(client.state.recentErrors[0].text, 'Failure 5');
  assert.equal(client.state.recentErrors.at(-1)?.text, 'Failure 54');
});

test('native finish errors retain details and update in place across event replay and history loading', async t => {
  const { client, event, flush, overrides } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  let message: Message = { id: 'failure', session_id: 'session', role: 'assistant', model: '', provider: '', parts: [{ type: 'finish', data: { reason: 'error', message: 'Request failed', details: 'status code 500: detail', time: 123 } }], created_at: 1, updated_at: 1 };
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => [message]);
  await client.loadMessages('session');
  assert.equal(client.state.recentErrors[0].text, 'Request failed\nstatus code 500: detail');
  assert.equal(client.state.recentErrors[0].time, 123000);
  assert.equal(client.state.recentErrors[0].sessionID, 'session');
  const id = client.state.recentErrors[0].id;
  event({ type: 'message', payload: { type: 'updated', payload: message } }); flush();
  await client.loadMessages('session');
  assert.equal(client.state.recentErrors.length, 1);
  message = { ...message, parts: [{ type: 'finish', data: { reason: 'content_filter', details: 'Provider refusal detail' } }] };
  event({ type: 'message', payload: { type: 'updated', payload: message } }); flush();
  assert.equal(client.state.recentErrors.length, 1);
  assert.equal(client.state.recentErrors[0].id, id);
  assert.equal(client.state.recentErrors[0].text, 'Provider refusal detail');
  for (const reason of ['end_turn', 'canceled', 'max_tokens']) {
    event({ type: 'message', payload: { type: 'updated', payload: { ...message, id: reason, parts: [{ type: 'finish', data: { reason } }] } } }); flush();
  }
  assert.equal(client.state.recentErrors.length, 1);
  event({ type: 'message', payload: { type: 'updated', payload: { ...message, session_id: 'other-session', parts: [{ type: 'finish', data: { reason: 'error' } }] } } }); flush();
  assert.equal(client.state.recentErrors.length, 2);
  assert.equal(client.state.recentErrors[1].text, 'error');
  assert.equal(client.state.messages.failure.session_id, 'session');
});

for (const archived of [false, true]) test(`session events preserve top-level listing and selected child updates with archived=${archived}`, async t => {
  const { client, session, event, flush } = setup(t);
  await client.connect(true);
  const child = { ...session, id: 'child', parent_session_id: 'session', archived };
  client.set('archived', archived);
  client.set('sessions', []);
  client.set('sessionID', child.id);
  client.set('currentSession', child);
  event({ type: 'session', payload: { type: 'created', payload: child } });
  flush();
  assert.deepEqual(client.state.sessions, []);
  event({ type: 'session', payload: { type: 'updated', payload: { ...child, title: 'Updated child' } } });
  flush();
  assert.equal(client.state.currentSession?.title, 'Updated child');
  assert.deepEqual(client.state.sessions, []);
  event({ type: 'session', payload: { type: 'created', payload: { ...session, archived } } });
  flush();
  assert.deepEqual(client.state.sessions.map(item => item.id), ['session']);
  event({ type: 'session', payload: { type: 'updated', payload: { ...session, archived: !archived } } });
  flush();
  assert.deepEqual(client.state.sessions, []);
});

test('failed workspace release reconnects the original workspace instead of leaving a false connected state', async t => {
  const { client, requests, overrides } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  overrides.set('/v1/workspaces/workspace', () => Response.json({ message: 'Release failed' }, { status: 503 }));
  await assert.rejects(client.switchWorkspace('/other-project'), /Release failed/);
  assert.equal(client.state.connected, false);
  assert.match(client.state.notice, /Reconnecting/);
  assert.equal(client.state.workspace?.path, '/project');
  assert.equal(client.state.sessionID, 'session');
  assert.equal(requests.some(request => request.path === '/v1/workspaces' && request.body?.path === '/other-project'), false);
  overrides.delete('/v1/workspaces/workspace');
  await client.connect(false);
  assert.equal(client.state.connected, true);
  assert.equal(client.state.sessionID, 'session');
  const attachments = requests.filter(request => request.path === '/v1/workspaces' && request.method === 'POST');
  assert.equal(attachments.at(-1)?.body.path, '/project');
});

for (const kind of ['child', 'archived']) test(`startup restores an unlisted ${kind} session from its URL`, async t => {
  const { client, session, requests, overrides } = setup(t);
  location.hash = `session=${kind}`;
  overrides.set(`/v1/workspaces/workspace/sessions/${kind}`, () => ({ ...session, id: kind, ...(kind === 'child' ? { parent_session_id: 'session' } : { archived: true }) }));
  await client.connect(true);
  assert.equal(client.state.sessionID, kind);
  assert.equal(client.state.currentSession?.id, kind);
  assert.equal(client.state.connected, true);
  assert.ok(requests.some(request => request.path.endsWith('/current-session') && request.body.session_id === kind));
  assert.ok(requests.some(request => request.path === `/v1/workspaces/workspace/sessions/${kind}/messages`));
});

for (const ambiguous of [false, true]) test(`startup preserves explicit session errors with ambiguous=${ambiguous}`, async t => {
  const { client, session, requests, overrides } = setup(t);
  location.hash = 'session=missing';
  if (ambiguous) overrides.set('/v1/workspaces/workspace/sessions', () => [{ ...session, id: 'missing-one' }, { ...session, id: 'missing-two' }]);
  else overrides.set('/v1/workspaces/workspace/sessions/missing', () => Response.json({ message: 'Session not found' }, { status: 404 }));
  await assert.rejects(client.connect(true), ambiguous ? /ambiguous/ : /Session not found/);
  assert.equal(client.state.connected, false);
  assert.equal(requests.some(request => request.path.endsWith('/current-session')), false);
  if (ambiguous) assert.equal(requests.some(request => request.path.endsWith('/sessions/missing')), false);
});

for (const navigate of [false, true]) test(`summarization refresh remains workspace-owned with navigation=${navigate}`, async t => {
  const { client, workspace, requests, overrides } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  const pending = deferred<unknown>();
  overrides.set('/v1/workspaces/workspace/agent/sessions/session/summarize', () => pending.promise);
  const operation = client.summarize();
  if (navigate) {
    const next = { ...workspace, id: 'other-workspace' };
    client.api.workspace = next;
    client.set('workspace', next);
  }
  const before = requests.length;
  pending.resolve({});
  await operation;
  const refreshes = requests.slice(before);
  if (navigate) assert.deepEqual(refreshes, []);
  else {
    assert.ok(refreshes.some(item => item.path === '/v1/workspaces/workspace/sessions'));
    assert.ok(refreshes.some(item => item.path === '/v1/workspaces/workspace/sessions/session/messages'));
  }
});

for (const navigate of [false, true]) test(`rollback uses its native contract with navigation=${navigate}`, async t => {
  const { client, workspace, requests, overrides } = setup(t);
  await client.connect(true);
  await client.selectSession('session');
  const pending = deferred<unknown>();
  overrides.set('/v1/workspaces/workspace/sessions/session/rollback', () => pending.promise);
  const operation = client.rollback('boundary');
  assert.deepEqual(requests.at(-1)?.body, { message_id: 'boundary' });
  if (navigate) {
    const next = { ...workspace, id: 'other-workspace' };
    client.api.workspace = next;
    client.set('workspace', next);
  }
  const before = requests.length;
  pending.resolve({});
  await operation;
  if (navigate) assert.deepEqual(requests.slice(before), []);
  else assert.ok(requests.slice(before).some(item => item.path.endsWith('/sessions/session/messages')));
  assert.equal(requests.some(item => item.path.endsWith('/rewind')), false);
});

const message = (text: string): Message => ({ id: 'message', session_id: 'session', role: 'assistant', parts: [{ type: 'text', data: { text } }], created_at: 1, updated_at: 1, model: 'model', provider: 'provider' });

for (const scope of [0, 1, 'runtime'] as const) for (const slot of ['large', 'small'] as const) test(`${slot} model selection honors ${scope} scope and retains session ownership`, async t => {
  const { client, session, overrides, requests } = setup(t);
  const owner = { provider_id: 'provider' };
  const provider = { id: 'provider', name: 'Provider', available: true, owner };
  const model = { provider: 'provider', model: 'chosen', max_tokens: 8192 };
  const models = { [slot]: { model, owner } };
  overrides.set('/v1/workspaces/workspace/sessions/session', body => body?.model_selection ? { ...session, models: body.model_selection.models } : session);
  overrides.set('/v1/workspaces/workspace/config/model', () => models);
  overrides.set('/v1/workspaces/workspace/config/model-overrides', () => models);
  await client.connect(true);
  await client.selectSession('session');
  await client.chooseModel(provider, model, scope, slot);
  const mutation = requests.find(item => item.path.endsWith(scope === 'runtime' ? '/config/model-overrides' : '/config/model'));
  assert.deepEqual(mutation?.body, scope === 'runtime' ? { state: models } : { scope, model_type: slot, model, owner });
  assert.equal(requests.some(item => item.path.endsWith(scope === 'runtime' ? '/config/model' : '/config/model-overrides')), false);
  const binding = requests.find(item => item.path.endsWith('/sessions/session') && item.method === 'PUT');
  assert.deepEqual(binding?.body.model_selection.models[slot], { model, owner });
});

test('rejected runtime model selection never falls back to a persisted scope', async t => {
  const { client, overrides, requests } = setup(t);
  overrides.set('/v1/workspaces/workspace/config/model-overrides', () => Response.json({ message: 'Client-owned runtime cannot be overridden' }, { status: 400 }));
  await client.connect(true);
  await assert.rejects(client.chooseModel({ id: 'provider', name: 'Provider', available: true, owner: { provider_id: 'provider' } }, { provider: 'provider', model: 'chosen' }, 'runtime'), /Client-owned runtime/);
  assert.equal(requests.some(item => item.path.endsWith('/config/model')), false);
});

test('goal mutation retains its originating workspace after a delayed session read', async t => {
  const { client, workspace, session, overrides, requests } = setup(t);
  await client.connect();
  client.set('sessionID', session.id);
  const pending = deferred<typeof session>();
  overrides.set('/v1/workspaces/workspace/sessions/session', () => pending.promise);
  overrides.set('/v1/workspaces/workspace/sessions/session/goal', () => ({ ...session, goal: { objective: 'Original goal', status: 'active', generation: 1 } }));
  const operation = client.runGoal('/goal set Original goal');
  client.api.workspace = { ...workspace, id: 'other' };
  client.set('workspace', { ...workspace, id: 'other' });
  pending.resolve(session);
  await operation;
  assert.ok(requests.some(request => request.path === '/v1/workspaces/workspace/sessions/session/goal' && request.method === 'PUT'));
  assert.equal(requests.some(request => request.path.startsWith('/v1/workspaces/other/')), false);
});

for (const operation of ['fork', 'archive', 'delete'] as const) test(`${operation} completion preserves a newer session selection`, async t => {
  const { client, session, overrides, requests } = setup(t);
  const pending = deferred<object>();
  overrides.set('/v1/workspaces/workspace/sessions/session', () => session);
  overrides.set('/v1/workspaces/workspace/sessions/other', () => ({ ...session, id: 'other' }));
  overrides.set(`/v1/workspaces/workspace/sessions/session/${operation}`, () => pending.promise);
  await client.connect(true);
  await client.selectSession('session');
  const changing = operation === 'fork' ? client.forkSession() : operation === 'archive' ? client.archiveSession() : client.deleteSession();
  await client.selectSession('other');
  const count = requests.length;
  pending.resolve(operation === 'fork' ? { ...session, id: 'forked' } : {});
  await changing;
  assert.equal(client.state.sessionID, 'other');
  assert.equal(requests.slice(count).some(item => item.path.endsWith('/current-session')), false);
});

test('sidebar archive and restore target the requested session without changing selection', async t => {
  const { client, session, overrides, requests } = setup(t);
  overrides.set('/v1/workspaces/workspace/sessions/session', () => session);
  await client.connect(true);
  await client.selectSession('session');
  const count = requests.length;
  await client.archiveSession('other', false);
  await client.archiveSession('other', true);
  assert.equal(client.state.sessionID, 'session');
  assert.ok(requests.some(item => item.path.endsWith('/sessions/other/archive') && item.method === 'POST'));
  assert.ok(requests.some(item => item.path.endsWith('/sessions/other/unarchive') && item.method === 'POST'));
  assert.equal(requests.slice(count).some(item => item.path.endsWith('/current-session')), false);
});

test('sidebar branching targets the requested session without selecting its source', async t => {
  const { client, session, overrides, requests } = setup(t);
  overrides.set('/v1/workspaces/workspace/sessions/session', () => session);
  overrides.set('/v1/workspaces/workspace/sessions/other/fork', () => ({ ...session, id: 'forked' }));
  overrides.set('/v1/workspaces/workspace/sessions/forked', () => ({ ...session, id: 'forked' }));
  await client.connect(true);
  await client.selectSession('session');
  const count = requests.length;
  await client.forkSession('other');
  assert.equal(client.state.sessionID, 'forked');
  assert.ok(requests.some(item => item.path.endsWith('/sessions/other/fork') && item.method === 'POST'));
  assert.equal(requests.slice(count).some(item => item.path.endsWith('/sessions/other')), false);
});

test('project initialization cannot submit its prompt to a different workspace', async t => {
  const { client, workspace, overrides, requests } = setup(t);
  const prompt = deferred<{ prompt: string }>();
  overrides.set('/v1/workspaces/workspace/project/init-prompt', () => prompt.promise);
  await client.connect(true);
  const initializing = client.initializeProject();
  client.api.workspace = { ...workspace, id: 'other' };
  client.set('workspace', client.api.workspace);
  prompt.resolve({ prompt: 'Initialize the original project.' });
  await assert.rejects(initializing, /workspace changed/);
  assert.equal(requests.some(item => item.path.endsWith('/project/init') || (item.path.endsWith('/agent') && item.method === 'POST')), false);
});

test('rewind preserves independent summary and file restoration options', async t => {
  const { client, session, overrides, requests } = setup(t);
  overrides.set('/v1/workspaces/workspace/sessions/session', () => session);
  await client.connect(true);
  await client.selectSession('session');
  for (const summarize of [false, true]) {
    for (const restoreFiles of [false, true]) {
      await client.rewind('message', restoreFiles, summarize);
      const request = requests.filter(item => item.path.endsWith('/rewind')).at(-1)!;
      assert.deepEqual(request.body, { message_id: 'message', summarize, restore_files: restoreFiles });
    }
  }
});

test('completed rewind does not reload its history into a different session', async t => {
  const { client, session, overrides, requests } = setup(t);
  const saving = deferred<object>();
  overrides.set('/v1/workspaces/workspace/sessions/session', () => session);
  overrides.set('/v1/workspaces/workspace/sessions/other', () => ({ ...session, id: 'other' }));
  overrides.set('/v1/workspaces/workspace/sessions/session/rewind', () => saving.promise);
  await client.connect(true);
  await client.selectSession('session');
  const rewinding = client.rewind('message', false, true);
  await client.selectSession('other');
  const count = requests.length;
  saving.resolve({});
  await rewinding;
  assert.equal(client.state.sessionID, 'other');
  assert.equal(requests.slice(count).some(item => item.path.includes('/sessions/session/')), false);
});

test('model selection stays bound to the originating session during navigation', async t => {
  const { client, session, overrides, requests } = setup(t);
  const owner = { provider_id: 'native' };
  const model = { provider: 'native', model: 'chosen' };
  const models = { large: { model, owner } };
  const saving = deferred<typeof models>();
  overrides.set('/v1/workspaces/workspace/sessions/session', body => body?.model_selection ? { ...session, models: body.model_selection.models, model_revision: 1 } : { ...session, model_revision: 0 });
  overrides.set('/v1/workspaces/workspace/sessions/other', () => ({ ...session, id: 'other', model_revision: 0 }));
  overrides.set('/v1/workspaces/workspace/config/model', () => saving.promise);
  await client.connect(true);
  await client.selectSession('session');
  const changing = client.chooseModel({ id: 'native', name: 'Native', available: true, owner }, model, 0);
  await client.selectSession('other');
  saving.resolve(models);
  await changing;
  const writes = requests.filter(item => item.method === 'PUT' && item.body?.model_selection);
  assert.equal(writes.length, 1);
  assert.equal(writes[0].path, '/v1/workspaces/workspace/sessions/session');
  assert.deepEqual(writes[0].body.model_selection.models, models);
  assert.equal(client.state.sessionID, 'other');
});

test('stream events arriving during a history snapshot remain newer than the snapshot', async t => {
  const { client, overrides, event, flush } = setup(t);
  await client.connect(true);
  const history = deferred<Message[]>();
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => history.promise);
  const selecting = client.selectSession('session');
  await new Promise(resolve => setImmediate(resolve));
  event({ type: 'message', payload: { type: 'updated', payload: message('new stream content') } });
  flush();
  history.resolve([message('old snapshot')]);
  await selecting;
  flush();
  assert.equal(client.state.messages.message.parts[0].data.text, 'new stream content');
  assert.deepEqual([...client.state.messageIDs], ['message']);
});

test('model selection preserves native ownership, scope, authority, and applies the returned state', async t => {
  const { client, requests, overrides } = setup(t);
  await client.connect(true);
  const owner = { provider_id: 'custom', kind: 'plugin', plugin_id: 'my-plugin' };
  const model = { provider: 'custom', model: 'selected', max_tokens: 9123 };
  const models = { large: { model, owner } };
  overrides.set('/v1/workspaces/workspace/config/model', () => models);
  await client.chooseModel({ id: 'custom', name: 'Custom', available: true, owner }, model, 1);
  const write = requests.find(request => request.path.endsWith('/config/model'))!;
  assert.deepEqual(write.body, { scope: 1, model_type: 'large', model, owner });
  assert.equal(write.headers['Crux-Workspace-Authority-Digest'], 'native');
  assert.deepEqual(requests.find(request => request.path.endsWith('/agent/update'))!.body, { state: models });
  overrides.set('/v1/workspaces/workspace/config/model', () => Response.json({ message: 'Unknown model' }, { status: 400 }));
  const before = requests.filter(request => request.path.endsWith('/agent/update')).length;
  await assert.rejects(client.chooseModel({ id: 'custom', name: 'Custom', available: true, owner }, { ...model, model: 'missing' }, 0), /Unknown model/);
  assert.equal(requests.filter(request => request.path.endsWith('/agent/update')).length, before);
});

test('a reply completing before admission returns does not leave the session busy and uses native delivery mode', async t => {
  const { client, overrides, event, flush, workspace, requests } = setup(t);
  workspace.config.options.tui = { delivery_mode: 'steer' };
  await client.connect(true);
  await client.selectSession('session');
  const admission = deferred<unknown>();
  overrides.set('/v1/workspaces/workspace/agent', () => admission.promise);
  const sending = client.send('fast reply');
  assert.equal(client.state.sessions[0].is_busy, true);
  event({ type: 'run_complete', payload: { type: 'created', payload: { session_id: 'session' } } });
  flush();
  admission.resolve({});
  await sending;
  assert.equal(client.state.sessions[0].is_busy, false);
  assert.equal(requests.find(request => request.path.endsWith('/agent') && request.method === 'POST')!.body.delivery_mode, 'steer');
});

test('switching workspace does not reuse a custom data directory and reconnect preserves the selected path', async t => {
  const { client, requests, overrides, workspace } = setup(t);
  await client.connect(true);
  overrides.set('/v1/workspaces', body => ({ ...workspace, path: body.path, data_dir: '/other/.crux', requested_data_dir: body.data_dir }));
  await client.switchWorkspace('/other');
  await client.connect(false);
  const attaches = requests.filter(request => request.path === '/v1/workspaces');
  assert.equal(attaches[0].body.data_dir, '/custom');
  assert.deepEqual(attaches.slice(1).map(request => [request.body.path, request.body.data_dir]), [['/other', ''], ['/other', '']]);
});

test('an explicit missing startup session fails without selecting a different session', async t => {
  const { client, overrides, requests } = setup(t);
  overrides.set('/ui/bootstrap', () => ({ path: '/project', session_id: 'missing' }));
  await assert.rejects(client.connect(true), /not found/);
  assert.equal(client.state.sessionID, '');
  assert.equal(requests.some(request => request.path.endsWith('/current-session')), false);
});

test('task snapshots ignore older responses and previous workspace lifetimes', async t => {
  const { client, overrides } = setup(t);
  await client.connect(true);
  const old = deferred<unknown>();
  overrides.set('/v1/workspaces/workspace/tasks', () => old.promise);
  const reading = client.refreshTasks();
  const current = [{ id: 'current', type: 'shell', description: 'Checks', ownership: { parent_session_id: 'session' }, state: { status: 'running' } }];
  overrides.set('/v1/workspaces/workspace/tasks', () => current);
  await client.refreshTasks();
  old.resolve([{ ...current[0], id: 'old' }]);
  await reading;
  assert.deepEqual(client.state.tasks.map(task => task.id), ['current']);
  const previous = deferred<unknown>();
  overrides.set('/v1/workspaces/workspace/tasks', () => previous.promise);
  const stale = client.refreshTasks();
  overrides.set('/v1/workspaces/workspace/tasks', () => []);
  await client.switchWorkspace('/other');
  previous.resolve(current);
  await stale;
  assert.equal(client.state.tasks.length, 0);
});

test('task-service failure is visible without blocking the workspace or model setup', async t => {
  const { client, overrides } = setup(t);
  overrides.set('/v1/workspaces/workspace/tasks', () => Response.json({ message: 'Managed task service is unavailable' }, { status: 503 }));
  await client.connect(true);
  assert.equal(client.state.connected, true);
  assert.match(client.state.tasksError, /Managed task service is unavailable/);
  overrides.set('/v1/workspaces/workspace/tasks', () => []);
  await client.refreshTasks();
  assert.equal(client.state.tasksError, '');
});

test('native task notifications refresh task state immediately', async t => {
  const { client, overrides, event, flush } = setup(t);
  await client.connect(true);
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'done', type: 'shell', description: 'Checks', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }]);
  event({ type: 'task_notification', payload: { type: 'created', payload: { task_id: 'done', parent_session_id: 'session', status: 'completed' } } });
  flush();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(client.state.tasks[0]?.id, 'done');
  assert.equal(client.state.activityRevision, 1);
});
