import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { build } from 'vite';
import solid from 'vite-plugin-solid';
import { Window } from 'happy-dom';

const root = path.resolve(import.meta.dirname, '..');
const directory = await mkdtemp(path.join(tmpdir(), 'crux-ui-tests-'));
const window = new Window({ url: 'http://127.0.0.1:8080' });
for (const key of ['window', 'document', 'navigator', 'location', 'history', 'Node', 'Element', 'HTMLElement', 'DocumentFragment', 'MutationObserver', 'CustomEvent', 'Event', 'KeyboardEvent', 'FormData', 'FileReader', 'DOMParser', 'ResizeObserver']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: key === 'window' ? window : window[key] });
}
globalThis.requestAnimationFrame = callback => setTimeout(() => callback(performance.now()), 0);
globalThis.cancelAnimationFrame = clearTimeout;
globalThis.getComputedStyle = window.getComputedStyle.bind(window);
const bundle = await build({
  root, configFile: false, logLevel: 'silent', plugins: [solid(), {
    name: 'ui-test-entry',
    resolveId(id) { if (id === '@xterm/xterm') return '\0test-xterm'; if (id === '@xterm/addon-fit') return '\0test-fit'; if (id === 'virtual:ui-test' || id.endsWith('/virtual:ui-test')) return '\0ui-test'; },
    load(id) {
      if (id === '\0test-xterm') return `export class Terminal {
        cols = 80; rows = 24; buffer = { active: { viewportY: 0, baseY: 0 } }; decoder = new TextDecoder();
        open(host) { this.host = host; }
        write(data, done) { this.host.textContent += typeof data === 'string' ? data : this.decoder.decode(data, { stream: true }); done?.(); }
        reset() { this.host.textContent = ''; this.decoder = new TextDecoder(); }
        loadAddon() {} focus() {} dispose() {} onData() {} onBinary() {} onResize() {} scrollToBottom() {} scrollToLine() {}
      }`;
      if (id === '\0test-fit') return `export class FitAddon { fit() {} }`;
      if (id === '\0ui-test') return `export { App, sessionMarkdown } from ${JSON.stringify(path.join(root, 'src/app.tsx'))}; export { createCruxState } from ${JSON.stringify(path.join(root, 'src/state.ts'))}; export { createLogin, validateAuthReceipt, providerUsage } from ${JSON.stringify(path.join(root, 'src/auth.ts'))}; export { render } from 'solid-js/web';`;
    },
  }],
  build: { write: false, minify: false, rollupOptions: { output: { inlineDynamicImports: true } }, lib: { entry: 'virtual:ui-test', formats: ['es'], fileName: 'test' } },
});
await writeFile(path.join(directory, 'runtime.mjs'), (Array.isArray(bundle) ? bundle[0] : bundle).output.find(item => item.type === 'chunk').code);
const { App, sessionMarkdown, createCruxState, render, createLogin, validateAuthReceipt, providerUsage } = await import(pathToFileURL(path.join(directory, 'runtime.mjs')).href);

async function eventually(check, description) {
  const end = Date.now() + 3000;
  while (!check()) { if (Date.now() > end) throw new Error(`Timed out: ${description}`); await delay(5); }
}

async function setup(t, stored = []) {
  window.history.replaceState(null, '', '/');
  const storage = new Map(stored);
  const storageDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: { getItem: name => storage.get(name) ?? null, setItem: (name, value) => storage.set(name, String(value)), removeItem: name => storage.delete(name) } });
  t.after(() => { if (storageDescriptor) Object.defineProperty(globalThis, 'localStorage', storageDescriptor); else Reflect.deleteProperty(globalThis, 'localStorage'); });
  const requests = [];
  const session = { id: 'session', title: 'Workspace session', archived: false, mode: 'default', message_count: 2, prompt_tokens: 200, completion_tokens: 50, cost: .01, created_at: 1, updated_at: 2, is_busy: true, attached_clients: 1 };
  let sessions = [session];
  let pending = [];
  const workspace = { id: 'workspace', path: '/project', data_dir: '/project/.crux', authority: { mode: 'server', revision: 1, digest: 'native' }, config: { models: { large: { provider: 'native', model: 'model' } }, options: { tui: { delivery_mode: 'queue' } } }, provider_surfaces: [] };
  const messages = [{ id: 'message', session_id: 'session', role: 'user', parts: [{ type: 'text', data: { text: 'Review this code' } }], created_at: 1, updated_at: 1 }];
  const overrides = new Map();
  let socket;
  class Socket {
    constructor() { socket = this; queueMicrotask(() => this.onopen?.()); }
    close() {}
  }
  globalThis.WebSocket = Socket;
  globalThis.fetch = async (url, init) => {
    const body = init.body ? JSON.parse(init.body) : undefined;
    const pathname = url.pathname;
    requests.push({ path: pathname, method: init.method, body, search: url.search });
    let value;
    if (overrides.has(pathname)) value = await overrides.get(pathname)(body, init.method, init);
    else if (pathname === '/ui/bootstrap') value = { path: '/project', version: 'test', settings_path: '/native/crux.json' };
    else if (pathname === '/ui/project-sessions') value = { sessions: [{ ...session, title: `${url.searchParams.get('path')} session` }], more: false };
    else if (pathname === '/v1/workspaces' || pathname === '/v1/workspaces/workspace') value = workspace;
    else if (pathname.endsWith('/config/set')) { workspace.config.options.tui.delivery_mode = body.value; value = {}; }
    else if (pathname.endsWith('/agent')) {
      if (init.method === 'POST') { pending.push({ delivery_mode: body.delivery_mode, submission_id: body.submission_id, prompt: body.prompt }); value = {}; }
      else value = { is_ready: true, model: { name: 'Native model', context_window: 32000 }, model_cfg: { provider: 'native', model: 'model' } };
    }
    else if (pathname.endsWith('/sessions')) {
      if (init.method === 'POST') { const created = { ...session, id: 'new-session', title: 'New session', is_busy: false }; sessions.push(created); value = created; }
      else value = sessions.filter(item => item.archived === (url.searchParams.get('archived') === 'true'));
    }
    else if (/\/sessions\/[^/]+$/.test(pathname)) value = sessions.find(item => item.id === pathname.split('/').at(-1));
    else if (pathname.endsWith('/projects') || pathname.endsWith('/task-notifications')) value = [];
    else if (pathname.endsWith('/jobs/detach')) value = { count: 0 };
    else if (pathname.endsWith('/permissions/skip')) value = { skip: false };
    else if (pathname.endsWith('/mcp/prompts') || pathname.endsWith('/mcp/resources')) value = [];
    else if (pathname.endsWith('/messages')) value = messages;
    else if (pathname.endsWith('/prompts/list')) value = pending;
    else if (pathname.endsWith('/prompts/clear')) { pending = []; value = {}; }
    else if (pathname.endsWith('/pending')) value = { permission: null, question: null };
    else if (pathname.endsWith('/history')) value = [{ id: 'checkpoint', path: 'src/app.ts', content: 'const saved = true;', version: 1, exists: true }];
    else if (pathname.endsWith('/filetracker/files')) value = ['src/app.ts'];
    else if (pathname.endsWith('/tasks')) value = [{ id: 'task', type: 'shell', description: 'Run checks', ownership: { parent_session_id: 'session' }, state: { status: 'running' } }];
    else if (pathname.endsWith('/tasks/task/output')) value = { output: 'Checks passed', output_truncated: false };
    else if (/\/tasks\/[^/]+\/log$/.test(pathname)) {
      const id = pathname.split('/').at(-2);
      const outputPath = pathname.replace(/\/log$/, '/output');
      const output = overrides.has(outputPath) ? await overrides.get(outputPath)(body, init.method, init) : { output: 'Checks passed' };
      const tasks = overrides.has('/v1/workspaces/workspace/tasks') ? await overrides.get('/v1/workspaces/workspace/tasks')() : client.state.tasks;
      const task = output.task ?? tasks.find(task => task.id === id) ?? { id, type: 'shell', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } };
      value = { task, data: Buffer.from(output.output ?? '').toString('base64'), snapshot: true, generation: task.state.started_at ?? 'run', next_offset: 0, truncated: false };
    }
    else if (/\/tasks\/[^/]+\/(message|restart|stop)$/.test(pathname)) {
      const id = pathname.split('/').at(-2);
      value = client.state.tasks.find(task => task.id === id);
    }
    else if (pathname.endsWith('/shell')) value = { output: 'shell-result', exit_code: 0 };
    else if (pathname.endsWith('/title')) { session.title = body.title; value = session; }
    else if (pathname.endsWith('/mode')) { sessions.at(-1).mode = body.mode; value = sessions.at(-1); }
    else if (pathname.endsWith('/fork')) { const fork = { ...session, id: 'branch', forked_from_session_id: 'session' }; sessions.push(fork); value = fork; }
    else if (pathname.endsWith('/commands') || pathname.startsWith('/ui/workspaces/') && pathname.endsWith('/instructions')) value = [];
    else if (pathname.endsWith('/auth/oauth/results')) value = { list: { target: body, results: [] } };
    else if (pathname.endsWith('/skills/catalog')) value = { skills: [{ id: 'builtin:review', name: 'review', description: 'Review code', user_invocable: true, source: 'builtin', enabled: true }], states: [] };
    else if (pathname.endsWith('/skills')) value = [{ id: 'builtin:review', name: 'review', description: 'Review code', user_invocable: true, source: 'builtin' }];
    else if (pathname.endsWith('/files/search')) value = { files: [{ root: '/project', path: 'src/app.ts', file_name: 'app.ts' }] };
    else value = {};
    return value instanceof Response ? value : Response.json(value);
  };
  const client = createCruxState();
  await client.connect(true);
  await client.selectSession('session');
  const container = document.createElement('div'); document.body.append(container);
  const dispose = render(() => App({ client }), container);
  t.after(() => { dispose(); client.dispose(); container.remove(); });
  const button = (text, scope = container) => {
    const element = [...scope.querySelectorAll('button')].find(item => item.textContent.trim() === text || item.getAttribute('aria-label') === text);
    assert.ok(element, `Button exists: ${text}`); return element;
  };
  const enter = (selector, text) => { const input = container.querySelector(selector); assert.ok(input, selector); input.value = text; if (input.tagName === 'TEXTAREA' || ['text', 'search', 'url', 'tel', 'password'].includes(input.type)) input.setSelectionRange(text.length, text.length); input.dispatchEvent(new window.Event('input', { bubbles: true })); return input; };
  const expand = name => { const element = button(name); if (element.getAttribute('aria-expanded') !== 'true') element.click(); return element; };
  const emit = event => socket.onmessage?.({ data: JSON.stringify({ type: 'event', event }) });
  const closeStream = () => socket.onclose?.();
  return { client, container, requests, button, enter, expand, session, workspace, overrides, emit, closeStream };
}

for (const wait of [0, 650]) test(`event stream closure during state restoration retries after ${wait}ms`, async t => {
  const { client, workspace, overrides, closeStream, requests } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace', () => new Promise(resolve => { release = resolve; }));
  const restoring = client.connect(false);
  const rejected = assert.rejects(restoring, /event stream closed while restoring state/);
  await eventually(() => Boolean(release), 'workspace restoration pending');
  closeStream();
  assert.equal(client.state.connected, false);
  if (wait) await delay(wait);
  overrides.delete('/v1/workspaces/workspace');
  release(workspace);
  await rejected;
  assert.equal(client.state.connected, false);
  await eventually(() => client.state.connected, 'event stream reconnected');
  assert.equal(client.state.sessionID, 'session');
  assert.ok(requests.filter(item => item.path === '/v1/workspaces' && item.method === 'POST').length >= 3);
});

test('desktop project catalogs do not attach inactive workspaces and header controls retain sidebar state', async t => {
  let saved = { version: 1, pinned: true, open: true, projects: [{ path: '/project', expanded: true }, { path: '/another-project', expanded: true }] };
  const controls = [];
  window.cruxDesktop = {
    selectDirectory: async () => '/third-project',
    loadProjects: async () => structuredClone(saved),
    saveProjects: async value => { saved = structuredClone(value); return saved; },
    windowControl: async action => { controls.push(action); },
  };
  t.after(() => { delete window.cruxDesktop; });
  const { container, requests, button } = await setup(t);
  await eventually(() => container.textContent.includes('/another-project session'), 'separate project catalogs');
  assert.equal(requests.filter(item => item.path === '/v1/workspaces' && item.method === 'POST').length, 1);
  assert.deepEqual(new Set(requests.filter(item => item.path === '/ui/project-sessions').map(item => new URLSearchParams(item.search).get('path'))), new Set(['/project', '/another-project']));
  assert.ok(container.querySelector('.desktop-header'));
  assert.ok(container.querySelector('.project-sidebar [aria-label="Sessions"]'));
  assert.ok(container.querySelector('.project-sidebar [aria-label="Search sessions"]'));
  assert.ok(container.querySelector('.project-sidebar [aria-label="Session filter"]'));
  assert.ok(container.querySelector('.project-sidebar [aria-label="Actions for Workspace session"]'));
  assert.equal(container.querySelector('.panel [data-section="sessions"]'), null);
  assert.equal(container.querySelector('.panel [aria-label="Sessions"]'), null);
  assert.equal(container.querySelector('.side-tab'), null);
  assert.equal(container.querySelector('.project-tab'), null);
  button('Unpin').click();
  assert.equal(saved.pinned, false);
  assert.ok(container.querySelector('.project-sidebar.floating'));
  button('Collapse projects').click();
  assert.equal(saved.open, false);
  assert.equal(container.querySelector('.project-sidebar'), null);
  button('Open projects').click();
  assert.equal(saved.open, true);
  button('Remove shortcut /project').click();
  await delay(10);
  assert.equal(saved.projects.some(project => project.path === '/project'), false);
  button('Minimize window').click();
  button('Maximize or restore window').click();
  button('Close window').click();
  assert.deepEqual(controls, ['minimize', 'maximize', 'close']);
});

test('desktop remote project browsing retains the selected connection without attaching it', async t => {
  let saved = { version: 1, pinned: true, open: true, projects: [] };
  window.cruxDesktop = {
    selectDirectory: async () => undefined,
    loadProjects: async () => structuredClone(saved),
    saveProjects: async value => { saved = structuredClone(value); return saved; },
    windowControl: async () => {},
  };
  t.after(() => { delete window.cruxDesktop; });
  const { container, button, overrides, requests } = await setup(t);
  let releaseFirst;
  overrides.set('/ui/connections', () => [{ name: 'first', address: 'first.example' }, { name: 'second', address: 'second.example' }]);
  overrides.set('/ui/connections/first/browser', () => new Promise(resolve => { releaseFirst = resolve; }));
  overrides.set('/ui/connections/second/browser', () => ({ roots: ['/second'], path: '/second/project', entries: [] }));
  overrides.set('/ui/connections/second/project-sessions', () => ({ sessions: [], more: false }));
  await eventually(() => container.querySelector('.project-sidebar'), 'desktop projects');
  button('Add remote project').click();
  const select = () => container.querySelector('[aria-label="Saved connection"]');
  await eventually(() => select()?.querySelector('option[value="second"]'), 'saved connections loaded');
  select().value = 'first'; select().dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => Boolean(releaseFirst), 'first connection browse pending');
  select().value = ''; select().dispatchEvent(new window.Event('change', { bubbles: true }));
  select().value = 'second'; select().dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.querySelector('[aria-label="Remote project directory"]').value === '/second/project', 'second connection directory');
  releaseFirst({ roots: ['/first'], path: '/first/project', entries: [] });
  await delay(0);
  assert.equal(container.querySelector('[aria-label="Remote project directory"]').value, '/second/project');
  select().closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => requests.some(item => item.path === '/ui/connections/second/project-sessions'), 'selected connection catalog');
  assert.ok(saved.projects.some(project => project.connection === 'second' && project.path === '/second/project'));
  assert.equal(requests.filter(item => item.path.endsWith('/v1/workspaces') && item.method === 'POST').length, 1);
});

test('integrated web view retains its edge tab without desktop project requests', async t => {
  const { container, requests } = await setup(t);
  assert.equal(container.querySelector('.desktop-header'), null);
  assert.equal(container.querySelector('.project-sidebar'), null);
  assert.ok(container.querySelector('.side-tab'));
  assert.ok(container.querySelector('.panel [data-section="sessions"]'));
  assert.ok(container.querySelector('.panel [aria-label="Sessions"]'));
  assert.equal(requests.some(item => item.path === '/ui/project-sessions'), false);
});

for (const scope of ['0', '1']) test(`advanced configuration saves native JSON at scope ${scope} and removes overrides`, async t => {
  const { container, button, enter, requests, overrides, workspace } = await setup(t);
  overrides.set('/v1/workspaces/workspace/config/set', body => { workspace.config.options.disabled_tools = body.value; return {}; });
  overrides.set('/v1/workspaces/workspace/config/remove', () => { delete workspace.config.options.disabled_tools; return {}; });
  button('Settings').click(); button('Configuration').click();
  container.querySelector('form[aria-label="Advanced configuration"]').closest('details').open = true;
  const select = container.querySelector('[aria-label="Configuration write scope"]');
  select.value = scope; select.dispatchEvent(new window.Event('change', { bubbles: true }));
  enter('[aria-label="Configuration key"]', 'options.disabled_tools');
  enter('[aria-label="Configuration JSON value"]', '["bash", "download"]');
  button('Save configuration').click();
  await eventually(() => container.textContent.includes('Saved options.disabled_tools'), 'native configuration saved');
  assert.deepEqual(requests.find(item => item.path.endsWith('/config/set')).body, { scope: Number(scope), key: 'options.disabled_tools', value: ['bash', 'download'] });
  enter('[aria-label="Configuration JSON value"]', 'not JSON');
  button('Remove saved override').click();
  await eventually(() => container.textContent.includes('Removed options.disabled_tools'), 'native override removed');
  assert.deepEqual(requests.find(item => item.path.endsWith('/config/remove')).body, { scope: Number(scope), key: 'options.disabled_tools' });
});

for (const value of [false, 0, '', null, { custom: ['value'] }]) test(`advanced configuration preserves JSON ${JSON.stringify(value)}`, async t => {
  const { button, enter, requests, overrides, container } = await setup(t);
  overrides.set('/v1/workspaces/workspace/config/set', () => ({}));
  button('Settings').click(); button('Configuration').click();
  container.querySelector('form[aria-label="Advanced configuration"]').closest('details').open = true;
  enter('[aria-label="Configuration key"]', 'tools.example');
  enter('[aria-label="Configuration JSON value"]', JSON.stringify(value));
  button('Save configuration').click();
  await eventually(() => requests.some(item => item.path.endsWith('/config/set')), 'JSON value submitted');
  assert.deepEqual(requests.find(item => item.path.endsWith('/config/set')).body.value, value);
});

test('advanced configuration retains failed drafts and distinguishes a saved write from refresh failure', async t => {
  const { container, button, enter, requests, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/config/set', () => Response.json({ message: 'config file updated but failed to publish in-memory state' }, { status: 500 }));
  button('Settings').click(); button('Configuration').click();
  container.querySelector('form[aria-label="Advanced configuration"]').closest('details').open = true;
  enter('[aria-label="Configuration key"]', 'hooks.PreToolUse');
  enter('[aria-label="Configuration JSON value"]', '{');
  button('Save configuration').click();
  await eventually(() => container.textContent.includes('Enter a JSON value'), 'invalid JSON error');
  assert.equal(requests.some(item => item.path.endsWith('/config/set')), false);
  const draft = '[{"command":"echo hook"}]';
  enter('[aria-label="Configuration JSON value"]', draft);
  button('Save configuration').click();
  await eventually(() => container.textContent.includes('config file updated but failed to publish'), 'native partial failure');
  assert.equal(container.querySelector('[aria-label="Configuration JSON value"]').value, draft);
  assert.equal(container.textContent.includes('Saved hooks.PreToolUse'), false);
  overrides.set('/v1/workspaces/workspace/config/set', () => {
    overrides.set('/v1/workspaces/workspace/agent', () => Response.json({ message: 'Agent view unavailable' }, { status: 503 }));
    return {};
  });
  button('Save configuration').click();
  await eventually(() => container.textContent.includes('Configuration was saved, but refreshing the current view failed: Agent view unavailable'), 'post-save refresh failure');
  assert.equal(container.querySelector('[aria-label="Configuration JSON value"]').value, draft);
});

test('advanced configuration sends the exact native owner for provider disabled state', async t => {
  const { client, container, button, enter, requests, overrides, workspace } = await setup(t);
  const owner = { provider_id: 'native', plugin_id: 'fixture', digest: 'original' };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native', owner, available: true }];
  await client.refreshWorkspace();
  overrides.set('/v1/workspaces/workspace/config/set', () => ({}));
  button('Settings').click(); button('Configuration').click();
  container.querySelector('form[aria-label="Advanced configuration"]').closest('details').open = true;
  enter('[aria-label="Configuration key"]', 'providers.native.disable');
  enter('[aria-label="Configuration JSON value"]', 'false');
  button('Save configuration').click();
  await eventually(() => container.textContent.includes('Saved providers.native.disable'), 'provider state saved');
  assert.deepEqual(requests.find(item => item.path.endsWith('/config/set')).body, { scope: 1, key: 'providers.native.disable', value: false, owner });
});

test('advanced configuration does not refresh a new workspace after an old write completes', async t => {
  const { client, container, button, enter, requests, overrides, workspace } = await setup(t);
  let finish;
  overrides.set('/v1/workspaces/workspace/config/set', () => new Promise(resolve => { finish = () => resolve({}); }));
  button('Settings').click(); button('Configuration').click();
  container.querySelector('form[aria-label="Advanced configuration"]').closest('details').open = true;
  enter('[aria-label="Configuration key"]', 'options.disabled_tools');
  enter('[aria-label="Configuration JSON value"]', '[]');
  button('Save configuration').click();
  await eventually(() => finish, 'configuration write pending');
  const next = { ...workspace, id: 'other', path: '/other' };
  client.api.workspace = next; client.set('workspace', next);
  const start = requests.length;
  finish(); await delay(20);
  assert.equal(container.querySelector('[aria-label="Configuration key"]').value, '');
  assert.equal(container.textContent.includes('Saved options.disabled_tools'), false);
  assert.equal(requests.slice(start).some(item => item.path === '/v1/workspaces/other/agent' || item.path === '/v1/workspaces/other/config/set'), false);
});

test('session instruction editor saves literal text and preserves drafts after conflicts', async t => {
  const { client, container, button, enter, session, overrides, requests } = await setup(t);
  session.instructions = { mode: 'inherit', text: '', revision: 0 };
  let conflict = false;
  overrides.set('/v1/workspaces/workspace/sessions/session', (body, method) => {
    if (method === 'PUT') {
      if (conflict) return Response.json({ message: 'session instructions changed; reload before saving' }, { status: 409 });
      session.instructions = { ...body.instruction_update, revision: 1 };
    }
    return session;
  });
  button('Instructions').click();
  await eventually(() => !container.querySelector('[aria-label="Session instruction mode"]').disabled, 'session instruction load');
  const mode = container.querySelector('[aria-label="Session instruction mode"]');
  mode.value = 'replace'; mode.dispatchEvent(new window.Event('change', { bubbles: true }));
  enter('[aria-label="Session prompt"]', 'A literal {{role}}\nfor this conversation.');
  button('Save session instructions').click();
  await eventually(() => client.state.currentSession?.instructions?.revision === 1, 'session instruction save');
  assert.deepEqual(requests.find(item => item.method === 'PUT' && item.body?.instruction_update).body, { instruction_update: { mode: 'replace', text: 'A literal {{role}}\nfor this conversation.', revision: 0 } });
  assert.equal(requests.some(item => item.method === 'POST' && item.path.endsWith('/config/set')), false);
  conflict = true;
  enter('[aria-label="Session prompt"]', 'Keep this unsaved text');
  button('Save session instructions').click();
  await eventually(() => container.textContent.includes('reload before saving'), 'instruction conflict');
  assert.equal(container.querySelector('[aria-label="Session prompt"]').value, 'Keep this unsaved text');
});

test('late instruction saves cannot replace another connection session instructions', async t => {
  const { client, workspace, container, button, enter, session, overrides } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/sessions/session', (_body, method) => method === 'PUT'
    ? new Promise(resolve => { release = resolve; }) : session);
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  const remoteSession = { ...session, instructions: { mode: 'append', text: 'Remote instructions', revision: 2 } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/sessions/session', () => remoteSession);
  button('Instructions').click();
  await eventually(() => !container.querySelector('[aria-label="Session instruction mode"]').disabled, 'local instructions loaded');
  const mode = container.querySelector('[aria-label="Session instruction mode"]');
  mode.value = 'append'; mode.dispatchEvent(new window.Event('change', { bubbles: true }));
  enter('[aria-label="Session prompt"]', 'Local instructions');
  button('Save session instructions').click();
  await eventually(() => Boolean(release), 'local instruction save pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await client.selectSession('session');
  await eventually(() => client.state.currentSession?.instructions?.text === 'Remote instructions', 'remote session selected');
  release({ ...session, instructions: { mode: 'append', text: 'Local instructions', revision: 3 } });
  await delay(0);
  assert.equal(client.state.currentSession.instructions.text, 'Remote instructions');
  assert.equal(client.state.currentSession.instructions.revision, 2);
});

test('native instruction editor reads and saves the selected configured file', async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  let loaded = false;
  let document = { path: '/project/AGENTS.md', text: 'Native source', revision: 'original', exists: true };
  overrides.set('/ui/workspaces/workspace/instructions', (body, method) => {
    const request = requests.at(-1);
    if (!new URLSearchParams(request.search).has('path')) return [{ path: document.path, scope: 'project' }];
    loaded = true;
    if (method === 'PUT') { assert.deepEqual(body, { text: 'Updated native source', revision: 'original' }); document = { ...document, text: body.text, revision: 'saved' }; }
    return document;
  });
  button('Instructions').click();
  await eventually(() => loaded && container.querySelector('[aria-label="Native instructions"]')?.value === 'Native source', 'native file loaded');
  enter('[aria-label="Native instructions"]', 'Updated native source');
  assert.equal(container.querySelector('[aria-label="Instruction file"]').disabled, true);
  button('Save native instructions').click();
  await eventually(() => container.textContent.includes('Native instruction file saved'), 'native file saved');
  const saved = requests.find(item => item.method === 'PUT' && item.path.endsWith('/instructions'));
  assert.equal(new URLSearchParams(saved.search).get('path'), '/project/AGENTS.md');
});

test('instruction publication retries use the saved file revision', async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  let attempts = 0;
  const path = '/project/provider.txt';
  overrides.set('/ui/workspaces/workspace/instructions', (body, method) => {
    if (!new URLSearchParams(requests.at(-1).search).has('path')) return [{ path, scope: 'provider', provider: 'native' }];
    if (method === 'GET') return { path, text: 'Before', revision: 'before', exists: true };
    attempts++;
    assert.equal(body.text, 'After');
    assert.equal(body.revision, attempts === 1 ? 'before' : 'saved');
    const saved = { path, text: 'After', revision: 'saved', exists: true };
    return attempts === 1 ? Response.json({ message: 'Runtime publication failed', saved_document: saved }, { status: 502 }) : saved;
  });
  button('Instructions').click();
  await eventually(() => container.querySelector('[aria-label="Native instructions"]')?.value === 'Before', 'instruction file loaded');
  enter('[aria-label="Native instructions"]', 'After');
  button('Save native instructions').click();
  await eventually(() => container.textContent.includes('Retry instruction publication'), 'publication retry offered');
  button('Retry instruction publication').click();
  await eventually(() => container.textContent.includes('Native instruction file saved'), 'publication retried');
  assert.equal(attempts, 2);
});

test('instruction editor keeps client and remote files with identical paths separate', async t => {
  const { client, container, button, enter, overrides, requests } = await setup(t);
  client.api.connection = 'remote';
  const file = '/shared/instructions.txt';
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/_ui/instructions', (body, method) => {
    const query = new URLSearchParams(requests.at(-1).search);
    if (!query.has('path')) return [{ path: file, scope: 'project', source: 'remote' }, { path: file, scope: 'provider', provider: 'native', source: 'client' }];
    assert.equal(query.get('path'), file);
    const source = query.get('source');
    assert.ok(['client', 'remote'].includes(source));
    if (method === 'PUT') assert.equal(source, 'client');
    return { path: file, text: body?.text ?? `${source} contents`, revision: 'revision', exists: true };
  });
  button('Instructions').click();
  await eventually(() => container.querySelector('[aria-label="Native instructions"]')?.value === 'remote contents', 'remote instruction file loaded');
  const select = container.querySelector('[aria-label="Instruction file"]');
  assert.equal(select.options.length, 2);
  select.value = JSON.stringify(['client', file]);
  select.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.querySelector('[aria-label="Native instructions"]')?.value === 'client contents', 'client instruction file loaded');
  enter('[aria-label="Native instructions"]', 'Updated client contents');
  button('Save native instructions').click();
  await eventually(() => requests.some(item => item.method === 'PUT' && new URLSearchParams(item.search).get('source') === 'client'), 'client instruction save routed');
});

test('interaction dropdown exposes native planning and permission combinations during a turn', async t => {
  const { client, container, session, overrides, requests } = await setup(t);
  session.is_busy = true; client.set('currentSession', { ...session });
  let skip = false;
  overrides.set('/v1/workspaces/workspace/permissions/skip', (body, method) => { if (method === 'POST') skip = body.skip; return { skip }; });
  const selector = container.querySelector('[aria-label="Interaction mode"]');
  assert.deepEqual([...selector.options].map(option => option.value), ['normal', 'plan', 'yolo', 'plan-yolo']);
  for (const mode of ['yolo', 'plan-yolo', 'plan', 'normal']) {
    assert.equal(selector.disabled, false);
    selector.value = mode; selector.dispatchEvent(new window.Event('change', { bubbles: true }));
    await eventually(() => !client.state.modeSaving && selector.value === mode, `mode ${mode} saved`);
    assert.equal(client.state.autoApprove, mode.includes('yolo'));
    assert.equal(client.state.currentSession.mode, mode.startsWith('plan') ? 'plan' : 'default');
  }
  assert.ok(requests.some(item => item.path.endsWith('/permissions/skip') && item.body?.skip === true));
  assert.ok(requests.some(item => item.path.endsWith('/permissions/skip') && item.body?.skip === false));
});

test('interaction dropdown restores the actual mode after a rejected change', async t => {
  const { client, container, session, overrides } = await setup(t);
  session.is_busy = false; client.set('currentSession', { ...session });
  overrides.set('/v1/workspaces/workspace/sessions/session/mode', () => Response.json({ message: 'Mode change rejected' }, { status: 409 }));
  const selector = container.querySelector('[aria-label="Interaction mode"]');
  selector.value = 'plan'; selector.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.textContent.includes('Mode change rejected') && !client.state.modeSaving && selector.value === 'normal', 'actual mode restored');
  assert.equal(client.state.currentSession.mode, 'default');
  assert.equal(client.state.autoApprove, false);
});

test('interaction dropdown displays a successful plan update when auto-approval fails', async t => {
  const { client, container, session, overrides } = await setup(t);
  session.is_busy = false; client.set('currentSession', { ...session });
  overrides.set('/v1/workspaces/workspace/permissions/skip', (_body, method) => method === 'POST' ? Response.json({ message: 'Permission update rejected' }, { status: 409 }) : { skip: false });
  const selector = container.querySelector('[aria-label="Interaction mode"]');
  selector.value = 'plan-yolo'; selector.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.textContent.includes('Permission update rejected') && !client.state.modeSaving && selector.value === 'plan', 'partial mode update reflected');
  assert.equal(client.state.currentSession.mode, 'plan');
  assert.equal(client.state.autoApprove, false);
});

test('disabled providers are hidden from model selection and optional in provider settings', async t => {
  const { client, container, button, workspace, overrides } = await setup(t);
  workspace.config.providers = { disabled: { disable: true } };
  workspace.provider_surfaces = [{ id: 'enabled', name: 'Enabled Provider', available: true, models: [{ id: 'active-model', name: 'Active model' }] }, { id: 'disabled', name: 'Disabled Provider', available: true, models: [{ id: 'disabled-model', name: 'Disabled model' }] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', providers: [] }));
  await client.refreshWorkspace();
  button('Choose model').click();
  assert.equal(container.querySelector('.model-popover').textContent.includes('Disabled model'), false);
  assert.ok(container.querySelector('.model-popover').textContent.includes('Active model'));
  button('Choose model').click();
  button('Settings').click(); button('Providers').click();
  assert.equal(container.textContent.includes('Disabled Provider'), false);
  const toggle = [...container.querySelectorAll('label')].find(item => item.textContent.includes('Show disabled providers')).querySelector('input');
  toggle.checked = true; toggle.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.textContent.includes('Disabled Provider'), 'disabled provider management');
});

for (const mismatch of [false, true]) test(`selected task lookup exposes failure and retries with mismatch=${mismatch}`, async t => {
  const { client, container, button, overrides } = await setup(t);
  client.set('messages', 'message', { role: 'assistant', parts: [{ type: 'tool_result', data: { tool_call_id: 'missing-call', name: 'agent', content: 'Background task', metadata: JSON.stringify({ task_id: 'historical' }) } }] });
  overrides.set('/v1/workspaces/workspace/tasks/historical/output', () => mismatch ? { task: { id: 'different' } } : Response.json({ message: 'Task lookup failed' }, { status: 503 }));
  container.querySelector('.tool-head').click();
  button('Inspect task').click();
  await eventually(() => container.textContent.includes(mismatch ? 'did not return task historical' : 'Task lookup failed'), 'selected task error');
  overrides.set('/v1/workspaces/workspace/tasks/historical/output', () => ({ task: { id: 'historical', type: 'shell', description: 'Recovered task', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }, output: 'Recovered output', output_truncated: false }));
  button('Retry selected task').click();
  await eventually(() => container.textContent.includes('Recovered output'), 'retrieved task output');
  assert.ok(container.textContent.includes('Recovered task'));
  assert.equal(container.textContent.includes('Retry selected task'), false);
});

for (const failed of ['history', 'filetracker/files']) test(`file inspector retains the independent result when ${failed} fails`, async t => {
  const { expand, container, button, overrides } = await setup(t);
  overrides.set(`/v1/workspaces/workspace/sessions/session/${failed}`, () => Response.json({ message: 'Read unavailable' }, { status: 503 }));
  button('Background tasks').click();
  expand('Files');
  await eventually(() => container.textContent.includes('Read unavailable'), 'section failure visible');
  if (failed === 'history') await eventually(() => container.querySelector('[aria-label="Last read: src/app.ts"]'), 'independent file list');
  else {
    await eventually(() => [...container.querySelectorAll('button')].some(item => item.textContent === 'v1'), 'independent checkpoints');
    button('v1').click();
    assert.equal(container.querySelector('.code-preview').textContent, 'const saved = true;');
  }
});

test('checkpoint refresh retains the selected native version until it is removed', async t => {
  const { expand, client, container, button, overrides } = await setup(t);
  button('Background tasks').click();
  expand('Files');
  await eventually(() => [...container.querySelectorAll('button')].some(item => item.textContent === 'v1'), 'checkpoint loaded');
  button('v1').click();
  overrides.set('/v1/workspaces/workspace/sessions/session/history', () => [{ id: 'checkpoint', path: 'src/app.ts', content: 'updated checkpoint', version: 1, exists: true }]);
  client.set('activityRevision', value => value + 1);
  await eventually(() => container.querySelector('.code-preview')?.textContent === 'updated checkpoint', 'selection refreshed by ID');
  overrides.set('/v1/workspaces/workspace/sessions/session/history', () => []);
  client.set('activityRevision', value => value + 1);
  await eventually(() => !container.querySelector('.code-preview'), 'removed checkpoint selection cleared');
});

test('file inspector refreshes missed changes after reconnect without a file event', async t => {
  const { expand, client, container, button, overrides, requests } = await setup(t);
  button('Background tasks').click();
  expand('Files');
  await eventually(() => [...container.querySelectorAll('button')].some(item => item.textContent === 'v1'), 'checkpoint loaded');
  button('v1').click();
  client.set('connected', false);
  overrides.set('/v1/workspaces/workspace/sessions/session/history', () => [{ id: 'checkpoint', path: 'src/app.ts', content: 'recovered checkpoint', version: 1, exists: true }]);
  overrides.set('/v1/workspaces/workspace/sessions/session/filetracker/files', () => ['src/recovered.ts']);
  const historyReads = () => requests.filter(item => item.path.endsWith('/sessions/session/history')).length;
  const disconnectedReads = historyReads();
  client.set('fileRevision', value => value + 1);
  await delay(0);
  assert.equal(historyReads(), disconnectedReads);
  client.set('connected', true);
  await eventually(() => container.querySelector('.code-preview')?.textContent === 'recovered checkpoint', 'checkpoint recovered');
  await eventually(() => container.querySelector('[aria-label="Last read: src/recovered.ts"]'), 'file list recovered');
  assert.equal(container.querySelector('[aria-label="Last read: src/app.ts"]'), null);
});

test('workspace browsing uses native directory paths without opening a workspace implicitly', async t => {
  const { container, button, requests, overrides } = await setup(t);
  overrides.set('/v1/browser', () => ({ roots: ['/root'], path: '/root/project', parent: '/root', entries: [{ name: 'nested', path: '/root/project/nested', directory: true }, { name: 'file.txt', path: '/root/project/file.txt', directory: false }], truncated: true }));
  button('Command palette').click();
  button('Open workspace…').click();
  button('Browse directories').click();
  await eventually(() => container.querySelector('[aria-label="Workspace directories"]'), 'native directory listing');
  const listing = container.querySelector('[aria-label="Workspace directories"]');
  assert.equal(listing.textContent.includes('file.txt'), false);
  assert.match(listing.textContent, /listing is truncated/);
  button('Use this directory').click();
  assert.equal(container.querySelector('[aria-label="Workspace directory"]').value, '/root/project');
  assert.equal(requests.filter(item => item.path === '/v1/workspaces' && item.method === 'POST').length, 1);
  button('nested').click();
  await eventually(() => requests.some(item => item.path === '/v1/browser' && new URLSearchParams(item.search).get('path') === '/root/project/nested'), 'native child path');
  await eventually(() => !button('Browse directories').disabled, 'directory load completed');
  overrides.set('/v1/browser', () => Response.json({ message: 'Directory unavailable' }, { status: 400 }));
  button('Browse roots').click();
  await eventually(() => container.textContent.includes('Directory unavailable'), 'visible browser error');
});

for (const mode of ['plain', 'git-init', 'git-clone']) test(`workspace creation preserves native ${mode} mode without opening implicitly`, async t => {
  const { container, button, enter, requests, overrides } = await setup(t);
  overrides.set('/v1/browser', () => ({ roots: ['/root'], path: '/root', entries: [] }));
  overrides.set('/v1/browser/create', () => ({ path: '/root/projects/new-project' }));
  button('Command palette').click(); button('Open workspace…').click(); button('Create or clone…').click();
  await eventually(() => container.querySelector('[aria-label="Creation root"]').value === '/root', 'creation roots loaded');
  enter('[aria-label="New workspace directory"]', 'projects/new-project');
  const select = container.querySelector('[aria-label="Workspace creation mode"]');
  select.value = mode; select.dispatchEvent(new window.Event('change', { bubbles: true }));
  if (mode === 'git-clone') enter('[aria-label="Repository clone URL"]', 'git@example.com:team/project.git');
  button('Create directory').click();
  await eventually(() => container.querySelector('[aria-label="Workspace directory"]').value === '/root/projects/new-project', 'created path selected');
  assert.equal(container.querySelector('[aria-label="Create workspace directory"]'), null);
  assert.deepEqual(requests.find(item => item.path === '/v1/browser/create').body, { root: '/root', relative_path: 'projects/new-project', mode, ...(mode === 'git-clone' ? { clone_url: 'git@example.com:team/project.git' } : {}) });
  assert.equal(requests.filter(item => item.path === '/v1/workspaces' && item.method === 'POST').length, 1);
  assert.match(container.textContent, /Open it when ready/);
});

test('workspace creation retains inputs after native failure and rejects traversal without a request', async t => {
  const { container, button, enter, requests, overrides } = await setup(t);
  overrides.set('/v1/browser', () => ({ roots: ['/root'], path: '/root', entries: [] }));
  overrides.set('/v1/browser/create', () => Response.json({ message: 'Repository access failed' }, { status: 400 }));
  button('Command palette').click(); button('Open workspace…').click(); button('Create or clone…').click();
  await eventually(() => container.querySelector('[aria-label="Creation root"]').value === '/root', 'creation roots loaded');
  enter('[aria-label="New workspace directory"]', '../escape');
  button('Create directory').click();
  await eventually(() => container.textContent.includes('without empty, . or .. segments'), 'invalid path shown');
  assert.equal(requests.some(item => item.path === '/v1/browser/create'), false);
  enter('[aria-label="New workspace directory"]', 'project');
  const select = container.querySelector('[aria-label="Workspace creation mode"]');
  select.value = 'git-clone'; select.dispatchEvent(new window.Event('change', { bubbles: true }));
  enter('[aria-label="Repository clone URL"]', 'ssh://git@example.com/team/project');
  button('Create directory').click();
  await eventually(() => container.textContent.includes('Repository access failed'), 'native failure shown');
  assert.equal(container.querySelector('[aria-label="New workspace directory"]').value, 'project');
  assert.equal(container.querySelector('[aria-label="Repository clone URL"]').value, 'ssh://git@example.com/team/project');
  assert.equal(select.value, 'git-clone');
  assert.equal(container.querySelector('[aria-label="Workspace directory"]').value, '/project');
});

test('closing workspace creation aborts its request and ignores a late result', async t => {
  const { container, button, enter, overrides } = await setup(t);
  let finish, signal;
  overrides.set('/v1/browser', () => ({ roots: ['/root'], path: '/root', entries: [] }));
  overrides.set('/v1/browser/create', (_body, _method, init) => { signal = init.signal; return new Promise(resolve => { finish = () => resolve({ path: '/root/late' }); }); });
  button('Command palette').click(); button('Open workspace…').click(); button('Create or clone…').click();
  await eventually(() => container.querySelector('[aria-label="Creation root"]').value === '/root', 'creation roots loaded');
  enter('[aria-label="New workspace directory"]', 'late');
  button('Create directory').click();
  await eventually(() => finish, 'creation pending');
  button('Hide creation form').click();
  assert.equal(signal.aborted, true);
  finish();
  await delay(0);
  assert.equal(container.querySelector('[aria-label="Workspace directory"]').value, '/project');
  assert.equal(container.textContent.includes('Open it when ready.'), false);
});

for (const usage of [
  { unseen: 0, estimated: false, total: 250 },
  { unseen: 150, estimated: false, total: 400 },
  { unseen: 0, estimated: true, total: 250 },
]) test(`context meter follows native occupancy with unseen=${usage.unseen} estimated=${usage.estimated}`, async t => {
  const { expand, client, container, button } = await setup(t);
  client.set('currentSession', { unseen_local_tokens: usage.unseen, estimated_usage: usage.estimated });
  button('Background tasks').click();
  expand('Session');
  const card = [...container.querySelectorAll('.card')].find(item => item.querySelector('strong')?.textContent === 'Context');
  assert.ok(card);
  const estimated = usage.estimated || usage.unseen > 0;
  assert.ok(card.querySelector('.card-head').textContent.includes(`${estimated ? '~' : ''}${usage.total}`));
  assert.equal(card.querySelector('.meter > span').style.width, `${usage.total / 32000 * 100}%`);
  assert.equal(card.textContent.includes('Context usage is estimated.'), estimated);
  client.set('currentSession', { unseen_local_tokens: 0, estimated_usage: false });
  assert.equal(card.textContent.includes('Context usage is estimated.'), false);
  assert.equal(card.querySelector('.meter > span').style.width, `${250 / 32000 * 100}%`);
});

test('project initialization status ignores an aborted previous workspace response', async t => {
  const { client, container, workspace, overrides } = await setup(t);
  await client.selectSession('');
  let resolveStatus;
  let signal;
  overrides.set('/v1/workspaces/workspace/project/needs-init', (_body, _method, init) => {
    signal = init.signal;
    return new Promise(resolve => { resolveStatus = resolve; });
  });
  client.set('connected', false);
  client.set('connected', true);
  await eventually(() => Boolean(resolveStatus), 'original initialization status requested');
  overrides.set('/v1/workspaces/other/project/needs-init', () => ({ needs_init: true }));
  const next = { ...workspace, id: 'other', path: '/other' };
  client.api.workspace = next;
  client.set('workspace', next);
  await eventually(() => container.textContent.includes('Initialize this project?'), 'new workspace initialization banner');
  assert.equal(signal.aborted, true);
  resolveStatus({ needs_init: false });
  await delay(20);
  assert.match(container.textContent, /Initialize this project\?/);
});

for (const scope of ['global', 'runtime']) test(`small-model default preserves native settings with ${scope} scope`, async t => {
  const { client, container, button, workspace, session, overrides, requests } = await setup(t);
  const owner = { provider_id: 'native' };
  const model = { provider: 'native', model: 'compact', max_tokens: 4096, reasoning_effort: 'low', provider_options: { retained: true } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [{ id: 'model', name: 'Main' }, { id: 'compact', name: 'Compact' }] }];
  overrides.set('/v1/workspaces/workspace/agent/default-small-model', () => model);
  overrides.set('/v1/workspaces/workspace/config/model', () => ({ small: { model, owner } }));
  overrides.set('/v1/workspaces/workspace/config/model-overrides', () => ({ small: { model, owner } }));
  overrides.set('/v1/workspaces/workspace/sessions/session', body => body?.model_selection ? { ...session, models: body.model_selection.models } : session);
  await client.refreshWorkspace();
  button('Settings').click();
  if (scope === 'runtime') button('Runtime').click();
  container.querySelectorAll('.settings .model-button')[1].click();
  button('Use Native Provider default small model').click();
  await eventually(() => !container.querySelector('[aria-label="Choose model"]'), 'default small model saved');
  assert.match(requests.find(item => item.path.endsWith('/agent/default-small-model')).search, /provider_id=native/);
  if (scope === 'runtime') {
    assert.deepEqual(requests.find(item => item.path.endsWith('/config/model-overrides')).body, { state: { small: { model, owner } } });
    assert.equal(requests.some(item => item.path.endsWith('/config/model')), false);
  } else assert.deepEqual(requests.find(item => item.path.endsWith('/config/model')).body, { scope: 0, model_type: 'small', model, owner });
});

test('opening task details displays and acknowledges only that task’s native notifications', async t => {
  const { container, button, overrides, requests } = await setup(t);
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'task', type: 'agent', description: 'Research completed', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }]);
  overrides.set('/v1/workspaces/workspace/task-notifications', () => [
    { notification_id: 'notice', task_id: 'task', status: 'completed', summary: 'Found the answer', final_output: '**Result**', exit_code: 0 },
    { notification_id: 'other-notice', task_id: 'other', status: 'completed', summary: 'Other result' },
  ]);
  button('Background tasks').click();
  await eventually(() => container.querySelector('[data-task-id="task"]'), 'task list');
  assert.equal(requests.some(item => item.path.includes('/task-notifications')), false);
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => requests.some(item => item.path.endsWith('/task-notifications/notice/read')), 'native notification acknowledgement');
  const notice = container.querySelector('[aria-label="Task completion notice"]');
  assert.match(notice.textContent, /Found the answer/);
  assert.match(notice.textContent, /Exit code: 0/);
  assert.equal(notice.querySelector('.markdown strong').textContent, 'Result');
  assert.equal(container.textContent.includes('Other result'), false);
  assert.equal(requests.some(item => item.path.endsWith('/task-notifications/other-notice/read')), false);
  const listing = requests.find(item => item.path.endsWith('/task-notifications'));
  assert.match(listing.search, /parent_session_id=session/);
  assert.match(listing.search, /unread_only=true/);
});

test('task notification acknowledgement failures retain the notice and can be retried', async t => {
  const { container, button, overrides, requests } = await setup(t);
  const notice = { notification_id: 'notice', task_id: 'task', status: 'failed', summary: 'Task failed', error_message: 'Task execution failed' };
  let reject = true;
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'task', type: 'shell', description: 'Checks', ownership: { parent_session_id: 'session' }, state: { status: 'failed' } }]);
  overrides.set('/v1/workspaces/workspace/task-notifications', () => [notice]);
  overrides.set('/v1/workspaces/workspace/task-notifications/notice/read', () => reject ? Response.json({ message: 'Acknowledgement unavailable' }, { status: 503 }) : notice);
  button('Background tasks').click();
  await eventually(() => container.querySelector('[data-task-id="task"]'), 'task list');
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => container.textContent.includes('Acknowledgement unavailable'), 'acknowledgement error');
  assert.match(container.querySelector('[aria-label="Task completion notice"]').textContent, /Task execution failed/);
  reject = false;
  button('Retry notifications').click();
  await eventually(() => !container.textContent.includes('Acknowledgement unavailable'), 'acknowledgement retry');
  assert.equal(requests.filter(item => item.path.endsWith('/task-notifications/notice/read')).length, 2);
});

test('closing task details prevents a delayed notification from being marked read', async t => {
  const { container, button, overrides, requests } = await setup(t);
  let finish;
  const pending = new Promise(resolve => { finish = resolve; });
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'task', type: 'agent', description: 'Research completed', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }]);
  overrides.set('/v1/workspaces/workspace/task-notifications', () => pending);
  button('Background tasks').click();
  await eventually(() => container.querySelector('[data-task-id="task"]'), 'task list');
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => requests.some(item => item.path.endsWith('/task-notifications')), 'notification request');
  button('Back to tasks').click();
  finish([{ notification_id: 'notice', task_id: 'task', status: 'completed', summary: 'Completed' }]);
  await delay(0);
  assert.equal(requests.some(item => item.path.endsWith('/task-notifications/notice/read')), false);
});

test('Markdown export retains native parts and protects embedded code fences', () => {
  const parts = [
    { type: 'text', data: { text: '**Visible reply**' } },
    { type: 'reasoning', data: { thinking: 'Stored reasoning' } },
    { type: 'tool_result', data: { content: '````\nresult', is_error: true } },
    { type: 'binary', data: { MIMEType: 'application/pdf', Data: 'Ynl0ZXM=', Path: '/project/report.pdf' } },
    { type: 'skill', data: { name: 'review' } },
    { type: 'finish', data: { reason: 'error', message: 'Provider failed' } },
    { type: 'future-part', data: { retained: true } },
  ];
  const text = sessionMarkdown('Original session', [{ role: 'assistant', parts }]);
  assert.ok(text.startsWith('# Original session\n'));
  assert.match(text, /\*\*Visible reply\*\*/);
  assert.match(text, /### Reasoning\n\nStored reasoning/);
  assert.ok(text.includes('`````json\n'));
  for (const part of parts.slice(2)) assert.ok(text.includes(JSON.stringify(part.data, null, 2)), part.type);
});

test('Markdown download keeps its originating title across session navigation', async t => {
  const { client, button, session, overrides } = await setup(t);
  let finish;
  const pending = new Promise(resolve => { finish = resolve; });
  overrides.set('/v1/workspaces/workspace/sessions/session/messages', () => pending);
  let blob;
  let filename;
  const createURL = URL.createObjectURL;
  const click = window.HTMLAnchorElement.prototype.click;
  URL.createObjectURL = value => { blob = value; return 'blob:export-test'; };
  window.HTMLAnchorElement.prototype.click = function () { filename = this.download; };
  t.after(() => { URL.createObjectURL = createURL; window.HTMLAnchorElement.prototype.click = click; });
  button('Command palette').click();
  button('Export as Markdown').click();
  client.set('currentSession', { ...session, id: 'other', title: 'Different session' });
  finish([{ role: 'assistant', parts: [{ type: 'text', data: { text: 'Original history' } }] }]);
  await eventually(() => blob, 'export download');
  assert.equal(filename, `${session.title.replace(/[^a-z0-9_-]+/gi, '_')}.md`);
  assert.equal(await blob.text(), `# ${session.title}\n\n## Crux\n\nOriginal history\n`);
});

test('language server diagnostics show native locations, severity and literal messages', async t => {
  const { container, button, enter, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/lsps', () => ({ gopls: { state: 'running', diagnostic_count: 1 } }));
  overrides.set('/v1/workspaces/workspace/lsps/gopls/diagnostics', () => ({
    'file:///project/main.go': [{ range: { start: { line: 3, character: 2 }, end: { line: 3, character: 8 } }, severity: 1, source: 'compiler', code: 0, message: 'Unknown <widget>', tags: [1] }],
  }));
  button('Settings').click(); button('Integrations').click();
  await eventually(() => container.textContent.includes('gopls'), 'language server');
  button('Diagnostics').click();
  await eventually(() => container.querySelector('[aria-label="gopls diagnostics"]')?.textContent.includes('Unknown <widget>'), 'diagnostic result');
  const dialog = container.querySelector('[aria-label="gopls diagnostics"]');
  assert.match(dialog.textContent, /\/project\/main.go/);
  assert.match(dialog.textContent, /4:3–4:9/);
  assert.match(dialog.textContent, /Error/);
  assert.match(dialog.textContent, /compiler · 0/);
  assert.equal(dialog.querySelector('widget'), null);
  assert.match(dialog.querySelector('details pre').textContent, /"tags"/);
  enter('[aria-label="Search diagnostics"]', 'missing-file');
  assert.match(dialog.textContent, /No matching diagnostics/);
});

for (const failure of [false, true]) test(`MCP settings refresh retains catalog failure=${failure}`, async t => {
  const { container, button, overrides, requests } = await setup(t);
  let info = { state: 'connected' };
  overrides.set('/v1/workspaces/workspace/mcp/states', () => ({ docs: info }));
  for (const kind of ['tools', 'prompts', 'resources']) {
    overrides.set(`/v1/workspaces/workspace/mcp/refresh-${kind}`, () => {
      info = failure && kind === 'prompts' ? { state: 'error', error: 'Prompt catalog unavailable' } : { state: 'connected' };
      return {};
    });
  }
  button('Settings').click(); button('Integrations').click();
  await eventually(() => container.querySelector('[aria-label="Refresh docs"]'), 'MCP server');
  button('Refresh docs').click();
  await eventually(() => container.textContent.includes(failure ? 'prompts: Prompt catalog unavailable' : 'docs refreshed.'), 'refresh result');
  assert.deepEqual(requests.filter(item => item.path.includes('/mcp/refresh-')).map(item => [item.path.split('/').at(-1), item.body]), [
    ['refresh-tools', { name: 'docs' }], ['refresh-prompts', { name: 'docs' }], ['refresh-resources', { name: 'docs' }],
  ]);
  if (failure) assert.equal(container.textContent.includes('docs refreshed.'), false);
});

test('MCP catalog refresh discovers prompts through the native server refresh operation', async t => {
  const { container, button, overrides, requests } = await setup(t);
  let refreshed = false;
  overrides.set('/v1/workspaces/workspace/mcp/states', () => ({ docs: { state: 'connected' } }));
  overrides.set('/v1/workspaces/workspace/mcp/prompts', () => refreshed ? [{ id: 'new', client_id: 'docs', prompt_id: 'new-prompt', title: 'New catalog prompt' }] : []);
  overrides.set('/v1/workspaces/workspace/mcp/refresh-prompts', () => { refreshed = true; return {}; });
  button('MCP prompts and resources').click();
  await eventually(() => !button('Refresh catalog').disabled, 'initial catalog load');
  button('Refresh catalog').click();
  await eventually(() => container.textContent.includes('New catalog prompt'), 'refreshed prompt catalog');
  assert.deepEqual(requests.find(item => item.path.endsWith('/mcp/refresh-prompts')).body, { name: 'docs' });
});

test('resetting a global tooling override restores the native provider default', async t => {
  const { client, container, button, workspace, overrides, requests } = await setup(t);
  const owner = { provider_id: 'native' };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [], instructions: { default: 'native', selection_default: 'native', profiles: { native: 'Provider tooling guidance' } } }];
  workspace.config.providers = { native: { tooling_instructions: 'crux' } };
  overrides.set('/v1/workspaces/workspace/config/provider-tooling', (body, method) => {
    assert.equal(method, 'DELETE');
    workspace.config.providers.native = {};
    return { scope: 0, owner, profile: '' };
  });
  await client.refreshWorkspace();
  button('Instructions').click();
  assert.equal(container.querySelector('[aria-label="Native tooling instructions"]').getAttribute('aria-checked'), 'false');
  button('Reset global tooling override').click();
  await eventually(() => container.querySelector('[aria-label="Native tooling instructions"]').getAttribute('aria-checked') === 'true', 'inherited native profile');
  const mutation = requests.find(item => item.path.endsWith('/config/provider-tooling'));
  assert.deepEqual(mutation.body, { scope: 0, owner });
  assert.equal(requests.some(item => item.path.endsWith('/config/provider-tooling') && item.method === 'PUT'), false);
});

test('instruction saves never apply their models to a replacement workspace', async t => {
  const { client, container, button, workspace, overrides, requests } = await setup(t);
  let finish;
  const pending = new Promise(resolve => { finish = resolve; });
  overrides.set('/v1/workspaces/workspace/config/set', () => pending);
  button('Instructions').click();
  const choice = [...container.querySelectorAll('.choice-card')].find(element => element.textContent.startsWith('Project only'));
  assert.ok(choice);
  choice.click();
  await eventually(() => requests.some(item => item.path.endsWith('/config/set')), 'instruction save request');
  client.api.workspace = { ...workspace, id: 'other' };
  client.set('workspace', client.api.workspace);
  const count = requests.length;
  finish({});
  await eventually(() => container.textContent.includes('Reopen it to apply'), 'instruction ownership error');
  assert.equal(requests.slice(count).some(item => item.path.startsWith('/v1/workspaces/other/agent')), false);
});

test('agent messages render markdown and update while streaming', async t => {
  const { client, container } = await setup(t);
  const text = '# Heading\n\n**Bold** and *italic* with [a link](https://example.com).\n\n- First\n- Second\n\n> Quoted text\n\n| Name | Value |\n| --- | --- |\n| item | 1 |\n\n```js\nconst value = 1;\n```';
  client.set('messages', 'assistant', { id: 'assistant', session_id: 'session', role: 'assistant', parts: [{ type: 'text', data: { text } }] });
  client.set('messageIDs', ['assistant']);
  await eventually(() => container.querySelector('.assistant .markdown h1'), 'formatted assistant response');
  const markdown = container.querySelector('.assistant .markdown');
  assert.equal(markdown.querySelector('h1').textContent, 'Heading');
  assert.equal(markdown.querySelector('strong').textContent, 'Bold');
  assert.equal(markdown.querySelector('em').textContent, 'italic');
  assert.equal(markdown.querySelectorAll('li').length, 2);
  assert.equal(markdown.querySelector('blockquote').textContent.trim(), 'Quoted text');
  assert.equal(markdown.querySelector('td').textContent, 'item');
  assert.match(markdown.querySelector('pre code').textContent, /const value = 1/);
  assert.equal(markdown.querySelector('button[data-copy-code]').textContent, 'Copy');
  client.set('messages', 'assistant', 'parts', [{ type: 'text', data: { text: text + '\n\n## Next section' } }]);
  await eventually(() => markdown.querySelector('h2')?.textContent === 'Next section', 'streamed markdown update');
});

test('tool results render in their tool message instead of the calling assistant message', async t => {
  const { client, container, button } = await setup(t);
  const messages = [{ id: 'call', role: 'assistant', parts: [{ type: 'tool_call', data: { id: 'tool-call', name: 'view', input: '{"file_path":"notes.md"}', finished: true } }] }];
  for (let i = 0; i < 120; i++) messages.push({ id: `filler-${i}`, role: 'user', parts: [{ type: 'text', data: { text: `Message ${i}` } }] });
  messages.push({ id: 'result', role: 'tool', parts: [{ type: 'tool_result', data: { tool_call_id: 'tool-call', name: 'view', content: 'Retained tool output' } }] });
  client.set('messages', Object.fromEntries(messages.map(message => [message.id, { ...message, session_id: 'session' }])));
  client.set('messageIDs', messages.map(message => message.id));
  client.set('messageRevision', value => value + 1);
  await eventually(() => container.querySelector('[data-message-id="result"] .tool-head'), 'standalone result in visible window');
  container.querySelector('[data-message-id="result"] .tool-head').click();
  assert.match(container.querySelector('[data-message-id="result"]').textContent, /Retained tool output/);
  button('Show 2 earlier messages').click();
  await eventually(() => container.querySelector('[data-message-id="call"] .tool-head'), 'original call after loading history');
  const result = container.querySelector('[data-message-id="result"]');
  assert.ok(result.classList.contains('tool'));
  assert.match(result.textContent, /Retained tool output/);
  assert.match(result.querySelector('.tool-summary').textContent, /notes\.md/);
  const call = container.querySelector('[data-message-id="call"]');
  assert.ok(call.querySelector('.tool-state.done'));
  call.querySelector('.tool-head').click();
  assert.doesNotMatch(call.textContent, /Retained tool output/);
  assert.equal(call.querySelector('.tool-output'), null);
});

test('rendered composer persists Queue/Steer and sends both modes while the session is busy', async t => {
  const { client, container, button, enter, requests, workspace } = await setup(t);
  button('Steer', container.querySelector('[aria-label="Delivery mode"]')).click();
  await eventually(() => client.state.deliveryMode === 'steer' && !client.state.deliverySaving, 'native steering selection');
  assert.equal(workspace.config.options.tui.delivery_mode, 'steer');
  assert.deepEqual(requests.find(item => item.path.endsWith('/config/set')).body, { scope: 0, key: 'options.tui.delivery_mode', value: 'steer' });
  enter('[aria-label="Message Crux"]', 'Redirect the current work');
  button('Steer the current turn').click();
  await eventually(() => client.state.queued.length === 1, 'steering pending item');
  assert.equal(requests.find(item => item.path.endsWith('/agent') && item.method === 'POST').body.delivery_mode, 'steer');
  assert.equal(requests.find(item => item.path.endsWith('/agent') && item.method === 'POST').body.run_id, undefined);
  assert.match(container.querySelector('[aria-label="Pending prompts"]').textContent, /Steer.*Redirect the current work/s);
  button('Queue', container.querySelector('[aria-label="Delivery mode"]')).click();
  await eventually(() => client.state.deliveryMode === 'queue' && !client.state.deliverySaving, 'native queue selection');
  enter('[aria-label="Message Crux"]', 'Do this afterwards');
  button('Queue message').click();
  await eventually(() => client.state.queued.length === 2, 'queued follow-up');
  assert.equal(requests.filter(item => item.path.endsWith('/agent') && item.method === 'POST').at(-1).body.delivery_mode, 'queue');
  button('Clear', container.querySelector('[aria-label="Pending prompts"]')).click();
  await eventually(() => !container.querySelector('[aria-label="Pending prompts"]'), 'clear pending controls');
});

test('rendered session actions rename and branch with recorded origin', async t => {
  const { client, button, enter, requests } = await setup(t);
  button('Workspace session').click();
  const input = enter('[aria-label="Session title"]', 'Renamed from GUI');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => client.state.currentSession.title === 'Renamed from GUI', 'renamed native session');
  button('Actions for Renamed from GUI').click(); button('Branch').click();
  await eventually(() => client.state.sessionID === 'branch', 'selected branch');
  assert.match(requests.find(item => item.path.endsWith('/fork')).search, /record_origin=true/);
  assert.equal(client.state.currentSession.forked_from_session_id, 'session');
});

test('late rename completion preserves another connection dialog', async t => {
  const { client, workspace, container, button, enter, overrides, requests } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/sessions/session/title', () => new Promise(resolve => { release = resolve; }));
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  button('Workspace session').click();
  const input = enter('[aria-label="Session title"]', 'Local title');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => Boolean(release), 'local rename pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await client.selectSession('session');
  button('Workspace session').click();
  enter('[aria-label="Session title"]', 'Remote draft');
  const count = requests.filter(item => item.path === '/ui/connections/remote/v1/workspaces/workspace/sessions').length;
  release({});
  await delay(0);
  assert.equal(container.querySelector('[aria-label="Session title"]')?.value, 'Remote draft');
  assert.equal(requests.filter(item => item.path === '/ui/connections/remote/v1/workspaces/workspace/sessions').length, count);
  assert.equal(button('Rename', container.querySelector('[role="dialog"]')).disabled, false);
});

test('rename failures remain visible inside the dialog without losing the draft', async t => {
  const { container, button, enter, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/sessions/session/title', () => Response.json({ message: 'Session title could not be saved' }, { status: 409 }));
  button('Workspace session').click();
  const input = enter('[aria-label="Session title"]', 'Keep this title');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => container.querySelector('[role="dialog"] [role="alert"]')?.textContent.includes('could not be saved'), 'visible rename failure');
  assert.equal(input.value, 'Keep this title');
  assert.equal(button('Rename', container.querySelector('[role="dialog"]')).disabled, false);
});

test('rendered file checkpoints, task output and shell controls call their native operations', async t => {
  const { expand, button, container, enter, requests } = await setup(t);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  expand('Files');
  await eventually(() => container.textContent.includes('src/app.ts'), 'checkpoint listing');
  button('v1').click();
  assert.match(container.querySelector('.code-preview').textContent, /const saved = true/);
  button('Background tasks').click();
  await eventually(() => container.textContent.includes('Run checks'), 'task listing');
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => container.textContent.includes('Checks passed'), 'task output');
  button('Stop', container.querySelector('[data-task-id="task"]')).click();
  await eventually(() => requests.some(item => item.path.endsWith('/tasks/task/stop')), 'task stop');
  button('Commands', container.querySelector('.shell-tabs')).click();
  const input = enter('[aria-label="Shell command"]', 'printf shell-result');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => container.querySelector('.shell-log').textContent.includes('shell-result'), 'shell output');
  assert.equal(requests.find(item => item.path.endsWith('/shell')).body.command, 'printf shell-result');
  button('Collapse shell').click(); button('Toggle shell').click();
  assert.match(container.querySelector('.shell-log').textContent, /shell-result/);
});

test('tmux picker discards old connection results and reloads the active attachment', async t => {
  const { client, workspace, container, button, overrides, requests } = await setup(t);
  let releaseLocal, localSignal;
  overrides.set('/ui/tmux-sessions', (_body, _method, init) => new Promise(resolve => {
    releaseLocal = resolve;
    localSignal = init.signal;
  }));
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/_ui/tmux-sessions', () => [{ id: '$2', name: 'Remote terminal', windows: 1, attached: 0 }]);
  button('Toggle shell').click();
  button('Attach tmux…').click();
  await eventually(() => Boolean(releaseLocal), 'local tmux listing pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await eventually(() => container.querySelector('.tmux-picker')?.textContent.includes('Remote terminal'), 'remote tmux listing');
  assert.equal(localSignal.aborted, true);
  releaseLocal([{ id: '$1', name: 'Old terminal', windows: 1, attached: 0 }]);
  await delay(0);
  assert.equal(container.querySelector('.tmux-picker').textContent.includes('Old terminal'), false);
  assert.equal(container.querySelector('.tmux-picker').textContent.includes('Remote terminal'), true);
  assert.ok(requests.some(item => item.path === '/ui/connections/remote/v1/workspaces/workspace/_ui/tmux-sessions'));
});

test('task action completion cannot refresh a replacement connection after dock disposal', async t => {
  const { client, workspace, container, button, overrides, requests } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/tasks/task/stop', () => new Promise(resolve => { release = resolve; }));
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  button('Background tasks').click();
  await eventually(() => container.querySelector('[data-task-id="task"]'), 'task card');
  container.querySelector('.task-row[data-task-id="task"]').click();
  button('Stop', container.querySelector('[data-task-id="task"]')).click();
  await eventually(() => Boolean(release), 'task stop pending');
  button('Collapse shell').click();
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await client.selectSession('session');
  const requestCount = requests.length;
  release({});
  await delay(0);
  assert.equal(requests.length, requestCount);
  assert.equal(client.state.sessionID, 'session');
});

test('steering finish banners are omitted without hiding partial output or errors', async t => {
  const { client, container } = await setup(t);
  client.set('messages', [
    { id: 'steered', session_id: 'session', role: 'assistant', parts: [{ type: 'text', data: { text: 'Partial work retained' } }, { type: 'finish', data: { reason: 'end_turn', message: 'Interrupted by steering' } }], created_at: 1, updated_at: 1 },
    { id: 'failed', session_id: 'session', role: 'assistant', parts: [{ type: 'finish', data: { reason: 'error', message: 'Provider unavailable' } }], created_at: 2, updated_at: 2 },
    { id: 'empty-steered', session_id: 'session', role: 'assistant', parts: [{ type: 'finish', data: { reason: 'end_turn', message: 'Interrupted by steering' } }], created_at: 3, updated_at: 3 },
    { id: 'empty-finished', session_id: 'session', role: 'assistant', parts: [{ type: 'finish', data: { reason: 'end_turn' } }], created_at: 4, updated_at: 4 },
  ]);
  await eventually(() => container.textContent.includes('Partial work retained'), 'partial response');
  assert.equal(container.textContent.includes('Interrupted by steering'), false);
  assert.ok(container.textContent.includes('Provider unavailable'));
  assert.equal(container.querySelector('[data-message-id="empty-steered"]'), null);
  assert.equal(container.querySelector('[data-message-id="empty-finished"]'), null);
});

test('desktop background transparency updates immediately and persists', async t => {
  const { container, button } = await setup(t);
  document.documentElement.classList.add('desktop');
  t.after(() => { document.documentElement.classList.remove('desktop'); document.documentElement.style.removeProperty('--backdrop-opacity'); });
  button('Settings').click();
  button('Behavior').click();
  const slider = container.querySelector('[aria-label="Background transparency"]');
  assert.equal(slider.value, '75');
  slider.value = '90';
  slider.dispatchEvent(new window.Event('input', { bubbles: true }));
  assert.ok(Math.abs(Number(document.documentElement.style.getPropertyValue('--backdrop-opacity')) - .1) < .000001);
  assert.equal(JSON.parse(localStorage.getItem('crux.web.backdrop-transparency')).value, 90);
});

test('task panel returns from its pop-out without recreating the panel', async t => {
  const { container, button } = await setup(t);
  const child = new Window({ url: 'http://127.0.0.1:8080' });
  const originalOpen = window.open;
  window.open = () => child;
  t.after(() => { window.open = originalOpen; child.close(); });
  const panel = container.querySelector('.shell');
  button('Pop out', panel).click();
  assert.equal(child.document.querySelector('.shell'), panel);
  assert.equal(container.querySelector('.shell'), null);
  child.dispatchEvent(new child.Event('beforeunload'));
  assert.equal(container.querySelector('.shell'), panel);
  assert.equal(container.querySelector('.shell-dock').classList.contains('collapsed'), false);
  button('Pop out', panel).click();
  assert.equal(child.document.querySelector('.shell'), panel);
  button('Dock panel', panel).click();
  assert.equal(container.querySelector('.shell'), panel);
  assert.equal(container.querySelector('.shell-dock').classList.contains('collapsed'), false);
});

test('expanded task panel supports keyboard resizing with a minimum height', async t => {
  const { container, button } = await setup(t);
  button('Expand shell').click();
  const handle = container.querySelector('[aria-label="Resize task and terminal panel"]');
  handle.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  assert.equal(container.querySelector('.shell-dock').style.getPropertyValue('--dock-height'), '220px');
});

test('task dock remains available when idle and offers shortcuts and commands', async t => {
  const { client, container, button } = await setup(t);
  await eventually(() => container.querySelector('.shell-dock.collapsed:not([hidden])'), 'running task pill');
  assert.equal(container.querySelector('.panel [data-section="tasks"]'), null);
  button('Tasks · 1', container.querySelector('.shell-tabs')).click();
  await eventually(() => container.querySelector('.shell-tasks [data-task-id="task"]'), 'bottom task view');
  assert.equal(container.querySelector('.shell-dock').classList.contains('collapsed'), false);
  button('Collapse shell').click();
  assert.equal(container.querySelector('.shell-dock').hidden, false);
  client.set('tasks', []);
  assert.equal(container.querySelector('.shell-dock').hidden, false);
  button('Help', container.querySelector('.shell-tabs')).click();
  await eventually(() => container.querySelector('.shell-help'), 'shortcut help');
  assert.ok(container.querySelector('.shell-help').textContent.includes('Shift+Enter'));
  assert.ok(container.querySelector('.shell-help-commands').textContent.includes('New session'));
  button('Collapse shell').click();
  assert.equal(container.querySelector('.shell-dock').hidden, false);
});

for (const type of ['agent', 'shell']) test(`${type} task output uses a selectable plain-text terminal viewport`, async t => {
  const { client, container, button, overrides } = await setup(t);
  const text = `**literal output**\n${'long-line '.repeat(100)}\nlast line`;
  overrides.set('/v1/workspaces/workspace/tasks/task/output', () => ({ output: text, output_truncated: false }));
  const tasks = [{ id: 'task', type, description: 'Terminal output task', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }];
  overrides.set('/v1/workspaces/workspace/tasks', () => tasks);
  client.set('tasks', tasks);
  button('Toggle shell').click();
  button('Command palette').click();
  button('Background tasks').click();
  await eventually(() => container.querySelector('.task-row[data-task-id="task"]'), 'task visible');
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => container.querySelector('[data-task-id="task"] .task-terminal .terminal-surface')?.textContent === text, 'terminal output loaded');
  const output = container.querySelector('[data-task-id="task"] .task-terminal .terminal-surface');
  assert.equal(output.closest('.task-terminal').getAttribute('aria-label'), 'Task terminal output');
  assert.ok(button('Back to tasks'));
  assert.equal(container.querySelector('.task-list'), null);
  assert.equal(output.querySelector('.markdown, strong'), null);
});

test('shell completion retains its original project across same-ID remote attachment', async t => {
  const { client, container, button, enter, overrides, requests, workspace } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/agent/sessions/session/shell', () => new Promise(resolve => { release = resolve; }));
  button('Toggle shell').click();
  const input = enter('[aria-label="Shell command"]', 'original-command');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => Boolean(release), 'original shell request pending');
  const remote = { ...workspace, path: '/remote-project', authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  await client.switchWorkspace('/remote-project', '', 'remote');
  const before = requests.length;
  release({ output: 'original-output', exit_code: 0 });
  await eventually(() => container.querySelector('.shell-log').textContent.includes('original-output'), 'original output retained');
  const entry = container.querySelector('.shell-entry-row');
  assert.equal(entry.querySelector('.muted.small-text').textContent, '/project');
  assert.equal(requests.slice(before).some(item => item.path.includes('/agent/sessions')), false);
  assert.equal(client.state.workspace.path, '/remote-project');
});

test('shell cancellation aborts the native request while closing preserves it', async t => {
  const { container, button, enter, overrides } = await setup(t);
  let signal;
  overrides.set('/v1/workspaces/workspace/agent/sessions/session/shell', (_body, _method, init) => new Promise((_resolve, reject) => {
    signal = init.signal;
    signal.addEventListener('abort', () => reject(new Error('Request aborted')), { once: true });
  }));
  button('Toggle shell').click();
  const input = enter('[aria-label="Shell command"]', 'long-running-command');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => Boolean(signal), 'native shell request started');
  button('Collapse shell').click();
  assert.equal(signal.aborted, false);
  button('Toggle shell').click();
  button('Stop command').click();
  await eventually(() => container.querySelector('.shell-log')?.textContent.includes('Cancellation requested.'), 'cancellation result visible');
  assert.equal(signal.aborted, true);
  assert.match(container.querySelector('.shell-entry-row').textContent, /\/project/);
  assert.equal(container.querySelector('[aria-label="Shell command"]').disabled, false);
});

test('project selection errors remain visible and preserve the active project', async t => {
  const { client, container, button, overrides } = await setup(t);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  const projects = [{ slug: 'current', name: 'Current project', selected: true, completed: 1, total: 2 }, { slug: 'next', name: 'Next project', selected: false, completed: 0, total: 1 }];
  overrides.set('/v1/workspaces/workspace/projects', () => projects);
  overrides.set('/v1/workspaces/workspace/projects/selection', () => Response.json({ message: 'Project selection failed' }, { status: 409 }));
  client.set('activityRevision', value => value + 1);
  await eventually(() => container.querySelector('[aria-label="Active project"]')?.value === 'current', 'active project loaded');
  const select = container.querySelector('[aria-label="Active project"]');
  select.value = 'next';
  select.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => container.textContent.includes('Project selection failed'), 'visible project selection failure');
  assert.equal(select.value, 'current');
  assert.equal(select.disabled, false);
});

test('project selection retains completed entries without offering activation', async t => {
  const { client, container, button, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/projects', () => [
    { slug: 'active', name: 'Current project', status: 'active', selected: true, completed: 1, total: 2 },
    { slug: 'paused', name: 'Paused project', status: 'paused', selected: false, completed: 1, total: 2 },
    { slug: 'finished', name: 'Finished project', status: 'completed', selected: false, completed: 2, total: 2 },
  ]);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  client.set('activityRevision', value => value + 1);
  await eventually(() => container.querySelector('[aria-label="Active project"]')?.value === 'active', 'project statuses');
  const options = [...container.querySelector('[aria-label="Active project"]').options];
  assert.equal(options.find(option => option.value === 'finished').disabled, true);
  assert.match(options.find(option => option.value === 'finished').textContent, /completed/);
  assert.equal(options.find(option => option.value === 'paused').disabled, false);
  assert.equal(options.find(option => option.value === '').disabled, false);
});

test('path privacy restores inheritance with the native revision and no overrides', async t => {
  const { container, button, overrides, requests } = await setup(t);
  const policy = { scope: { kind: 'session', session_id: 'session' }, revision: 7, settings: { enabled: true, censor_images: true, targets: [{ path: '/project', recursive: true, censor_images: true }] } };
  overrides.set('/v1/workspaces/workspace/path-translation', (body, method) => method === 'POST' ? { ...body, revision: 8 } : policy);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  button('Path privacy').click();
  await eventually(() => !button('Use inherited settings').disabled, 'privacy policy loaded');
  button('Use inherited settings').click();
  button('Save', container.querySelector('[role="dialog"]')).click();
  await eventually(() => requests.some(item => item.path.endsWith('/path-translation') && item.method === 'POST'), 'privacy reset submitted');
  const submitted = requests.find(item => item.path.endsWith('/path-translation') && item.method === 'POST').body;
  assert.deepEqual(submitted, { scope: policy.scope, revision: 7, settings: { inherit: true, enabled: false, targets: [] } });
});

test('file inspector clears the previous session when opening a blank conversation', async t => {
  const { expand, client, container, button } = await setup(t);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  expand('Files');
  await eventually(() => container.querySelector('.file-path')?.textContent.includes('src/app.ts'), 'session files loaded');
  await client.selectSession('');
  await eventually(() => !container.querySelector('.file-path'), 'previous session files removed');
  assert.match(container.querySelector('[aria-label="Session details"]').textContent, /No session selected/);
});

test('integrations reconnect refreshes missed state and ignores pre-disconnect reads', async t => {
  const { client, container, button, overrides, requests } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/skills/catalog', () => new Promise(resolve => { release = resolve; }));
  button('Settings').click();
  button('Integrations').click();
  await eventually(() => Boolean(release), 'old integrations read pending');
  client.set('connected', false);
  const catalogReads = () => requests.filter(item => item.path.endsWith('/skills/catalog')).length;
  const disconnectedReads = catalogReads();
  client.set('servicesRevision', value => value + 1);
  await delay(0);
  assert.equal(catalogReads(), disconnectedReads);
  overrides.set('/v1/workspaces/workspace/skills/catalog', () => ({ skills: [{ id: 'recovered', name: 'Recovered skill', source: 'project', enabled: true }], states: [] }));
  client.set('connected', true);
  await eventually(() => container.textContent.includes('Recovered skill'), 'integrations recovered');
  release({ skills: [{ id: 'obsolete', name: 'Obsolete skill', source: 'project', enabled: true }], states: [] });
  await delay(0);
  assert.equal(container.textContent.includes('Obsolete skill'), false);
  assert.equal(container.textContent.includes('Recovered skill'), true);
});

test('skill catalog reload displays disabled skills and discovery failures', async t => {
  const { client, container, button, overrides, requests } = await setup(t);
  let reloaded = false;
  overrides.set('/v1/workspaces/workspace/skills/catalog', body => {
    reloaded ||= body.force_reload;
    return {
      skills: [{ id: '/project/skills/review/SKILL.md', name: 'Workspace review', description: 'Review changes', source: 'project', user_invocable: true, enabled: false }],
      states: reloaded ? [{ name: 'Broken skill', path: '/project/skills/broken/SKILL.md', state: 1, error: 'Missing description' }] : [],
    };
  });
  button('Settings').click(); button('Integrations').click();
  await eventually(() => container.querySelector('.skill')?.textContent.includes('Workspace review'), 'full skill catalog');
  assert.match(container.querySelector('.skill').textContent, /Disabled/);
  const revision = client.state.skillsRevision;
  button('Reload skills').click();
  await eventually(() => container.textContent.includes('Missing description'), 'discovery diagnostics');
  assert.match(container.textContent, /\/project\/skills\/broken\/SKILL.md/);
  assert.equal(client.state.skillsRevision, revision + 1);
  assert.ok(requests.some(item => item.path.endsWith('/skills/catalog') && item.method === 'POST' && item.body.force_reload === true));
});

test('session creation refresh cannot select a session on a replacement connection', async t => {
  const { client, workspace, overrides, requests } = await setup(t);
  let release;
  const created = { id: 'original-session', title: 'Original session', mode: 'default' };
  overrides.set('/v1/workspaces/workspace/sessions', (_body, method) => method === 'POST'
    ? created : new Promise(resolve => { release = resolve; }));
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  const creation = client.newSession();
  await eventually(() => Boolean(release), 'original session refresh pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await client.selectSession('session');
  const requestCount = requests.length;
  release([created]);
  assert.equal(await creation, 'original-session');
  assert.equal(client.state.sessionID, 'session');
  assert.equal(requests.length, requestCount);
});

test('message submission completion cannot refresh or unlock a replacement connection', async t => {
  const { client, workspace, overrides, requests } = await setup(t);
  let releaseLocal, releaseRemote;
  const agent = { is_ready: true, model_cfg: { provider: 'native', model: 'model' } };
  overrides.set('/v1/workspaces/workspace/agent', (_body, method) => method === 'POST'
    ? new Promise(resolve => { releaseLocal = resolve; }) : agent);
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/agent', (_body, method) => method === 'POST'
    ? new Promise(resolve => { releaseRemote = resolve; }) : agent);
  const localSend = client.send('Local message');
  await eventually(() => Boolean(releaseLocal), 'local message pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  assert.equal(client.state.sending, false);
  await client.selectSession('session');
  const remoteSend = client.send('Remote message');
  await eventually(() => Boolean(releaseRemote), 'remote message pending');
  const requestCount = requests.length;
  releaseLocal({});
  await localSend;
  assert.equal(client.state.sending, true);
  assert.equal(requests.length, requestCount);
  releaseRemote({});
  await remoteSend;
  assert.equal(client.state.sending, false);
});

test('skill reload completion cannot update a replacement connection with the same workspace ID', async t => {
  const { client, workspace, container, button, overrides } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/skills/catalog', body => body.force_reload
    ? new Promise(resolve => { release = resolve; })
    : { skills: [], states: [] });
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  button('Settings').click(); button('Integrations').click();
  button('Reload skills').click();
  await eventually(() => Boolean(release), 'skill reload pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  const revision = client.state.skillsRevision;
  release({ skills: [{ id: 'old-skill', name: 'Old attachment skill', source: 'project' }], states: [] });
  await delay(0);
  assert.equal(client.state.skillsRevision, revision);
  assert.equal(container.textContent.includes('Old attachment skill'), false);
});

test('index saves remain bound to their connection across identical workspace IDs', async t => {
  const { client, workspace, container, button, overrides } = await setup(t);
  let releaseLocal, releaseRemote;
  const localStatus = { enabled: true, state: 'ready', configured_database_path: '/local/index.db' };
  const remoteStatus = { enabled: true, state: 'ready', configured_database_path: '/remote/index.db' };
  overrides.set('/v1/workspaces/workspace/codebase-index', (_body, method) => method === 'POST'
    ? new Promise(resolve => { releaseLocal = resolve; }) : localStatus);
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/codebase-index', (_body, method) => method === 'POST'
    ? new Promise(resolve => { releaseRemote = resolve; }) : remoteStatus);
  button('Settings').click(); button('Codebase index').click();
  const database = () => container.querySelector('[aria-label="Index database path"]');
  await eventually(() => database()?.value === '/local/index.db', 'local index loaded');
  button('Save settings').click();
  await eventually(() => Boolean(releaseLocal), 'local save pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await eventually(() => database()?.value === '/remote/index.db', 'remote index loaded');
  button('Save settings').click();
  await eventually(() => Boolean(releaseRemote), 'remote save pending');
  releaseLocal(localStatus);
  await delay(0);
  assert.equal(database().value, '/remote/index.db');
  assert.equal(button('Save settings').disabled, true);
  releaseRemote(remoteStatus);
  await eventually(() => !button('Save settings').disabled, 'remote save completed');
  assert.equal(database().value, '/remote/index.db');
});

test('failed skill lookup stays visible and retries on the next command search', async t => {
  const { container, enter, overrides } = await setup(t);
  let calls = 0;
  overrides.set('/v1/workspaces/workspace/skills', () => ++calls === 1
    ? Response.json({ message: 'Skills unavailable' }, { status: 503 })
    : [{ id: 'builtin:review', name: 'review', description: 'Review code', user_invocable: true, source: 'builtin' }]);
  enter('[aria-label="Message Crux"]', '/review');
  await eventually(() => container.textContent.includes('Skills unavailable'), 'skill lookup error');
  enter('[aria-label="Message Crux"]', '/revie');
  await eventually(() => container.querySelector('.suggestions')?.textContent.includes('/review'), 'skill lookup recovery');
  assert.equal(calls, 2);
});

test('file references and native skill inputs survive composition and submission', async t => {
  const { client, container, button, enter, requests, overrides } = await setup(t);
  const attachment = { file_path: '/project/src/app.ts', file_name: 'app.ts', mime_type: 'text/plain; charset=utf-8', content: 'Y29uc3QgdmFsdWUgPSAxOw==' };
  overrides.set('/ui/workspaces/workspace/files/attachment', () => attachment);
  enter('[aria-label="Message Crux"]', '/review');
  await eventually(() => container.querySelector('.suggestions')?.textContent.includes('/review'), 'native skill list');
  container.querySelector('.suggestions button').click();
  enter('[aria-label="Message Crux"]', '@app');
  await eventually(() => container.querySelector('.suggestions')?.textContent.includes('app.ts'), 'file search');
  container.querySelector('.suggestions button').click();
  await eventually(() => container.querySelector('[aria-label="Message Crux"]').value === '@/project/src/app.ts ', 'workspace file attached');
  button('Queue message').click();
  await eventually(() => client.state.queued.length === 1, 'skill submission');
  const submitted = requests.find(item => item.path.endsWith('/agent') && item.method === 'POST').body;
  assert.deepEqual(submitted.input_parts, [{ type: 'text', text: '@/project/src/app.ts ' }, { type: 'skill', text: 'review', path: 'builtin:review' }]);
  assert.deepEqual(submitted.attachments, [attachment]);
});

test('pending file attachments follow a blank draft into its new plan session', async t => {
  const { client, container, button, enter, overrides, requests } = await setup(t);
  await client.selectSession('');
  const attachment = { file_path: '/project/src/app.ts', file_name: 'app.ts', mime_type: 'text/plain', content: 'Y29uc3Q=' };
  let release;
  overrides.set('/ui/workspaces/workspace/files/attachment', () => new Promise(resolve => { release = resolve; }));
  enter('[aria-label="Message Crux"]', '@app');
  await eventually(() => container.querySelector('.suggestions')?.textContent.includes('app.ts'), 'file search');
  container.querySelector('.suggestions button').click();
  await eventually(() => Boolean(release), 'pending attachment request');
  const mode = container.querySelector('[aria-label="Interaction mode"]');
  mode.value = 'plan'; mode.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => client.state.currentSession?.mode === 'plan', 'new plan session');
  release(attachment);
  await eventually(() => container.querySelector('[aria-label="Message Crux"]').value === '@/project/src/app.ts ', 'attachment follows migrated draft');
  button('Send message').click();
  await eventually(() => requests.some(item => item.path.endsWith('/agent') && item.method === 'POST'), 'attached draft submitted');
  const submitted = requests.find(item => item.path.endsWith('/agent') && item.method === 'POST').body;
  assert.deepEqual(submitted.attachments, [attachment]);
});

test('custom commands expand named arguments into an editable draft without submitting', async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  overrides.set('/ui/workspaces/workspace/commands', () => [{ id: 'project:review', name: 'project:review', content: 'Review $FILE with $FILE_TYPE. Keep $FILE unchanged.', arguments: [{ id: 'FILE', title: 'FILE', required: true }, { id: 'FILE_TYPE', title: 'FILE_TYPE', required: true }] }]);
  enter('[aria-label="Message Crux"]', '/project:review');
  await eventually(() => container.querySelector('.suggestions')?.textContent.includes('/project:review'), 'native custom command catalog');
  container.querySelector('.suggestions button').click();
  enter('[aria-label="Command argument FILE"]', '$literal.ts');
  enter('[aria-label="Command argument FILE_TYPE"]', 'TypeScript');
  button('Insert command').click();
  assert.equal(container.querySelector('[aria-label="Message Crux"]').value, 'Review $literal.ts with TypeScript. Keep $literal.ts unchanged.');
  assert.equal(requests.some(item => item.path.endsWith('/agent') && item.method === 'POST'), false);
});

test('invalid delivery preference is visible and never changes the selected mode', async t => {
  const { client, container, button, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/config/set', () => Response.json({ message: 'Native preference could not be saved' }, { status: 400 }));
  button('Steer', container.querySelector('[aria-label="Delivery mode"]')).click();
  await eventually(() => container.querySelector('[role="alert"]')?.textContent.includes('could not be saved'), 'visible native error');
  assert.equal(client.state.deliveryMode, 'queue');
  assert.equal(button('Queue', container.querySelector('[aria-label="Delivery mode"]')).getAttribute('aria-checked'), 'true');
});

test('agent creation submits the written instructions literally', async t => {
  const { client, workspace, button, enter, overrides, requests } = await setup(t);
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', available: true, models: [{ id: 'model', name: 'Model' }] }];
  await client.refreshWorkspace();
  overrides.set('/v1/workspaces/workspace/agent/definitions', () => ({ path: '/project/.ai-cli/agents/reviewer.md' }));
  button('Settings').click(); button('Agents').click();
  enter('[aria-label="Agent name"]', 'reviewer');
  enter('[aria-label="Agent description"]', 'Review changes');
  const instructions = '# Instructions\n\nReview $INPUT and {{literal}}.\n';
  enter('[aria-label="Agent instructions"]', instructions);
  enter('[aria-label="Agent tools"]', 'view, search');
  button('Create definition').click();
  await eventually(() => requests.some(item => item.path.endsWith('/agent/definitions')), 'definition submitted');
  assert.deepEqual(requests.find(item => item.path.endsWith('/agent/definitions')).body, { scope: 'project', name: 'reviewer', description: 'Review changes', instructions, model: 'native/model', tools: ['view', 'search'] });
});

test('agent creation preserves edits made while the native save is pending', async t => {
  const { client, container, button, enter, overrides, workspace } = await setup(t);
  workspace.provider_surfaces = [{ id: 'native', name: 'Native', available: true, models: [{ id: 'model', name: 'Model' }] }];
  await client.refreshWorkspace();
  let finish;
  overrides.set('/v1/workspaces/workspace/agent/definitions', () => new Promise(resolve => { finish = resolve; }));
  button('Settings').click(); button('Agents').click();
  enter('[aria-label="Agent name"]', 'reviewer');
  enter('[aria-label="Agent description"]', 'Review changes');
  enter('[aria-label="Agent instructions"]', 'Review the changes.');
  button('Create definition').click();
  await eventually(() => finish, 'native agent save');
  enter('[aria-label="Agent instructions"]', 'Review the next change too.');
  finish({ path: '/project/.ai-cli/agents/reviewer.md' });
  await eventually(() => !button('Create definition').disabled, 'agent save completed');
  assert.equal(container.querySelector('[aria-label="Agent instructions"]').value, 'Review the next change too.');
  assert.equal(container.querySelector('[aria-label="Agent name"]').value, 'reviewer');
});

test('agent creation completion stays with its original remote attachment', async t => {
  const { client, container, button, enter, overrides, workspace } = await setup(t);
  workspace.provider_surfaces = [{ id: 'native', name: 'Native', available: true, models: [{ id: 'model', name: 'Model' }] }];
  await client.refreshWorkspace();
  let finish;
  overrides.set('/v1/workspaces/workspace/agent/definitions', () => new Promise(resolve => { finish = resolve; }));
  button('Settings').click(); button('Agents').click();
  enter('[aria-label="Agent name"]', 'reviewer');
  enter('[aria-label="Agent description"]', 'Review changes');
  enter('[aria-label="Agent instructions"]', 'Review the changes.');
  button('Create definition').click();
  await eventually(() => finish, 'original attachment save');
  client.api.connection = 'other-server';
  client.set('workspace', 'connection_name', 'other-server');
  await eventually(() => !button('Create definition').disabled, 'new attachment available');
  let finishCurrent;
  overrides.set('/ui/connections/other-server/v1/workspaces/workspace/agent/definitions', () => new Promise(resolve => { finishCurrent = resolve; }));
  button('Create definition').click();
  await eventually(() => finishCurrent, 'current attachment save');
  finish({ path: '/original/.ai-cli/agents/reviewer.md' });
  await delay(0);
  assert.equal(button('Create definition').disabled, true);
  assert.equal(container.querySelector('[aria-label="Agent name"]').value, 'reviewer');
  assert.equal(client.state.toasts.some(item => item.text.includes('/original/')), false);
  finishCurrent({ path: '/current/.ai-cli/agents/reviewer.md' });
  await eventually(() => !button('Create definition').disabled, 'current attachment save completed');
  assert.equal(container.querySelector('[aria-label="Agent name"]').value, '');
});

test('MCP setup preserves HTTP headers, OAuth options and tool selection', async t => {
  const { container, button, enter, requests, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/config/set', () => ({}));
  button('Settings').click();
  button('Integrations').click();
  button('Add server').click();
  const dialog = [...container.querySelectorAll('[role="dialog"]')].at(-1);
  enter('[aria-label="Server name"]', 'docs');
  button('HTTP', dialog).click();
  enter('[aria-label="Server URL"]', 'https://example.com/mcp');
  enter('[aria-label="Server headers"]', '{"Authorization":"Bearer $MCP_TOKEN","X-Label":" exact value "}');
  const oauth = [...dialog.querySelectorAll('label')].find(label => label.textContent.includes('Enable OAuth sign-in')).querySelector('input');
  oauth.checked = true; oauth.dispatchEvent(new window.Event('change', { bubbles: true }));
  enter('[aria-label="OAuth client ID"]', 'client-id');
  enter('[aria-label="OAuth client secret"]', '$CLIENT_SECRET');
  enter('[aria-label="OAuth callback port"]', '8123');
  enter('[aria-label="Server timeout"]', '45');
  enter('[aria-label="Enabled server tools"]', 'read\nsearch');
  enter('[aria-label="Disabled server tools"]', 'write');
  button('Add server', dialog).click();
  await eventually(() => requests.some(item => item.body?.key === 'mcp.docs'), 'native MCP config saved');
  const saved = requests.find(item => item.body?.key === 'mcp.docs').body;
  assert.deepEqual(saved, { scope: 1, key: 'mcp.docs', value: { type: 'http', url: 'https://example.com/mcp', headers: { Authorization: 'Bearer $MCP_TOKEN', 'X-Label': ' exact value ' }, timeout: 45, disabled: false, enabled_tools: ['read', 'search'], disabled_tools: ['write'], oauth: true, oauth_client_id: 'client-id', oauth_client_secret: '$CLIENT_SECRET', oauth_callback_port: 8123 } });
});

test('MCP setup keeps a pending save bound to its original workspace', async t => {
  const { client, container, button, enter, requests, overrides, workspace } = await setup(t);
  let finish;
  overrides.set('/v1/workspaces/workspace/config/set', () => new Promise(resolve => { finish = resolve; }));
  button('Settings').click(); button('Integrations').click(); button('Add server').click();
  const dialog = [...container.querySelectorAll('[role="dialog"]')].at(-1);
  enter('[aria-label="Server name"]', 'docs'); enter('[aria-label="Server command"]', 'mcp-docs');
  button('Add server', dialog).click();
  await eventually(() => finish, 'pending MCP save');
  const next = { ...workspace, id: 'other-workspace', path: '/other' };
  client.api.workspace = next;
  client.set('workspace', next);
  await eventually(() => !container.querySelector('[aria-label="Server name"]'), 'old workspace form closed');
  button('Add server').click();
  finish({});
  await delay(20);
  assert.ok(container.querySelector('[aria-label="Server name"]'));
  assert.equal(container.textContent.includes('MCP server docs added to this workspace.'), false);
  assert.equal(requests.filter(item => item.body?.key === 'mcp.docs').length, 1);
  assert.equal(requests.find(item => item.body?.key === 'mcp.docs').path, '/v1/workspaces/workspace/config/set');
});

test('MCP setup rejects invalid headers without writing configuration', async t => {
  const { container, button, enter, requests } = await setup(t);
  button('Settings').click(); button('Integrations').click(); button('Add server').click();
  const dialog = [...container.querySelectorAll('[role="dialog"]')].at(-1);
  enter('[aria-label="Server name"]', 'docs'); button('HTTP', dialog).click();
  enter('[aria-label="Server URL"]', 'https://example.com/mcp');
  enter('[aria-label="Server headers"]', '{"Authorization":123}');
  button('Add server', dialog).click();
  await eventually(() => dialog.textContent.includes('Headers must be a JSON object with string values.'), 'invalid headers visible');
  assert.equal(requests.some(item => item.body?.key === 'mcp.docs'), false);
});

test('native usage events update the sidebar without renderer quota requests', async t => {
  const { client, container, button, requests, emit } = await setup(t);
  const owner = { provider_id: 'native' };
  client.set('workspace', 'provider_surfaces', [{ id: 'native', name: 'Native', available: true, usage_available: true, owner, models: [] }]);
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  emit({ type: 'agent_event', payload: { type: 'updated', payload: { type: 'usage', provider_id: 'native', owner, usage: { revision: 0, digest: '', usage: { provider_id: 'native', windows: [{ name: 'Weekly', percent: 30 }] } } } } });
  await eventually(() => container.querySelector('.usage')?.textContent.includes('70% remaining'), 'native quota displayed');
  assert.equal(container.querySelector('.usage .meter span').style.width, '70%');
  emit({ type: 'agent_event', payload: { type: 'updated', payload: { type: 'usage', provider_id: 'other', owner: { provider_id: 'other' }, usage: { revision: 0, digest: '', usage: { provider_id: 'other', windows: [{ name: 'Weekly', percent: 90 }] } } } } });
  await eventually(() => client.state.providerUsage.other, 'second provider quota retained');
  assert.equal(container.querySelector('.usage .meter span').style.width, '70%');
  emit({ type: 'agent_event', payload: { type: 'updated', payload: { type: 'usage', provider_id: 'native', owner, usage: { revision: 0, digest: '', usage: null } } } });
  await eventually(() => !container.querySelector('.usage .meter'), 'old quota removed');
  assert.equal(requests.some(item => item.path.endsWith('/providers/usage')), false);
});

test('provider usage consumes native snapshots without requests and rejects stale ownership', () => {
  const owner = { provider_id: 'native' };
  const usage = { provider_id: 'native', plan: 'Current account', windows: [{ name: 'Weekly', percent: 30 }] };
  const state = { connected: true, workspace: { id: 'workspace', authority: { mode: 'client', revision: 1, digest: 'native' } }, providerUsage: { native: { connection: 'remote', workspaceID: 'workspace', owner, revision: 1, digest: 'native', usage } } };
  const client = { state, api: { connection: 'remote', request: () => assert.fail('usage must arrive through native events') } };
  assert.equal(providerUsage(client, owner), usage);
  assert.equal(providerUsage(client, { provider_id: 'other' }), undefined);
  state.workspace.authority.revision = 2;
  assert.equal(providerUsage(client, owner), undefined);
  state.workspace.authority.revision = 1;
  client.api.connection = 'other';
  assert.equal(providerUsage(client, owner), undefined);
  client.api.connection = 'remote';
  state.connected = false;
  assert.equal(providerUsage(client, owner), undefined);
  state.connected = true;
  state.workspace.id = 'other-workspace';
  assert.equal(providerUsage(client, owner), undefined);
});

const repairSummary = (request, extra = {}) => ({ workspace_id: request.operation_workspace_id, operation_id: request.operation_id, provider_id: 'native', action: 'switch', revision: 7, original: { accounts_saved: true, config_saved: false, runtime_published: false }, repair_ready: true, abandoned: false, needs_reload: false, ...extra });

for (const action of ['apply', 'abandon']) test(`authentication storage ${action} uses the exact reviewed identity and revision`, async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  overrides.set('/v1/workspaces/workspace/auth/local-repair', body => ({ request: body, result: { summary: repairSummary(body, { revision: body.apply || body.abandon ? 8 : 7, abandoned: body.abandon, needs_reload: body.apply }), needs_reload: body.apply, abandoned: body.abandon, accounts_written: body.apply } }));
  button('Settings').click(); button('Providers').click();
  enter('[aria-label="Authentication operation workspace"]', 'original-workspace');
  enter('[aria-label="Authentication operation ID"]', 'original-operation');
  assert.equal(button('Repair saved authentication').disabled, true);
  button('Review authentication operation').click();
  await eventually(() => !button('Repair saved authentication').disabled, 'native repair review');
  button(action === 'apply' ? 'Repair saved authentication' : 'Abandon storage recovery').click();
  await eventually(() => container.textContent.includes(action === 'apply' ? 'Storage repair completed.' : 'Recovery intent abandoned.'), 'native storage receipt');
  const sent = requests.filter(item => item.path.endsWith('/auth/local-repair')).map(item => item.body);
  assert.deepEqual(sent, [
    { workspace_id: 'workspace', operation_workspace_id: 'original-workspace', operation_id: 'original-operation', revision: 0, apply: false, abandon: false },
    { workspace_id: 'workspace', operation_workspace_id: 'original-workspace', operation_id: 'original-operation', revision: 7, apply: action === 'apply', abandon: action === 'abandon' },
  ]);
  assert.equal(button('Repair saved authentication').disabled, true);
  assert.equal(requests.some(item => item.path.endsWith('/agent/update') || item.path.endsWith('/auth/switch')), false);
});

test('authentication storage retry retains the original apply revision and partial progress', async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  let attempts = 0;
  overrides.set('/v1/workspaces/workspace/auth/local-repair', body => {
    if (!body.apply) return { request: body, result: { summary: repairSummary(body) } };
    attempts++;
    if (attempts === 1) return Response.json({ request: body, result: { summary: repairSummary(body, { revision: 8, repair_accounts_written: true }), accounts_written: true }, error: { code: 'failed', message: 'Storage write incomplete' } }, { status: 422 });
    return { request: body, result: { summary: repairSummary(body, { revision: 9, needs_reload: true }), needs_reload: true, accounts_matched: true, config_written: true } };
  });
  button('Settings').click(); button('Providers').click();
  enter('[aria-label="Authentication operation ID"]', 'operation');
  button('Review authentication operation').click();
  await eventually(() => !button('Repair saved authentication').disabled, 'repair reviewed');
  button('Repair saved authentication').click();
  await eventually(() => container.textContent.includes('Storage write incomplete'), 'partial repair visible');
  assert.ok(container.textContent.includes('"accounts_written": true'));
  button('Retry storage repair').click();
  await eventually(() => container.textContent.includes('Storage repair completed.'), 'exact repair retried');
  const mutations = requests.filter(item => item.path.endsWith('/auth/local-repair') && item.body.apply);
  assert.equal(mutations.length, 2);
  assert.deepEqual(mutations[1].body, mutations[0].body);
  assert.equal(mutations[1].body.revision, 7);
});

test('authentication repair rejects a different operation and clears review when inputs change', async t => {
  const { container, button, enter, overrides } = await setup(t);
  let mismatch = true;
  overrides.set('/v1/workspaces/workspace/auth/local-repair', body => ({ request: body, result: { summary: repairSummary(body, mismatch ? { operation_id: 'other' } : {}) } }));
  button('Settings').click(); button('Providers').click();
  enter('[aria-label="Authentication operation ID"]', 'operation');
  button('Review authentication operation').click();
  await eventually(() => container.textContent.includes('different operation or revision'), 'mismatched repair rejected');
  assert.equal(button('Repair saved authentication').disabled, true);
  mismatch = false;
  button('Review authentication operation').click();
  await eventually(() => !button('Repair saved authentication').disabled, 'matching repair reviewed');
  enter('[aria-label="Authentication operation ID"]', 'new-operation');
  assert.equal(button('Repair saved authentication').disabled, true);
  assert.equal(button('Abandon storage recovery').disabled, true);
});

test('authentication repair ignores a delayed review after leaving provider settings', async t => {
  const { container, button, enter, overrides, requests } = await setup(t);
  let finishReview;
  overrides.set('/v1/workspaces/workspace/auth/local-repair', body => new Promise(resolve => { finishReview = () => resolve({ request: body, result: { summary: repairSummary(body) } }); }));
  button('Settings').click(); button('Providers').click();
  enter('[aria-label="Authentication operation ID"]', 'old-operation');
  button('Review authentication operation').click();
  await eventually(() => finishReview, 'repair review pending');
  button('Behavior').click(); button('Providers').click();
  finishReview();
  await delay(0);
  assert.equal(container.querySelector('[aria-label="Authentication operation ID"]').value, '');
  assert.equal(button('Repair saved authentication').disabled, true);
  assert.equal(button('Abandon storage recovery').disabled, true);
  assert.equal(container.textContent.includes('Reviewed saved progress.'), false);
  assert.equal(requests.filter(item => item.path.endsWith('/auth/local-repair')).length, 1);
});

for (const scope of ['0', '1']) test(`provider token maintenance retains the exact native owner and scope ${scope}`, async t => {
  const { client, container, button, workspace, overrides, requests } = await setup(t);
  const owner = { provider_id: 'native', has_oauth: true, definition_digest: 'retained-definition', oauth_binding_digest: 'retained-binding' };
  const target = { workspace_id: 'workspace', owner, generation: { epoch: 'epoch', sequence: 1 } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', generation: target.generation, providers: [{ owner, configured: true, credentials: [], credential_slots: [] }] }));
  overrides.set('/v1/workspaces/workspace/auth/accounts', () => ({ target, accounts: [] }));
  let finish;
  overrides.set('/v1/workspaces/workspace/config/refresh-oauth', () => new Promise(resolve => { finish = () => resolve({}); }));
  await client.refreshWorkspace();
  button('Settings').click(); button('Providers').click();
  await eventually(() => container.querySelector('.provider button:not(:disabled)'), 'provider loaded');
  button('Manage').click();
  await eventually(() => !button('Refresh OAuth token').disabled, 'accounts loaded');
  const select = container.querySelector('[aria-label="Token refresh scope"]');
  select.closest('details').open = true;
  select.value = scope; select.dispatchEvent(new window.Event('change', { bubbles: true }));
  button('Refresh OAuth token').click(); button('Refresh OAuth token').click();
  await eventually(() => finish, 'native refresh started');
  assert.deepEqual(requests.filter(item => item.path.endsWith('/config/refresh-oauth')).map(item => item.body), [{ scope: Number(scope), owner }]);
  assert.equal(select.disabled, true);
  finish();
  await eventually(() => !button('Refresh OAuth token').disabled, 'native refresh and status completed');
  assert.match(container.textContent, /OAuth token refreshed and saved/);
  assert.equal(requests.some(item => item.path.endsWith('/auth/oauth/begin')), false);
});

for (const outcome of ['rejected', 'view-failed', 'navigated']) test(`token refresh ${outcome} does not report an unrelated account success`, async t => {
  const { client, container, button, workspace, overrides, requests } = await setup(t);
  const owner = { provider_id: 'native', has_oauth: true };
  const target = { workspace_id: 'workspace', owner, generation: { epoch: 'epoch', sequence: 1 } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', generation: target.generation, providers: [{ owner, configured: true, credentials: [], credential_slots: [] }] }));
  overrides.set('/v1/workspaces/workspace/auth/accounts', () => ({ target, accounts: [] }));
  let finish;
  overrides.set('/v1/workspaces/workspace/config/refresh-oauth', () => new Promise(resolve => { finish = () => resolve(outcome === 'rejected' ? Response.json({ message: 'OAuth refresh not supported for this provider' }, { status: 400 }) : {}); }));
  await client.refreshWorkspace();
  button('Settings').click(); button('Providers').click();
  await eventually(() => container.querySelector('.provider button:not(:disabled)'), 'provider loaded');
  button('Manage').click();
  await eventually(() => !button('Refresh OAuth token').disabled, 'accounts loaded');
  button('Refresh OAuth token').click();
  await eventually(() => finish, 'native refresh pending');
  if (outcome === 'view-failed') overrides.set('/v1/workspaces/workspace', () => Response.json({ message: 'Workspace status unavailable' }, { status: 503 }));
  if (outcome === 'navigated') {
    client.api.workspace = { ...workspace, id: 'other' };
    client.set('workspace', client.api.workspace);
  }
  const count = requests.length;
  finish();
  if (outcome === 'navigated') {
    await delay(0);
    assert.equal(requests.slice(count).some(item => item.path.endsWith('/auth/accounts') || item.path.endsWith('/config/refresh-oauth')), false);
    assert.equal(container.textContent.includes('OAuth token refreshed and saved'), false);
  } else {
    await eventually(() => container.textContent.includes(outcome === 'rejected' ? 'OAuth refresh not supported' : 'Token refreshed, but the account view could not be refreshed'), 'native outcome visible');
    assert.equal(container.textContent.includes('OAuth token refreshed and saved'), outcome === 'view-failed');
  }
});

test('MCP servers awaiting native OAuth offer sign-in without a configured OAuth flag', async t => {
  const { container, button, workspace, overrides, requests } = await setup(t);
  workspace.config.mcp = { remote: { type: 'http', url: 'https://example.test/mcp' } };
  overrides.set('/v1/workspaces/workspace/mcp/states', () => ({ remote: { state: 'needs_auth' } }));
  overrides.set('/v1/workspaces/workspace/mcp/pending-auth', () => [{ name: 'remote', url: 'https://example.test/mcp' }]);
  overrides.set('/v1/workspaces/workspace/mcp/auth-url', () => ({ auth_url: 'https://example.test/authorize' }));
  overrides.set('/v1/workspaces/workspace/mcp/auth', () => new Promise(() => {}));
  button('Settings').click(); button('Integrations').click();
  await eventually(() => container.querySelector('[aria-label="Sign in to remote"]'), 'native pending sign-in');
  assert.equal(button('Sign in to remote').textContent.trim(), 'Sign in required');
  button('Sign in to remote').click();
  await eventually(() => container.querySelector('.callout a[href="https://example.test/authorize"]'), 'native authorization URL');
  assert.deepEqual(requests.find(item => item.path.endsWith('/mcp/auth')).body, { name: 'remote' });
  button('Cancel').click();
});

test('failed account operations populate the native storage repair identity', async t => {
  const { client, container, button, workspace, overrides, requests } = await setup(t);
  const owner = { provider_id: 'native', has_oauth: true };
  const target = { workspace_id: 'workspace', owner, generation: { epoch: 'epoch', sequence: 1 } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', generation: target.generation, providers: [{ owner, configured: true, credentials: [], credential_slots: [] }] }));
  overrides.set('/v1/workspaces/workspace/auth/accounts', () => ({ target, accounts: [{ id: 'account', display_name: 'Saved account', active: true, credential_state: 'ready' }] }));
  overrides.set('/v1/workspaces/workspace/auth/logout', () => Response.json({ message: 'Account save failed' }, { status: 422 }));
  await client.refreshWorkspace();
  button('Settings').click(); button('Providers').click();
  await eventually(() => container.querySelector('.provider button:not(:disabled)'), 'provider loaded');
  button('Manage').click();
  await eventually(() => [...container.querySelectorAll('button')].some(item => item.textContent === 'Sign out' && !item.disabled), 'account loaded');
  button('Sign out').click();
  await eventually(() => container.textContent.includes('Account save failed'), 'native failure visible');
  const mutation = requests.find(item => item.path.endsWith('/auth/logout')).body;
  assert.equal(container.querySelector('[aria-label="Authentication operation workspace"]').value, mutation.target.workspace_id);
  assert.equal(container.querySelector('[aria-label="Authentication operation ID"]').value, mutation.operation_id);
  assert.equal(requests.some(item => item.path.endsWith('/auth/local-repair')), false);
});

test('captured workspace routes retain their original connection', async t => {
  const { client, requests, overrides } = await setup(t);
  const local = client.api.route('/sessions');
  client.api.connection = 'first-server';
  const remote = client.api.route('/sessions');
  overrides.set('/ui/connections/first-server/v1/workspaces/workspace/sessions', () => []);
  client.api.connection = 'second-server';
  await client.api.request(local);
  assert.equal(requests.at(-1).path, '/v1/workspaces/workspace/sessions');
  await client.api.request(remote);
  assert.equal(requests.at(-1).path, '/ui/connections/first-server/v1/workspaces/workspace/sessions');
  client.api.connection = '';
});

for (const boundary of ['dispose', 'workspace', 'connection']) test(`OAuth completion cannot report success after ${boundary}`, async t => {
  const target = { workspace_id: 'workspace', owner: { provider_id: 'native' }, generation: { epoch: 'epoch', sequence: 1 } };
  const events = [], requests = [];
  let resolveComplete;
  const client = {
    state: { workspace: { id: 'workspace' } },
    api: { connection: '', endpoint: path => path, workspace: { id: 'workspace' }, request: async (path, _method, body) => {
      requests.push({ path, body });
      if (path.endsWith('/begin')) return { login: body, state: { login: body, sequence: 1, phase: 'authorized' } };
      if (path.endsWith('/complete')) return new Promise(resolve => { resolveComplete = () => resolve({ workspace: { id: 'workspace' }, outcome: { login_id: body.login_id, operation_id: body.operation_id, previous: target, progress: { runtime_published: true }, change: { current: { target: { ...target, generation: { epoch: 'epoch', sequence: 2 } } } } } }); });
      throw new Error(`Unexpected request: ${path}`);
    } },
    fail: error => events.push(['fail', error.message]),
  };
  const flow = createLogin(client, target, Object.fromEntries(['status', 'error', 'retryAvailable', 'done', 'closed'].map(name => [name, value => events.push([name, value])])));
  t.after(() => flow.dispose());
  await eventually(() => resolveComplete, 'completion request retained');
  events.length = 0;
  if (boundary === 'dispose') flow.dispose();
  else if (boundary === 'connection') client.api.connection = 'other-server';
  else { client.state.workspace = { id: 'different' }; client.api.workspace = client.state.workspace; }
  resolveComplete();
  await delay(0);
  assert.deepEqual(events, []);
  await flow.retry();
  assert.equal(requests.filter(item => item.path.endsWith('/complete')).length, 1);
});

for (const phase of ['canceled', 'committing']) test(`disposed OAuth cancellation cannot act on a delayed ${phase} response`, async t => {
  const target = { workspace_id: 'workspace', owner: { provider_id: 'native' }, generation: { epoch: 'epoch', sequence: 1 } };
  const events = [], requests = [];
  let resolveCancel;
  const client = {
    state: { workspace: { id: 'workspace' } },
    api: { connection: '', endpoint: path => path, workspace: { id: 'workspace' }, request: async (path, _method, body, signal) => {
      requests.push({ path, body });
      if (path.endsWith('/begin')) return { login: body, state: { login: body, sequence: 1, phase: 'waiting-for-device' } };
      if (path.endsWith('/wait')) return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true }));
      if (path.endsWith('/cancel')) return new Promise(resolve => { resolveCancel = () => resolve({ login: body, state: { login: body, sequence: 2, phase } }); });
      throw new Error(`Unexpected request: ${path}`);
    } },
    fail: error => events.push(['fail', error.message]),
  };
  const flow = createLogin(client, target, Object.fromEntries(['status', 'error', 'retryAvailable', 'done', 'closed'].map(name => [name, value => events.push([name, value])])));
  t.after(() => flow.dispose());
  await eventually(() => requests.some(item => item.path.endsWith('/wait')), 'polling started');
  const canceled = flow.cancel();
  await eventually(() => resolveCancel, 'cancellation retained');
  await flow.cancel();
  flow.dispose();
  events.length = 0;
  resolveCancel();
  await canceled;
  assert.deepEqual(events, []);
  assert.equal(requests.filter(item => item.path.endsWith('/cancel')).length, 1);
  assert.equal(requests.some(item => item.path.endsWith('/complete')), false);
});

test('OAuth save retries serialize and retain the original login', async t => {
  const target = { workspace_id: 'workspace', owner: { provider_id: 'native' }, generation: { epoch: 'epoch', sequence: 1 } };
  const completions = [], events = [];
  let finish;
  const client = {
    state: { workspace: { id: 'workspace' } },
    api: { connection: '', endpoint: path => path, workspace: { id: 'workspace' }, request: async (path, _method, body) => {
      if (path.endsWith('/begin')) return { login: body, state: { login: body, sequence: 1, phase: 'authorized' } };
      if (path.endsWith('/complete')) {
        completions.push(body);
        if (completions.length === 1) throw new Error('Save unavailable');
        return new Promise(resolve => { finish = () => resolve({ workspace: { id: 'workspace' }, outcome: { login_id: body.login_id, operation_id: body.operation_id, previous: target, progress: { runtime_published: true }, change: { current: { target: { ...target, generation: { epoch: 'epoch', sequence: 2 } } } } } }); });
      }
      throw new Error(`Unexpected request: ${path}`);
    } },
    fail: error => events.push(['fail', error.message]),
  };
  const flow = createLogin(client, target, Object.fromEntries(['status', 'error', 'retryAvailable', 'done', 'closed'].map(name => [name, value => events.push([name, value])])));
  t.after(() => flow.dispose());
  await eventually(() => events.some(([name, value]) => name === 'retryAvailable' && value), 'save retry available');
  const retry = flow.retry();
  await eventually(() => finish, 'retry retained');
  await flow.retry();
  assert.equal(completions.length, 2);
  assert.deepEqual(completions, [flow.login, flow.login]);
  finish();
  await retry;
  assert.equal(events.filter(([name]) => name === 'done').length, 1);
});

test('authentication cannot adopt a superseded or mismatched receipt', () => {
  const target = { workspace_id: 'workspace', owner: { provider_id: 'native' }, generation: { epoch: 'epoch', sequence: 1 } };
  const request = { operation_id: 'operation', target, account_id: 'account' };
  const receipt = { workspace: { id: 'workspace' }, outcome: { operation_id: 'operation', previous: target, superseded: false, progress: { runtime_published: true }, change: { current: { target: { ...target, generation: { epoch: 'epoch', sequence: 2 } }, status: { active_account_id: 'account' } } } } };
  assert.doesNotThrow(() => validateAuthReceipt(receipt, request));
  assert.throws(() => validateAuthReceipt({ ...receipt, outcome: { ...receipt.outcome, superseded: true } }, request), /current runtime receipt/);
  assert.throws(() => validateAuthReceipt(receipt, { ...request, account_id: 'different' }), /different account/);
  assert.throws(() => validateAuthReceipt(receipt, { ...request, operation_id: 'other' }), /does not match/);
});

test.after(async () => { await window.happyDOM.abort(); await rm(directory, { recursive: true, force: true }); });

test('unsent drafts are restored after the page reloads and cleared after sending', async t => {
  const { client, container } = await setup(t);
  const input = () => container.querySelector('[aria-label="Message Crux"]');
  input().value = 'Survives reload'; input().dispatchEvent(new window.Event('input', { bubbles: true }));
  window.dispatchEvent(new window.Event('pagehide'));
  container.textContent = '';
  const reloaded = createCruxState();
  await reloaded.connect(true);
  await reloaded.selectSession('session');
  const second = document.createElement('div'); document.body.append(second);
  const dispose = render(() => App({ client: reloaded }), second);
  t.after(() => { dispose(); reloaded.dispose(); second.remove(); });
  await eventually(() => second.querySelector('[aria-label="Message Crux"]')?.value === 'Survives reload', 'draft restored');
  assert.equal(client.state.sessionID, 'session');
});

test('model saves cannot bind sessions on a replacement connection with the same workspace ID', async t => {
  const { client, workspace, overrides, requests } = await setup(t);
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  overrides.set('/v1/workspaces/workspace/config/model', () => pending);
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  const owner = { provider_id: 'native' };
  const model = { provider: 'native', model: 'chosen' };
  const choosing = assert.rejects(client.chooseModel({ id: 'native', owner }, model, 0), /original workspace is no longer connected/);
  await eventually(() => requests.some(item => item.path.endsWith('/config/model')), 'model save pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  await client.selectSession('session');
  release({ large: { model, owner } });
  await choosing;
  assert.equal(client.api.connection, 'remote');
  assert.equal(requests.some(item => item.path === '/ui/connections/remote/v1/workspaces/workspace/sessions/session' && item.method === 'PUT'), false);
});

test('delivery saves retain attachment ownership across same-ID connections', async t => {
  const { client, workspace, overrides } = await setup(t);
  let releaseLocal;
  let releaseRemote;
  overrides.set('/v1/workspaces/workspace/config/set', () => new Promise(resolve => { releaseLocal = resolve; }));
  const remote = { ...workspace, config: structuredClone(workspace.config), authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace/config/set', () => new Promise(resolve => { releaseRemote = resolve; }));
  const localSave = client.setDeliveryMode('steer');
  await eventually(() => Boolean(releaseLocal), 'local delivery save pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  assert.equal(client.state.deliverySaving, false);
  const remoteSave = client.setDeliveryMode('queue');
  await eventually(() => Boolean(releaseRemote), 'remote delivery save pending');
  releaseLocal({});
  await localSave;
  assert.equal(client.state.deliveryMode, 'queue');
  assert.equal(client.state.deliverySaving, true);
  releaseRemote({});
  await remoteSave;
  assert.equal(client.state.deliverySaving, false);
});

test('goal updates stop before mutation when their attachment changes during a read', async t => {
  const { client, workspace, session, overrides, requests } = await setup(t);
  let release;
  overrides.set('/v1/workspaces/workspace/sessions/session', () => new Promise(resolve => { release = resolve; }));
  const remote = { ...workspace, authority: { mode: 'client', revision: 1, digest: 'remote' } };
  overrides.set('/ui/connections/remote/v1/workspaces', () => remote);
  overrides.set('/ui/connections/remote/v1/workspaces/workspace', () => remote);
  const updating = assert.rejects(client.runGoal('/goal Keep the original project'), /workspace changed during the goal update/);
  await eventually(() => Boolean(release), 'goal read pending');
  await client.switchWorkspace('/project', '/project/.crux', 'remote');
  release(session);
  await updating;
  assert.equal(requests.some(item => item.path.endsWith('/goal') && item.method === 'PUT'), false);
});

test('legacy local drafts migrate to connection-scoped storage', async t => {
  const draft = { text: 'Saved before remote projects', files: [], skills: [], updated: Date.now() };
  const { container } = await setup(t, [['crux.web.drafts', JSON.stringify({ version: 1, saved: Date.now(), value: { '/project:session': draft } })]]);
  await eventually(() => container.querySelector('[aria-label="Message Crux"]')?.value === draft.text, 'legacy draft restored');
  await eventually(() => {
    const value = JSON.parse(localStorage.getItem('crux.web.drafts')).value;
    return !value['/project:session'] && value[JSON.stringify(['', '/project', '/project/.crux', 'session'])]?.text === draft.text;
  }, 'legacy draft migrated');
});

test('new-session drafts survive mode selection, rejected sends, and session switching', async t => {
  const { client, container, button, enter, overrides } = await setup(t);
  await client.selectSession('');
  enter('[aria-label="Message Crux"]', 'Keep this unsent draft');
  const mode = container.querySelector('[aria-label="Interaction mode"]');
  mode.value = 'plan'; mode.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => client.state.currentSession?.mode === 'plan', 'new plan session');
  assert.equal(container.querySelector('[aria-label="Message Crux"]').value, 'Keep this unsent draft');
  overrides.set('/v1/workspaces/workspace/agent', (_body, method) => method === 'POST' ? Response.json({ message: 'fixture admission rejected' }, { status: 409 }) : { is_ready: true });
  button('Send message').click();
  await eventually(() => client.state.error.includes('fixture admission rejected'), 'visible send failure');
  assert.equal(container.querySelector('[aria-label="Message Crux"]').value, 'Keep this unsent draft');
  await client.selectSession('session');
  enter('[aria-label="Message Crux"]', 'Separate session draft');
  await client.selectSession('new-session');
  assert.equal(container.querySelector('[aria-label="Message Crux"]').value, 'Keep this unsent draft');
});

test('MCP prompt arguments and text/binary resources become editable message context', async t => {
  const { client, container, button, enter, overrides, requests } = await setup(t);
  overrides.set('/v1/workspaces/workspace/mcp/prompts', () => [{ id: 'ui-list-id', prompt_id: 'native-prompt-id', client_id: 'docs', title: 'Review design', arguments: [{ id: 'area', title: 'Area', required: true }] }]);
  overrides.set('/v1/workspaces/workspace/mcp/resources', () => [{ mcp_name: 'docs', name: 'Design reference', uri: 'docs://design' }]);
  overrides.set('/v1/workspaces/workspace/mcp/get-prompt', body => ({ prompt: `Review ${body.args.area}` }));
  overrides.set('/v1/workspaces/workspace/mcp/read-resource', () => [{ uri: 'docs://design', text: 'Design notes' }, { uri: 'docs://image', mime_type: 'image/png', blob: 'aW1hZ2U=' }]);
  button('MCP prompts and resources').click();
  await eventually(() => container.querySelector('[aria-label="MCP prompts & resources"] .list-item'), 'MCP prompt catalog');
  container.querySelector('[aria-label="MCP prompts & resources"] .list-item').click();
  enter('[aria-label="Prompt argument area"]', 'state ownership');
  button('Resolve prompt').click();
  await eventually(() => container.querySelector('.preview-box').textContent.includes('Review state ownership'), 'prompt preview');
  assert.deepEqual(requests.find(item => item.path.endsWith('/mcp/get-prompt')).body, { client_id: 'docs', prompt_id: 'native-prompt-id', args: { area: 'state ownership' } });
  button('Add to message').click();
  assert.equal(container.querySelector('[aria-label="Message Crux"]').value, 'Review state ownership');
  button('MCP prompts and resources').click(); button('Resources 1').click();
  await eventually(() => container.querySelector('[aria-label="MCP prompts & resources"] .list-item'), 'MCP resource catalog');
  container.querySelector('[aria-label="MCP prompts & resources"] .list-item').click();
  await eventually(() => container.querySelector('.preview-box').textContent.includes('Design notes'), 'resource content');
  button('Add to message').click(); button('Queue message').click();
  await eventually(() => client.state.queued.length === 1, 'MCP context submission');
  const sent = requests.find(item => item.path.endsWith('/agent') && item.method === 'POST').body;
  assert.match(sent.prompt, /Review state ownership[\s\S]*Design notes/);
  assert.deepEqual(sent.attachments, [{ file_name: 'Design reference-1', mime_type: 'image/png', content: 'aW1hZ2U=' }]);
});

test('foreground job count failures remain visible in the tasks panel', async t => {
  const { container, button, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/agent/jobs/detach', () => Response.json({ message: 'Foreground jobs unavailable' }, { status: 503 }));
  button('Background tasks').click();
  await eventually(() => container.querySelector('.tasks [role="alert"]')?.textContent.includes('Foreground jobs unavailable'), 'foreground job error');
  assert.equal([...container.querySelectorAll('.tasks button')].some(element => element.textContent.includes('foreground job')), false);
});

test('project context selection, task detachment, restart and continuation use native operations', async t => {
  const { client, container, button, enter, overrides, requests } = await setup(t);
  let active = false;
  overrides.set('/v1/workspaces/workspace/projects', () => [{ slug: 'project-a', name: 'Project A', status: 'active', selected: active, completed: 1, total: 3 }]);
  overrides.set('/v1/workspaces/workspace/projects/selection', body => { active = body.slug === 'project-a'; return {}; });
  client.set('activityRevision', value => value + 1);
  await eventually(() => container.querySelector('[aria-label="Active project"]').options.length === 2, 'project catalog');
  const select = container.querySelector('[aria-label="Active project"]'); select.value = 'project-a'; select.dispatchEvent(new window.Event('change', { bubbles: true }));
  await eventually(() => active, 'native project selection');
  overrides.set('/v1/workspaces/workspace/agent/jobs/detach', () => ({ count: 1 }));
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'task', type: 'shell', description: 'Completed checks', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }, { id: 'agent-task', type: 'agent', child_session_id: 'child', description: 'Research', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }]);
  button('Background tasks').click();
  await eventually(() => container.textContent.includes('Move 1 foreground'), 'detachable jobs');
  button('Move 1 foreground job to the background').click();
  container.querySelector('.task-row[data-task-id="task"]').click(); button('Restart').click(); button('Back to tasks').click();
  container.querySelector('.task-row[data-task-id="agent-task"]').click();
  enter('[aria-label="Task follow-up"]', 'Check the remaining references'); button('Send', container.querySelector('.task-chat')).click();
  await eventually(() => requests.some(item => item.path.endsWith('/tasks/agent-task/message')), 'task continuation');
  assert.deepEqual(requests.find(item => item.path.endsWith('/tasks/agent-task/message')).body, { parent_session_id: 'session', prompt: 'Check the remaining references' });
  assert.ok(requests.some(item => item.path.endsWith('/tasks/task/restart') && item.method === 'POST'));
  assert.match(requests.find(item => item.path.endsWith('/jobs/detach') && item.method === 'POST').search, /session_id=session/);
});

for (const adapter of ['hosted-paste', 'device-code']) test(`provider ${adapter} sign-in commits the original login and validates its runtime receipt`, async t => {
  const { client, container, button, enter, overrides, workspace, requests } = await setup(t);
  const owner = { provider_id: 'native', has_oauth: true, oauth_adapter: adapter, oauth_flow_id: 'flow' };
  const target = { workspace_id: 'workspace', owner, generation: { epoch: 'epoch', sequence: 1 } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [], authentication: [{ kind: 'oauth', flow_id: 'flow', adapter, available: true }] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', generation: target.generation, providers: [{ owner, configured: true, credentials: [], account_state: 'none' }] }));
  overrides.set('/v1/workspaces/workspace/auth/accounts', () => ({ target, accounts: [] }));
  let login;
  let submitted;
  let resume;
  const reply = (phase, sequence) => ({ login, state: { login, phase, sequence, ...(['waiting-for-code', 'waiting-for-device'].includes(phase) ? { authorization_url: 'https://example.test/login' } : {}), ...(phase === 'waiting-for-device' ? { user_code: 'DISPLAY-CODE' } : {}) } });
  overrides.set('/v1/workspaces/workspace/auth/oauth/begin', body => { login = body; return reply(adapter === 'device-code' ? 'waiting-for-device' : 'waiting-for-code', 1); });
  overrides.set('/v1/workspaces/workspace/auth/oauth/wait', (body, _method, init) => body.after >= 2 ? reply('authorized', 3) : new Promise((resolve, reject) => { resume = resolve; init.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true }); }));
  overrides.set('/v1/workspaces/workspace/auth/oauth/code', body => { submitted = body; resume(reply('authorizing', 2)); return { ...reply('authorizing', 2), submission_id: body.submission_id }; });
  overrides.set('/v1/workspaces/workspace/auth/oauth/complete', body => ({ workspace: { id: 'workspace' }, outcome: { login_id: body.login_id, operation_id: body.operation_id, previous: target, progress: { runtime_published: true }, change: { current: { target: { ...target, generation: { epoch: 'epoch', sequence: 2 } }, status: { active_account_id: 'new-account' } } } } }));
  await client.refreshWorkspace();
  button('Settings').click(); button('Providers').click();
  await eventually(() => container.querySelector('.provider button:not(:disabled)'), 'provider status');
  button('Manage').click();
  const signIn = adapter === 'device-code' ? 'Sign in with a device code' : 'Sign in (paste code)';
  await eventually(() => !button(signIn).disabled, 'account snapshot');
  button(signIn).click();
  await eventually(() => container.querySelector('.oauth a') && resume, 'native sign-in controls');
  assert.equal(container.querySelector('.oauth a').getAttribute('href'), 'https://example.test/login');
  if (adapter === 'device-code') {
    assert.equal(container.querySelector('.device-code code').textContent, 'DISPLAY-CODE');
    assert.equal(container.querySelector('[aria-label="Authorization code"]'), null);
    resume(reply('authorized', 3));
  } else {
    enter('[aria-label="Authorization code"]', 'fixture-authorization-code'); button('Submit').click();
  }
  await eventually(() => container.textContent.includes('Signed in to Native Provider.'), 'validated native receipt');
  if (adapter === 'hosted-paste') {
    assert.deepEqual(submitted.login, login);
    assert.equal(submitted.input, 'fixture-authorization-code');
  } else assert.equal(requests.some(item => item.path.endsWith('/oauth/code')), false);
  assert.deepEqual(requests.find(item => item.path.endsWith('/oauth/complete')).body, login);
  assert.equal(requests.filter(item => item.path.endsWith('/oauth/begin')).length, 1);
});

test('Electron folder picker feeds the native workspace path without opening a browser', async t => {
  const { container, button, enter, requests } = await setup(t);
  window.cruxDesktop = { selectDirectory: async () => '/chosen project' };
  t.after(() => delete window.cruxDesktop);
  button('Command palette').click(); button('Open workspace…').click();
  button('Choose…').click();
  await eventually(() => container.querySelector('[aria-label="Workspace directory"]').value === '/chosen project', 'directory selected');
  assert.equal(requests.filter(item => item.path === '/v1/workspaces' && item.method === 'POST').length, 1);
});

test('rendered permission and question decisions preserve native request identities', async t => {
  const { client, container, button, enter, requests, overrides } = await setup(t);
  const permission = { id: 'permission-id', session_id: 'session', tool_call_id: 'call', tool_name: 'bash', action: 'execute', description: 'Run tests', path: '/project', params: { command: 'go test' } };
  client.set('permission', permission);
  button('Allow once').click();
  await eventually(() => !client.state.permission, 'native permission resolved');
  assert.deepEqual(requests.find(item => item.path.endsWith('/permissions/grant')).body, { permission, action: 'allow' });
  client.set('question', { id: 'batch-id', session_id: 'session', questions: [{ id: 'question-id', type: 'free_text', question: 'Which file?', description: '' }] });
  overrides.set('/v1/workspaces/workspace/questions/answer', () => ({ resolved: true }));
  enter('[aria-label="Which file?: written answer"]', 'app.ts'); button('Submit').click();
  await eventually(() => !client.state.question, 'question resolved');
  assert.deepEqual(requests.find(item => item.path.endsWith('/questions/answer')).body, { batch_request_id: 'batch-id', responses: [{ request_id: 'question-id', fill_in_text: 'app.ts' }] });
});

test('question batches render labels and markdown while preserving exact answers', async t => {
  const { client, container, button, enter, requests, overrides } = await setup(t);
  client.set('question', { id: 'batch', session_id: 'session', confirm_title: 'Apply choices?', confirm_description: '**Review** your answers.', questions: [
    { id: 'file', label: 'File', type: 'free_text', question: 'Which file?', description: 'Use the **full path**.' },
    { id: 'enable', label: 'Enable', type: 'yes_no', question: 'Enable caching?', description: 'Retain `cache` entries.' },
  ] });
  overrides.set('/v1/workspaces/workspace/questions/answer', () => ({ resolved: true }));
  const card = container.querySelector('[aria-label="Question from Crux"]');
  assert.equal(button('File', card).getAttribute('aria-current'), 'step');
  await eventually(() => card.querySelector('.markdown strong')?.textContent === 'full path', 'formatted question description');
  enter('[aria-label="Which file?: written answer"]', '/project/app.ts');
  button('Next', card).click();
  button('No', card).click();
  button('Next', card).click();
  await eventually(() => card.querySelector('.confirm-summary .markdown strong')?.textContent === 'Review', 'formatted confirmation');
  button('Submit', card).click();
  await eventually(() => !client.state.question, 'batch resolved');
  assert.deepEqual(requests.find(item => item.path.endsWith('/questions/answer')).body, { batch_request_id: 'batch', responses: [
    { request_id: 'file', fill_in_text: '/project/app.ts' },
    { request_id: 'enable', yes: false, fill_in_text: '' },
  ] });
});

test('code copying uses sanitized rendered code and shell output survives closing the dock', async t => {
  const { client, container, button, enter } = await setup(t);
  let copied;
  const original = navigator.clipboard.writeText;
  navigator.clipboard.writeText = async text => { copied = text; };
  t.after(() => { navigator.clipboard.writeText = original; });
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: '<script>window.attacked=true</script>\n\n```js\nconst result = 42;\n```' } }]);
  await eventually(() => container.querySelector('[data-copy-code]'), 'code copy control');
  assert.equal(container.querySelector('.markdown script'), null);
  container.querySelector('button[data-copy-code]').click();
  await eventually(() => copied !== undefined, 'copied code');
  assert.equal(copied.trim(), 'const result = 42;');
  button('Toggle shell').click();
  const input = enter('[aria-label="Shell command"]', 'printf shell-result');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => container.querySelector('.shell-log').textContent.includes('shell-result'), 'shell completes');
  button('Collapse shell').click(); button('Toggle shell').click();
  assert.match(container.querySelector('.shell-log').textContent, /shell-result/);
});

for (const [adapter, committing] of [['device-code', false], ['browser', false], ['browser', true]]) test(`${adapter} sign-in retains original identity through ${committing ? 'an already admitted commit' : 'cancellation'}`, async t => {
  const { client, container, button, overrides, workspace, requests } = await setup(t);
  const owner = { provider_id: 'native', has_oauth: true, oauth_adapter: adapter, oauth_flow_id: 'flow' };
  const target = { workspace_id: 'workspace', owner, generation: { epoch: 'epoch', sequence: 1 } };
  workspace.provider_surfaces = [{ id: 'native', name: 'Native Provider', owner, available: true, models: [], authentication: [{ kind: 'oauth', flow_id: 'flow', adapter, available: true }] }];
  overrides.set('/v1/workspaces/workspace/auth', () => ({ workspace_id: 'workspace', generation: target.generation, providers: [{ owner, configured: true, credentials: [], account_state: 'none' }] }));
  overrides.set('/v1/workspaces/workspace/auth/accounts', () => ({ target, accounts: [] }));
  let login;
  const reply = (phase, sequence) => ({ login, state: { login, phase, sequence, ...(phase === 'waiting-for-browser' || phase === 'waiting-for-device' ? { authorization_url: 'https://example.test/verify' } : {}), ...(phase === 'waiting-for-device' ? { user_code: 'DISPLAY-CODE' } : {}) } });
  const pending = signal => new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true }));
  overrides.set('/v1/workspaces/workspace/auth/oauth/begin', body => { login = body; return reply(adapter === 'browser' ? 'waiting-for-loopback' : 'waiting-for-device', 1); });
  overrides.set('/v1/workspaces/workspace/auth/oauth/wait', (body, _method, init) => adapter === 'browser' && body.after === 1 ? reply('waiting-for-browser', 2) : pending(init.signal));
  overrides.set('/ui/workspaces/workspace/oauth/loopback', (_body, _method, init) => pending(init.signal));
  overrides.set('/v1/workspaces/workspace/auth/oauth/cancel', () => committing ? reply('committing', 3) : Response.json({ ...reply('canceled', 3), error: { code: 'canceled', message: 'Sign-in canceled' } }, { status: 408 }));
  overrides.set('/v1/workspaces/workspace/auth/oauth/complete', body => ({ workspace: { id: 'workspace' }, outcome: { login_id: body.login_id, operation_id: body.operation_id, previous: target, progress: { runtime_published: true }, change: { current: { target: { ...target, generation: { epoch: 'epoch', sequence: 2 } }, status: { active_account_id: 'new-account' } } } } }));
  await client.refreshWorkspace();
  button('Settings').click(); button('Providers').click();
  await eventually(() => container.querySelector('.provider button:not(:disabled)'), 'provider status');
  const signIn = adapter === 'device-code' ? 'Sign in with a device code' : 'Sign in';
  button('Manage').click(); await eventually(() => !button(signIn).disabled, 'account snapshot');
  button(signIn).click();
  await eventually(() => container.querySelector('.oauth a'), 'native interaction URL');
  assert.equal(container.querySelector('.oauth a').getAttribute('href'), 'https://example.test/verify');
  assert.equal(container.querySelector('[aria-label="Authorization code"]'), null);
  if (adapter === 'device-code') assert.equal(container.querySelector('.device-code code').textContent, 'DISPLAY-CODE');
  else assert.deepEqual(requests.find(item => item.path.endsWith('/oauth/loopback')).body, login);
  button('Cancel sign-in').click();
  await eventually(() => !container.querySelector('.oauth'), 'native cancellation receipt');
  assert.deepEqual(requests.find(item => item.path.endsWith('/oauth/cancel')).body, login);
  assert.equal(requests.some(item => item.path.endsWith('/oauth/complete')), committing);
  if (committing) assert.deepEqual(requests.find(item => item.path.endsWith('/oauth/complete')).body, login);
});

test('browser notification controls request permission only on enable and expose test and disable actions', async t => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'Notification');
  const delivered = [];
  let requested = 0;
  class DesktopNotification {
    static permission = 'default';
    static async requestPermission() { requested++; return this.permission = 'granted'; }
    constructor(title, options) { this.title = title; this.options = options; delivered.push(this); }
    close() { this.closed = true; this.onclose?.(); }
  }
  Object.defineProperty(globalThis, 'Notification', { configurable: true, value: DesktopNotification });
  t.after(() => { if (descriptor) Object.defineProperty(globalThis, 'Notification', descriptor); else Reflect.deleteProperty(globalThis, 'Notification'); });
  const { client, container, button, requests } = await setup(t);
  button('Settings').click(); button('Behavior').click();
  assert.equal(requested, 0);
  assert.equal(button('Send test notification').disabled, true);
  button('Enable notifications').click();
  await eventually(() => client.state.notificationsEnabled, 'notification permission accepted');
  assert.equal(requested, 1);
  assert.match(container.textContent, /Notifications enabled for this tab/);
  button('Send test notification').click();
  assert.equal(delivered.length, 1);
  assert.equal(delivered[0].title, 'Crux notifications');
  delivered[0].onerror();
  assert.match(container.textContent, /could not display the notification/);
  button('Disable notifications').click();
  assert.equal(client.state.notificationsEnabled, false);
  assert.equal(requests.some(item => item.path.endsWith('/config/set')), false);
});

for (const permission of ['denied', 'default', 'unsupported']) test(`browser notification ${permission} status does not claim enabled delivery`, async t => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'Notification');
  const value = permission === 'unsupported' ? undefined : class { static permission = 'default'; static async requestPermission() { return permission; } };
  Object.defineProperty(globalThis, 'Notification', { configurable: true, value });
  t.after(() => { if (descriptor) Object.defineProperty(globalThis, 'Notification', descriptor); else Reflect.deleteProperty(globalThis, 'Notification'); });
  const { client, container, button } = await setup(t);
  button('Settings').click(); button('Behavior').click(); button('Enable notifications').click();
  await eventually(() => client.state.notificationError, 'notification failure visible');
  assert.equal(client.state.notificationsEnabled, false);
  assert.equal(button('Send test notification').disabled, true);
  assert.match(container.querySelector('.settings-section [role="alert"]').textContent, permission === 'denied' ? /blocked/ : permission === 'unsupported' ? /does not support/ : /not granted/);
});

test('recent errors remain inspectable after dismissal and update while the dialog is open', async t => {
  const { client, container, button, enter } = await setup(t);
  const text = `Provider request failed\n${'Full diagnostic detail '.repeat(80)}\n<script>not markup</script>`;
  client.fail(text);
  button('Dismiss error').click();
  button('Recent errors').click();
  const dialog = container.querySelector('[role="dialog"][aria-label="Recent errors"]');
  assert.ok(dialog);
  assert.equal(dialog.querySelector('pre').textContent, text);
  assert.equal(dialog.querySelector('script'), null);
  const original = navigator.clipboard.writeText;
  let copied;
  navigator.clipboard.writeText = async value => { copied = value; };
  t.after(() => { navigator.clipboard.writeText = original; });
  button('Copy error', dialog).click();
  await eventually(() => copied === text && dialog.textContent.includes('Copied'), 'full error copied');
  client.toast('A later failure', 'error');
  button('Dismiss').click();
  assert.equal(dialog.querySelectorAll('.error-record').length, 2);
  button('Copy listed errors', dialog).click();
  await eventually(() => copied.includes(text) && copied.includes('A later failure'), 'listed errors copied in full');
  enter('[aria-label="Search recent errors"]', 'later');
  assert.equal(dialog.querySelectorAll('.error-record').length, 1);
  assert.equal(dialog.querySelector('pre').textContent, 'A later failure');
  enter('[aria-label="Search recent errors"]', 'no such error');
  assert.match(dialog.textContent, /No matching errors/);
  button('Clear history', dialog).click();
  assert.match(dialog.textContent, /No recent errors/);
  assert.equal(client.state.recentErrors.length, 0);
});

test('recent error copying reports clipboard failure without losing the record', async t => {
  const { client, container, button } = await setup(t);
  client.fail('Retained failure');
  button('Recent errors').click();
  const dialog = container.querySelector('[role="dialog"][aria-label="Recent errors"]');
  const original = navigator.clipboard.writeText;
  navigator.clipboard.writeText = async () => { throw new Error('Clipboard is unavailable'); };
  t.after(() => { navigator.clipboard.writeText = original; });
  button('Copy error', dialog).click();
  await eventually(() => dialog.querySelector('[role="alert"]'), 'clipboard failure visible');
  assert.match(dialog.querySelector('[role="alert"]').textContent, /Clipboard is unavailable/);
  assert.equal(dialog.querySelector('pre').textContent, 'Retained failure');
});

test('recent errors are available from the palette and slash command without a ready model or submission', async t => {
  const { client, container, button, enter, requests } = await setup(t);
  button('Command palette').click();
  enter('[aria-label="Search commands"]', 'Recent errors');
  const option = container.querySelector('[role="option"]');
  assert.match(option.textContent, /Recent errors/);
  option.click();
  assert.ok(container.querySelector('[role="dialog"][aria-label="Recent errors"]'));
  button('Close recent errors').click();
  client.set('agent', 'is_ready', false);
  const input = enter('textarea[aria-label="Message Crux"]', '/errors');
  input.closest('form').dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => container.querySelector('[role="dialog"][aria-label="Recent errors"]'), 'slash command opens error history');
  assert.equal(requests.some(item => item.path.endsWith('/agent') && item.method === 'POST'), false);
});

test('native summary placeholders, streaming content, and completion retain their summary identity', async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'role', 'assistant');
  client.set('messages', 'message', 'is_summary_message', true);
  client.set('messages', 'message', 'parts', []);
  await eventually(() => container.querySelector('.conversation-summary'), 'summary placeholder visible');
  const summary = container.querySelector('.conversation-summary');
  assert.match(summary.textContent, /Conversation summary.*Summarizing/s);
  assert.equal(summary.open, true);
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: '**Retained context**' } }]);
  await eventually(() => summary.querySelector('strong')?.textContent === 'Retained context', 'summary streams Markdown');
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: '**Retained context**' } }, { type: 'finish', data: { reason: 'end_turn' } }]);
  assert.equal(summary.open, false);
  assert.equal(summary.textContent.includes('Summarizing'), false);
  summary.querySelector('summary').click();
  assert.equal(summary.open, true);
  assert.equal(summary.querySelector('strong').textContent, 'Retained context');
  assert.ok(container.querySelector('[data-message-id="message"] [aria-label="Copy message"]'));
  client.set('messages', 'message', 'is_summary_message', false);
  assert.equal(container.querySelector('.conversation-summary'), null);
  assert.equal(container.querySelector('[data-message-id="message"] .markdown strong').textContent, 'Retained context');
});

for (const reason of ['error', 'canceled', 'max_tokens']) test(`summary ${reason} status stays outside the collapsed content`, async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'role', 'assistant');
  client.set('messages', 'message', 'is_summary_message', true);
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: 'Partial context' } }, { type: 'finish', data: { reason, message: 'Summary did not finish' } }]);
  await eventually(() => container.querySelector('.conversation-summary'), 'summary rendered');
  assert.equal(container.querySelector('.conversation-summary').open, false);
  const status = container.querySelector('[data-message-id="message"] .turn-status');
  assert.match(status.textContent, /Summary did not finish/);
  assert.equal(status.closest('.conversation-summary'), null);
});

test('Markdown export labels native summaries without losing content', () => {
  const text = sessionMarkdown('Example', [{ role: 'assistant', is_summary_message: true, parts: [{ type: 'text', data: { text: '## Context\n\nRetained decisions' } }, { type: 'finish', data: { reason: 'end_turn' } }] }]);
  assert.match(text, /## Conversation summary\n\n## Context\n\nRetained decisions/);
  assert.match(text, /"reason": "end_turn"/);
});

test('reasoning duration uses native Unix-second timestamps', async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'role', 'assistant');
  client.set('messages', 'message', 'parts', [{ type: 'reasoning', data: { thinking: 'Reviewing the change.', started_at: 100, finished_at: 145 } }, { type: 'finish', data: { reason: 'stop' } }]);
  assert.match(container.querySelector('.thinking summary').textContent, /45s/);
});

test('native tool images and non-success finish reasons remain visible in the transcript', async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'parts', [{ type: 'tool_result', data: { name: 'image', content: 'Generated image', data: 'aW1hZ2U=', mime_type: 'image/png' } }, { type: 'finish', data: { reason: 'content_filter' } }]);
  await eventually(() => container.querySelector('.turn-status'), 'finish status');
  container.querySelector('.tool-head').click();
  assert.equal(container.querySelector('img.inline-image').getAttribute('src'), 'data:image/png;base64,aW1hZ2U=');
  assert.match(container.querySelector('.turn-status').textContent, /provider declined/);
  client.set('messages', 'message', 'parts', [{ type: 'finish', data: { reason: 'canceled' } }]);
  assert.match(container.querySelector('.turn-status').textContent, /Stopped/);
});

test('task envelopes render as a separate expandable message class without user controls or raw XML', async t => {
  const { client, container, button, overrides } = await setup(t);
  const envelope = '<task-notification><task-id>task</task-id><task-type>shell</task-type><status>failed</status><summary>Checks finished</summary><error-message>Command failed</error-message><exit-code>7</exit-code><result>Output &amp; diagnostic &lt;script&gt;not code&lt;/script&gt;</result></task-notification>';
  client.set('currentSession', 'is_busy', false);
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: envelope } }, { type: 'finish', data: { reason: 'stop' } }]);
  await eventually(() => container.querySelector('[aria-label="Background task notification"]'), 'classified notification');
  const notification = container.querySelector('[aria-label="Background task notification"]');
  assert.equal(notification.classList.contains('user'), false);
  assert.equal(notification.querySelector('.message-actions'), null);
  assert.equal(notification.querySelector('details').open, false);
  assert.match(notification.textContent, /Background shell.*Checks finished.*failed/s);
  assert.match(notification.querySelector('.task-notice-body .bad').textContent, /Command failed/);
  assert.doesNotMatch(notification.textContent, /<task-notification>|<task-id>/);
  assert.equal(notification.querySelector('script'), null);
  assert.match(notification.querySelector('pre').textContent, /Output & diagnostic <script>not code<\/script>/);
  assert.equal(container.querySelector('.turn-status'), null);
  overrides.set('/v1/workspaces/workspace/tasks/task/output', () => ({ task: { id: 'task', type: 'shell', description: 'Checks', ownership: { parent_session_id: 'session' }, state: { status: 'failed' } }, output: 'Result detail', output_truncated: false }));
  notification.querySelector('summary').click();
  button('Inspect task', notification).click();
  await eventually(() => container.querySelector('[data-task-id="task"] .task-terminal .terminal-surface')?.textContent === 'Result detail', 'notification opens native task output');
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: 'Please inspect <task-id>task</task-id> in this file.' } }]);
  assert.equal(container.querySelector('[aria-label="Background task notification"]'), null);
  assert.ok(container.querySelector('.message.user'));
  assert.ok(container.querySelector('.message.user .message-actions'));
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: '<task-notification><status>completed</status></task-notification>' } }]);
  assert.equal(container.querySelector('[aria-label="Background task notification"]'), null);
});

test('normal native finish reasons do not create warning banners and tool labels have separate fields', async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'role', 'tool');
  for (const reason of ['stop', 'end_turn', 'tool_use', 'tool-calls']) {
    client.set('messages', 'message', 'parts', [{ type: 'tool_call', data: { name: 'view', input: JSON.stringify({ file_path: '/workspace/long path/example.ts' }), finished: true } }, { type: 'finish', data: { reason } }]);
    assert.equal(container.querySelector('.turn-status'), null);
    assert.equal(container.querySelector('.tool-name').textContent, 'Read');
    assert.equal(container.querySelector('.tool-name').getAttribute('title'), 'view');
    assert.equal(container.querySelector('.tool-summary').textContent, '/workspace/long path/example.ts');
    assert.equal(container.querySelector('.tool-state').getAttribute('aria-label'), 'running');
  }
  for (const reason of ['error', 'canceled', 'max_tokens', 'content_filter', 'unknown']) {
    client.set('messages', 'message', 'parts', [{ type: 'finish', data: { reason, message: 'Provider detail' } }]);
    assert.match(container.querySelector('.turn-status').textContent, /Provider detail/);
  }
});

test('task toolbar works with the inspector closed and follows live output through completion', async t => {
  const { client, container, button, overrides } = await setup(t);
  if (container.querySelector('[aria-label="Session details"]')) button('Close details').click();
  assert.equal(container.querySelector('[aria-label="Session details"]'), null);
  assert.equal(container.querySelector('.tasks-button .count').textContent, '1');
  button('Background tasks').click();
  await eventually(() => container.querySelector('[data-task-id="task"]'), 'task panel from toolbar');
  let output = 'First line';
  overrides.set('/v1/workspaces/workspace/tasks/task/output', () => ({ output, output_truncated: false }));
  container.querySelector('.task-row[data-task-id="task"]').click();
  await eventually(() => container.querySelector('.task-terminal .terminal-surface')?.textContent === 'First line', 'initial task output');
  output = 'First line\nSecond line';
  await eventually(() => container.querySelector('.task-terminal .terminal-surface')?.textContent.includes('Second line'), 'automatic output polling');
  output += '\nFinished';
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'task', type: 'shell', description: 'Run checks', ownership: { parent_session_id: 'session' }, state: { status: 'completed', exit_code: 0 } }]);
  await client.refreshTasks();
  await eventually(() => container.querySelector('.task-terminal .terminal-surface')?.textContent.includes('Finished'), 'final output after completion');
  assert.equal(container.querySelector('.tasks-button .count'), null);
  assert.ok(button('Restart'));
  assert.ok(container.querySelector('.task-detail[data-task-id="task"]'));
  button('Back to tasks').click();
  assert.ok(container.querySelector('.task-row[data-task-id="task"]'));
});

test('workspace task scope includes other sessions and continuation keeps the task parent', async t => {
  const { client, container, button, enter, overrides, requests } = await setup(t);
  overrides.set('/v1/workspaces/workspace/tasks', () => [{ id: 'other-task', type: 'agent', child_session_id: 'child', description: 'Other session task', ownership: { parent_session_id: 'other-session' }, state: { status: 'completed' } }]);
  await client.refreshTasks();
  button('Background tasks').click();
  await eventually(() => container.querySelector('[aria-label="Task scope"]'), 'task scope');
  assert.equal(container.querySelector('[data-task-id="task"]'), null);
  button('Workspace', container.querySelector('[aria-label="Task scope"]')).click();
  assert.match(container.querySelector('[data-task-id="other-task"]').textContent, /Other session task/);
  container.querySelector('.task-row[data-task-id="other-task"]').click();
  enter('[aria-label="Task follow-up"]', 'Continue review'); button('Send', container.querySelector('.task-chat')).click();
  await eventually(() => requests.some(item => item.path.endsWith('/tasks/other-task/message')), 'native continuation');
  assert.deepEqual(requests.find(item => item.path.endsWith('/tasks/other-task/message')).body, { parent_session_id: 'other-session', prompt: 'Continue review' });
  await client.selectSession('');
  assert.equal(button('Workspace', container.querySelector('[aria-label="Task scope"]')).getAttribute('aria-checked'), 'true');
  assert.ok(container.querySelector('[data-task-id="other-task"]'));
});

for (const paired of [false, true]) for (const field of ['task_id', 'shell_id']) test(`tool result opens its native ${field} with paired=${paired}`, async t => {
  const { client, container, button, requests } = await setup(t);
  client.set('messages', 'message', 'role', 'assistant');
  client.set('messages', 'message', 'parts', [
    ...(paired ? [{ type: 'tool_call', data: { id: 'call', name: 'agent', input: '{}', finished: true } }] : []),
    { type: 'tool_result', data: { tool_call_id: 'call', name: 'agent', content: 'Task started', metadata: JSON.stringify({ [field]: 'task' }) } },
  ]);
  container.querySelector('.tool-head').click();
  button('Inspect task').click();
  await eventually(() => requests.some(item => item.path.endsWith('/tasks/task/log')), 'native task output');
  assert.ok(container.querySelector('[aria-label="Session details"]'));
  assert.match(container.textContent, /Run checks/);
});

for (const metadata of ['', '{invalid', '{"task_id":42}']) test(`tool result does not invent a task link from metadata ${metadata}`, async t => {
  const { client, container } = await setup(t);
  client.set('messages', 'message', 'role', 'assistant');
  client.set('messages', 'message', 'parts', [{ type: 'tool_result', data: { tool_call_id: 'call', name: 'agent', content: 'Task task started', metadata } }]);
  container.querySelector('.tool-head').click();
  assert.equal([...container.querySelectorAll('button')].some(button => button.textContent.trim() === 'Inspect task'), false);
});

for (const relationship of ['parent_session_id', 'forked_from_session_id']) test(`session inspector navigates the native ${relationship}`, async t => {
  const { expand, client, button, overrides, requests, session } = await setup(t);
  const related = { ...session, id: 'origin', title: 'Origin conversation', is_busy: false };
  client.set('sessions', [session, related]);
  client.set('currentSession', { ...session, [relationship]: 'origin' });
  overrides.set('/v1/workspaces/workspace/sessions/origin', () => related);
  button('Command palette').click(); button('Background tasks').click(); expand('Session');
  button(relationship === 'parent_session_id' ? 'Open parent session' : 'Branched from Origin conversation').click();
  await eventually(() => client.state.sessionID === 'origin', 'related session selected');
  assert.ok(requests.some(item => item.path.endsWith('/current-session') && item.body.session_id === 'origin'));
});

test('question drafts survive reconnect and replay but not replacement batches', async t => {
  const { client, container, enter, overrides, closeStream, emit } = await setup(t);
  let question = { id: 'original-batch', session_id: 'session', questions: [{ id: 'item', type: 'free_text', question: 'Which file?', description: '' }] };
  overrides.set('/ui/workspaces/workspace/pending', () => ({ permission: null, question }));
  await client.refreshPending();
  const input = enter('[aria-label="Which file?: written answer"]', 'original-file');
  closeStream();
  assert.equal(client.state.connected, false);
  await eventually(() => client.state.connected, 'pending question restored after reconnect');
  assert.equal(container.querySelector('[aria-label="Which file?: written answer"]'), input);
  assert.equal(input.value, 'original-file');
  emit({ type: 'question_batch_request', payload: { type: 'created', payload: { ...question, confirm_title: 'Replayed batch' } } });
  await eventually(() => client.state.question?.confirm_title === 'Replayed batch', 'duplicate batch event applied');
  assert.equal(container.querySelector('[aria-label="Which file?: written answer"]'), input);
  assert.equal(input.value, 'original-file');
  question = { ...question, id: 'replacement-batch' };
  await client.refreshPending();
  const replacement = container.querySelector('[aria-label="Which file?: written answer"]');
  assert.notEqual(replacement, input);
  assert.equal(replacement.value, '');
  question = null;
  await client.refreshPending();
  assert.equal(container.querySelector('[aria-label="Question from Crux"]'), null);
});

for (const action of ['answer', 'cancel']) test(`question ${action} waits for connectivity without clearing the draft`, async t => {
  const { client, container, button, enter, requests, overrides } = await setup(t);
  const route = `/v1/workspaces/workspace/questions/${action}`;
  overrides.set(route, () => ({ resolved: true }));
  client.set('question', { id: 'question', session_id: 'session', questions: [{ id: 'item', type: 'free_text', question: 'Which file?', description: '' }] });
  const card = container.querySelector('[aria-label="Question from Crux"]');
  const input = enter('[aria-label="Which file?: written answer"]', 'original-file');
  const submit = button(action === 'answer' ? 'Submit' : 'Cancel', card);
  client.set('connected', false);
  assert.equal(submit.disabled, true);
  submit.dispatchEvent(new window.Event('click', { bubbles: true }));
  await delay(0);
  assert.equal(requests.filter(item => item.path === route).length, 0);
  assert.equal(input.value, 'original-file');
  client.set('connected', true);
  assert.equal(submit.disabled, false);
  submit.click();
  await eventually(() => !client.state.question, 'question refreshed after response');
  assert.equal(requests.filter(item => item.path === route).length, 1);
  if (action === 'answer') assert.deepEqual(requests.find(item => item.path === route).body, { batch_request_id: 'question', responses: [{ request_id: 'item', fill_in_text: 'original-file' }] });
});

for (const action of ['grant', 'answer', 'cancel']) test(`pending ${action} completion stays in its originating workspace`, async t => {
  const { client, container, button, enter, requests, overrides, workspace } = await setup(t);
  const route = `/v1/workspaces/workspace/${action === 'grant' ? 'permissions/grant' : `questions/${action}`}`;
  let resolve;
  overrides.set(route, () => new Promise(done => { resolve = done; }));
  if (action === 'grant') {
    client.set('permission', { id: 'permission', session_id: 'session', tool_call_id: 'call', tool_name: 'view', action: 'read', description: 'Read file', path: '/project/file', params: {} });
    button('Allow once').click();
  } else {
    client.set('question', { id: 'question', session_id: 'session', questions: [{ id: 'item', type: 'free_text', question: 'Which file?', description: '' }] });
    const card = container.querySelector('[aria-label="Question from Crux"]');
    if (action === 'answer') enter('[aria-label="Which file?: written answer"]', 'file');
    button(action === 'answer' ? 'Submit' : 'Cancel', card).click();
  }
  await eventually(() => !!resolve, 'decision request pending');
  client.api.workspace = { ...workspace, id: 'other' };
  const count = requests.length;
  resolve({ resolved: true });
  await delay(0);
  assert.equal(requests.length, count);
  assert.equal(requests.filter(item => item.path === route).length, 1);
});

for (const timestamp of ['2026-11-02T12:34:56Z', '0001-01-01T00:00:00Z']) test(`file read details use the native timestamp ${timestamp}`, async t => {
  const { expand, container, button, requests, overrides } = await setup(t);
  overrides.set('/v1/workspaces/workspace/filetracker/lastread', () => timestamp);
  button('Command palette').click(); button('Background tasks').click(); expand('Files');
  await eventually(() => container.querySelector('[aria-label="Last read: src/app.ts"]'), 'read file row');
  button('Last read: src/app.ts').click();
  await eventually(() => !container.querySelector('[aria-label="File read details"] [role="status"]'), 'read timestamp loaded');
  const details = container.querySelector('[aria-label="File read details"]');
  if (timestamp.startsWith('0001-')) assert.match(details.textContent, /No recorded read time/);
  else assert.equal(details.querySelector('time').getAttribute('datetime'), timestamp);
  const query = new URLSearchParams(requests.find(item => item.path.endsWith('/filetracker/lastread')).search);
  assert.equal(query.get('session_id'), 'session');
  assert.equal(query.get('path'), 'src/app.ts');
});

test('closing file details cancels a pending read timestamp', async t => {
  const { expand, container, button, overrides } = await setup(t);
  let signal;
  let resolve;
  overrides.set('/v1/workspaces/workspace/filetracker/lastread', (_body, _method, init) => {
    signal = init.signal;
    return new Promise(done => { resolve = done; });
  });
  button('Command palette').click(); button('Background tasks').click(); expand('Files');
  await eventually(() => container.querySelector('[aria-label="Last read: src/app.ts"]'), 'read file row');
  button('Last read: src/app.ts').click();
  await eventually(() => !!resolve, 'read timestamp pending');
  button('Close details').click();
  assert.equal(signal.aborted, true);
  resolve('2026-11-02T12:34:56Z');
  await delay(0);
  assert.equal(container.querySelector('[aria-label="File read details"]'), null);
});

test('MCP editing retains connection settings without copying managed tokens', async t => {
  const { client, container, button, enter, requests } = await setup(t);
  client.set('workspace', 'config', 'mcp', { example: {
    type: 'http', url: 'https://example.com/mcp', headers: { Authorization: '$MCP_TOKEN' },
    timeout: 30, enabled_tools: ['lookup'], disabled_tools: ['write'], disabled: true,
    oauth: true, oauth_client_id: 'client', oauth_client_secret: '$MCP_SECRET', oauth_callback_port: 4321,
    oauth_token: { access_token: 'fixture-only' },
  } });
  button('Settings').click(); button('Integrations').click(); button('Edit example').click();
  const dialog = container.querySelector('[aria-label="Edit MCP server"]');
  assert.equal(dialog.querySelector('[aria-label="Server name"]').disabled, true);
  assert.equal(dialog.querySelector('[aria-label="Server URL"]').value, 'https://example.com/mcp');
  enter('[aria-label="Server URL"]', 'https://example.com/updated');
  button('Save server', dialog).click();
  await eventually(() => requests.some(item => item.path.endsWith('/config/set')), 'MCP update');
  const request = requests.find(item => item.path.endsWith('/config/set'));
  assert.deepEqual(request.body, { scope: 1, key: 'mcp.example', value: {
    type: 'http', url: 'https://example.com/updated', args: [], env: {}, headers: { Authorization: '$MCP_TOKEN' },
    timeout: 30, enabled_tools: ['lookup'], disabled_tools: ['write'], disabled: true,
    oauth: true, oauth_client_id: 'client', oauth_client_secret: '$MCP_SECRET', oauth_callback_port: 4321,
  } });
});

for (const disableOAuth of [false, true]) test(`MCP editing explicitly clears optional settings with OAuth disabled=${disableOAuth}`, async t => {
  const { client, container, button, enter, requests } = await setup(t);
  client.set('workspace', 'config', 'mcp', { example: {
    type: 'http', url: 'https://example.com/mcp', timeout: 30,
    oauth: true, oauth_client_id: 'client', oauth_client_secret: '$MCP_SECRET', oauth_callback_port: 4321,
  } });
  button('Settings').click(); button('Integrations').click(); button('Edit example').click();
  const dialog = container.querySelector('[aria-label="Edit MCP server"]');
  enter('[aria-label="Server timeout"]', '');
  if (disableOAuth) dialog.querySelector('input[type="checkbox"]').click();
  else {
    enter('[aria-label="OAuth client ID"]', '');
    enter('[aria-label="OAuth client secret"]', '');
    enter('[aria-label="OAuth callback port"]', '');
  }
  button('Save server', dialog).click();
  await eventually(() => requests.some(item => item.path.endsWith('/config/set')), 'MCP clear update');
  const value = requests.find(item => item.path.endsWith('/config/set')).body.value;
  assert.equal(value.timeout, 0);
  assert.equal(value.oauth, !disableOAuth);
  assert.equal(value.oauth_client_id, '');
  assert.equal(value.oauth_client_secret, '');
  assert.equal(value.oauth_callback_port, 0);
});

test('MCP creation does not overwrite an existing server', async t => {
  const { client, container, button, enter, requests } = await setup(t);
  client.set('workspace', 'config', 'mcp', { example: { type: 'stdio', command: 'original' } });
  button('Settings').click(); button('Integrations').click(); button('Add server').click();
  const dialog = container.querySelector('[aria-label="Add MCP server"]');
  enter('[aria-label="Server name"]', 'example');
  enter('[aria-label="Server command"]', 'replacement');
  button('Add server', dialog).click();
  assert.match(dialog.textContent, /already exists/);
  assert.equal(requests.some(item => item.path.endsWith('/config/set')), false);
});

test('conversation rollback uses the native endpoint without rewind options', async t => {
  const { client, container, button, requests, session } = await setup(t);
  session.is_busy = false;
  client.set('currentSession', { ...session });
  client.set('messages', 'message', 'role', 'user');
  button('Command palette').click(); button('Rewind session…').click();
  container.querySelector('.scroll-list .list-item').click();
  const operation = container.querySelector('[aria-label="History operation"]');
  operation.value = 'rollback'; operation.dispatchEvent(new window.Event('change', { bubbles: true }));
  const dialog = container.querySelector('[aria-label="Roll back session"]');
  assert.match(dialog.textContent, /Files stay unchanged/);
  assert.equal(dialog.querySelector('input[type="checkbox"]'), null);
  button('Roll back', dialog).click();
  await eventually(() => requests.some(item => item.path.endsWith('/rollback')), 'native rollback request');
  assert.deepEqual(requests.find(item => item.path.endsWith('/rollback')).body, { message_id: 'message' });
  assert.equal(requests.some(item => item.path.endsWith('/rewind')), false);
});

test('late rollback completion leaves a replacement dialog open', async t => {
  const { client, container, button, requests, session, overrides } = await setup(t);
  session.is_busy = false;
  client.set('currentSession', { ...session });
  let resolve;
  overrides.set('/v1/workspaces/workspace/sessions/session/rollback', () => new Promise(done => { resolve = done; }));
  button('Command palette').click(); button('Rewind session…').click();
  container.querySelector('.scroll-list .list-item').click();
  const operation = container.querySelector('[aria-label="History operation"]');
  operation.value = 'rollback'; operation.dispatchEvent(new window.Event('change', { bubbles: true }));
  button('Roll back').click();
  await eventually(() => !!resolve, 'rollback pending');
  button('Cancel').click();
  button('Command palette').click(); button('Rewind session…').click();
  const replacement = container.querySelector('[aria-label="Rewind session"]');
  const revision = client.state.activityRevision;
  resolve({});
  await eventually(() => client.state.activityRevision > revision, 'rollback refresh completed');
  await delay(0);
  assert.equal(container.querySelector('[aria-label="Rewind session"]'), replacement);
  assert.equal(requests.filter(item => item.path.endsWith('/rollback')).length, 1);
});

test('configuration inspector reports native failures and reloads provenance', async t => {
  const { container, button, overrides, requests } = await setup(t);
  let available = false;
  overrides.set('/v1/workspaces/workspace/config-view', () => available
    ? { workspace_id: 'workspace', view: { source: 'workspace', model: 'selected-model' } }
    : Response.json({ message: 'Client runtime owns this configuration' }, { status: 400 }));
  overrides.set('/v1/workspaces/workspace/project/needs-init', () => ({ needs_init: true }));
  button('Settings').click(); button('Configuration').click();
  await eventually(() => container.textContent.includes('Model provenance: Client runtime owns this configuration'), 'native provenance error');
  assert.match(container.textContent, /This project has not been initialized/);
  available = true;
  button('Refresh configuration').click();
  await eventually(() => container.textContent.includes('selected-model'), 'provenance recovery');
  assert.equal(container.textContent.includes('Client runtime owns this configuration'), false);
  assert.equal(requests.filter(item => item.path.endsWith('/config-view')).length, 2);
});

test('configuration inspector reloads after reconnect and accepted runtime changes', async t => {
  const { client, container, button, overrides, requests } = await setup(t);
  let release;
  let signal;
  overrides.set('/v1/workspaces/workspace/config-view', (_body, _method, init) => {
    signal = init.signal;
    return new Promise(resolve => { release = resolve; });
  });
  button('Settings').click(); button('Configuration').click();
  await eventually(() => Boolean(release), 'configuration read pending');
  client.set('connected', false);
  await eventually(() => signal.aborted, 'disconnected configuration read aborted');
  assert.equal(button('Refresh configuration').disabled, true);
  const reads = requests.filter(item => item.path.endsWith('/config-view')).length;
  client.set('workspace', 'authority', 'revision', 2);
  await delay(0);
  assert.equal(requests.filter(item => item.path.endsWith('/config-view')).length, reads);
  overrides.set('/v1/workspaces/workspace/config-view', () => ({ view: { model: 'recovered-model' } }));
  overrides.set('/v1/workspaces/workspace/project/needs-init', () => ({ needs_init: true }));
  client.set('connected', true);
  await eventually(() => container.textContent.includes('recovered-model'), 'configuration restored');
  assert.match(container.textContent, /This project has not been initialized/);
  release({ view: { model: 'obsolete-model' } });
  await delay(0);
  assert.equal(container.textContent.includes('obsolete-model'), false);
  overrides.set('/v1/workspaces/workspace/config-view', () => ({ view: { model: 'updated-model' } }));
  client.set('workspace', 'authority', 'revision', 3);
  await eventually(() => container.textContent.includes('updated-model'), 'accepted runtime provenance refreshed');
});

test('empty views use direct labels and a neutral composer placeholder', async t => {
  const { client, container, button } = await setup(t);
  await client.selectSession('');
  assert.equal(container.querySelector('.welcome h1').textContent, 'New session');
  assert.equal(container.querySelector('.welcome > p'), null);
  assert.equal(container.querySelector('[aria-label="Message Crux"]').placeholder, 'Message Crux…');
  if (!container.querySelector('[aria-label="Session details"]')) button('Toggle details panel').click();
  assert.match(container.querySelector('[aria-label="Session details"]').textContent, /No session selected/);
  button('Command palette').click();
  assert.equal(container.querySelector('[aria-label="Search commands"]').placeholder, 'Search commands…');
});

test('older task notifications retrieve details outside the recent task list without polling terminal output', async t => {
  const { client, container, button, overrides, requests } = await setup(t);
  overrides.set('/v1/workspaces/workspace/tasks/older/output', () => ({ task: { id: 'older', type: 'shell', description: 'Earlier checks', ownership: { parent_session_id: 'session' }, state: { status: 'completed' } }, output: 'Earlier result', output_truncated: false }));
  client.set('messages', 'message', 'parts', [{ type: 'text', data: { text: '<task-notification><task-id>older</task-id><task-type>shell</task-type><status>completed</status><summary>Earlier checks completed</summary></task-notification>' } }]);
  button('Inspect task').click();
  await eventually(() => container.querySelector('[data-task-id="older"] .task-terminal .terminal-surface')?.textContent === 'Earlier result', 'retrieved older task');
  await delay(100);
  assert.ok(requests.filter(item => item.path.endsWith('/tasks/older/output')).length <= 2);
});

test('pending task notifications hide their envelope without changing native queue contents', async t => {
  const { client, container } = await setup(t);
  const prompt = '<task-notification><task-id>a12345678</task-id><task-type>agent</task-type><status>completed</status><summary>Review finished</summary><result>Internal result payload</result></task-notification>';
  client.set('queued', [{ delivery_mode: 'queue', submission_id: 'notification', prompt }, { delivery_mode: 'queue', submission_id: 'followup', prompt: 'User follow-up' }]);
  assert.match(container.querySelector('[aria-label="Pending prompts"] .queued').textContent, /TaskBackground agent · completed · Review finished/);
  assert.doesNotMatch(container.querySelector('[aria-label="Pending prompts"]').textContent, /task-notification|Internal result payload/);
  assert.match(container.querySelector('[aria-label="Pending prompts"]').textContent, /QueueUser follow-up/);
  assert.equal(client.state.queued[0].prompt, prompt);
  assert.equal(client.state.queued.length, 2);
});
