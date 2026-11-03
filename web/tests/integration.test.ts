import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, mkdir, writeFile, readFile, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { startHost } from '../electron/host.mjs';
import { createCruxState } from '../src/state.ts';

const binary = process.env.CRUX_BINARY ?? path.resolve(import.meta.dirname, '../../crux');

async function eventually(check: () => boolean, description: string) {
  const deadline = Date.now() + 15000;
  while (!check()) {
    if (Date.now() > deadline) throw new Error(`Timed out: ${description}`);
    await delay(10);
  }
}

test('repository binary, desktop launcher, and web state complete a native run using shared settings', { timeout: 90000 }, async t => {
  const cleanups: (() => unknown)[] = [];
  const cleanup = (callback: () => unknown) => cleanups.push(callback);
  t.after(async () => { for (const callback of cleanups.reverse()) await callback(); });
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), 'crux-web-integration-')));
  cleanup(() => rm(directory, { recursive: true, force: true }));
  const env = { ...process.env, CRUX_PROVIDER_PROFILE: 'integrated', CRUX_PROVIDER_PLUGINS: '', CRUX_PROVIDER_PLUGIN_COMPAT: '' };
  for (const key of ['XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME', 'CRUX_GLOBAL_CONFIG', 'CRUX_GLOBAL_DATA', 'CRUX_CACHE_DIR', 'AI_CLI_DIR']) {
    env[key] = path.join(directory, key);
    await mkdir(env[key], { recursive: true });
  }
  const workspace = path.join(directory, 'workspace with spaces');
  const dataDir = path.join(directory, 'custom data');
  await mkdir(workspace);
  const calls: Record<string, any>[] = [];
  const releaseRuns = new Map<string, () => void>();
  const interrupted = new Set<string>();
  const provider = createServer(async (request, response) => {
    let text = '';
    for await (const chunk of request) text += chunk;
    const body = JSON.parse(text);
    calls.push(body);
    assert.equal(request.headers.authorization, 'Bearer integration-key');
    assert.equal(request.url, '/v1/chat/completions');
    if (body.stream !== true) {
      response.setHeader('Content-Type', 'application/json');
      response.end(JSON.stringify({ id: 'title', choices: [{ index: 0, message: { role: 'assistant', content: 'Integration session' }, finish_reason: 'stop' }] }));
      return;
    }
    response.setHeader('Content-Type', 'text/event-stream');
    if (JSON.stringify(body.messages).includes('background-task-marker') && !body.messages.some((message: any) => message.role === 'tool' && message.tool_call_id === 'background-call')) {
      response.write('data: ' + JSON.stringify({ id: 'task-reply', object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', tool_calls: [{ index: 0, id: 'background-call', type: 'function', function: { name: 'bash', arguments: JSON.stringify({ description: 'Native background checks', command: 'printf native-task-start; while [ ! -f task-release ]; do sleep 0.1; done; printf native-task-complete', run_in_background: true }) } }] }, finish_reason: null }] }) + '\n\n');
      response.end('data: ' + JSON.stringify({ id: 'task-reply', object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] }) + '\n\ndata: [DONE]\n\n');
      return;
    }
    const finish = () => response.end('data: ' + JSON.stringify({ id: 'reply', object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }], usage: { prompt_tokens: 2, completion_tokens: 4, total_tokens: 6 } }) + '\n\ndata: [DONE]\n\n');
    response.write('data: ' + JSON.stringify({ id: 'reply', object: 'chat.completion.chunk', choices: [{ index: 0, delta: { role: 'assistant', content: 'Native web integration completed.' }, finish_reason: null }] }) + '\n\n');
    const prompt = JSON.stringify(body.messages);
    for (const mode of ['queue', 'steer']) {
      if (body.model === 'b' && prompt.includes(`${mode}-active-marker`) && !prompt.includes(`${mode}-followup-marker`)) {
        releaseRuns.set(mode, finish);
        response.on('close', () => { if (!response.writableEnded) interrupted.add(mode); });
        return;
      }
    }
    finish();
  });
  provider.listen(0, '127.0.0.1');
  await once(provider, 'listening');
  cleanup(() => { provider.closeAllConnections(); provider.close(); });
  const port = (provider.address() as { port: number }).port;
  const config = {
    options: { disable_default_providers: true, auto_lsp: false, notifications: 'disabled' },
    providers: { fixture: { type: 'openai-compat', base_url: `http://127.0.0.1:${port}/v1`, api_key: 'integration-key', models: ['a', 'b'].map(id => ({ id, name: id, context_window: 32768, default_max_tokens: 512 })) } },
    models: { large: { provider: 'fixture', model: 'a' }, small: { provider: 'fixture', model: 'a' } },
  };
  await writeFile(path.join(env.CRUX_GLOBAL_CONFIG!, 'crux.json'), JSON.stringify(config));
  const host = startHost(binary, ['--cwd', 'workspace with spaces', '--data-dir', dataDir], { env, cwd: directory });
  cleanup(() => host.stop());
  const origin = await host.ready;
  const page = await fetch(origin);
  assert.equal(page.status, 200);
  const html = await page.text();
  const script = html.match(/src="([^"]+\.js)"/);
  assert.ok(script, 'the host serves the built application');
  const asset = await fetch(new URL(script[1], origin));
  assert.equal(asset.status, 200);
  assert.match(asset.headers.get('cache-control')!, /immutable/);
  const servedAsset = await asset.text();
  assert.ok(servedAsset.length > 1000);
  assert.equal(servedAsset, await readFile(path.resolve(import.meta.dirname, '../../internal/webclient/dist', script[1].replace(/^\/+/, '')), 'utf8'));
  const globals: Record<string, unknown> = {
    location: new URL(origin), history: { replaceState() {} },
    requestAnimationFrame: (callback: FrameRequestCallback) => setTimeout(() => callback(performance.now()), 0),
    cancelAnimationFrame: clearTimeout,
  };
  const descriptors = new Map<string, PropertyDescriptor | undefined>();
  for (const [key, value] of Object.entries(globals)) {
    descriptors.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
  }
  const client = createCruxState();
  cleanup(() => {
    client.dispose();
    for (const [key, descriptor] of descriptors) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
  await client.connect(true);
  assert.equal(client.state.connected, true);
  assert.equal(client.state.agent?.is_ready, true);
  assert.equal(client.state.workspace?.path, workspace);
  assert.equal(client.state.workspace?.requested_data_dir, dataDir);
  assert.equal(client.state.workspace?.config.models.large.model, 'a');
  const surface = client.state.workspace!.provider_surfaces!.find(item => item.id === 'fixture')!;
  await client.chooseModel(surface, { provider: 'fixture', model: 'b', max_tokens: 512 }, 0);
  assert.equal(client.state.agent?.model_cfg.model, 'b');
  await client.send('process-integration-marker');
  await eventually(() => Object.values(client.state.messages).some(message => message.parts.some(part => part.type === 'text' && part.data.text === 'Native web integration completed.')), 'streamed native reply in web state');
  await eventually(() => !client.state.sessions.find(item => item.id === client.state.sessionID)?.is_busy, 'run completion');
  assert.equal(client.state.error, '');
  assert.ok(calls.some(call => call.model === 'b' && JSON.stringify(call.messages).includes('process-integration-marker')));
  const persisted = JSON.parse(await readFile(path.join(env.CRUX_GLOBAL_DATA!, 'crux.json'), 'utf8'));
  assert.equal(persisted.models.large.model, 'b');
  const sessionID = client.state.sessionID;
  await client.connect(false);
  assert.equal(client.state.sessionID, sessionID);
  assert.ok(client.state.messageIDs.length >= 2);
  assert.equal(client.state.workspace?.config.models.large.model, 'b');
  await client.updateSession('title', 'Renamed through web client');
  assert.equal(client.state.currentSession?.title, 'Renamed through web client');
  await client.forkSession();
  assert.notEqual(client.state.sessionID, sessionID);
  assert.equal(client.state.currentSession?.forked_from_session_id, sessionID);
  assert.ok(client.state.messageIDs.length >= 2);
  await client.updateSession('mode', 'plan');
  assert.equal(client.state.currentSession?.mode, 'plan');
  await client.updateSession('mode', 'default');
  await client.updateSession('analysis-effort', 'high');
  assert.equal(client.state.currentSession?.analysis_effort, 'high');
  const shell = await client.api.request<{ output: string; exit_code: number }>(client.api.route(`/agent/sessions/${client.state.sessionID}/shell`), 'POST', { command: 'printf web-shell-marker', term_width: 120 });
  assert.equal(shell.exit_code, 0);
  assert.equal(shell.output, 'web-shell-marker');
  await client.loadMessages(client.state.sessionID);
  assert.ok(Object.values(client.state.messages).some(message => message.parts.some(part => part.type === 'shell_command' && part.data.output === 'web-shell-marker')));
  const fileSearch = await client.api.request<{ files: unknown[] }>(client.api.route('/files/search'), 'POST', { query: 'probe', roots: [workspace], limit: 20 });
  assert.ok(Array.isArray(fileSearch.files));
  for (const mode of ['queue', 'steer'] as const) {
    await client.selectSession('');
    await client.setDeliveryMode(mode);
    const saved = JSON.parse(await readFile(path.join(env.CRUX_GLOBAL_DATA!, 'crux.json'), 'utf8'));
    assert.equal(saved.options.tui.delivery_mode, mode);
    await client.send(`${mode}-active-marker`);
    await eventually(() => releaseRuns.has(mode), `${mode} active provider stream`);
    const count = calls.filter(call => call.stream === true && call.model === 'b').length;
    await client.send(`${mode}-followup-marker`);
    const queueTimer = setInterval(() => void client.refreshQueue().catch(client.fail), 30);
    cleanup(() => clearInterval(queueTimer));
    if (mode === 'queue') {
      assert.equal(calls.filter(call => call.stream === true && call.model === 'b').length, count, 'queue does not interrupt the active stream');
      await eventually(() => client.state.queued.length > 0, 'queued follow-up snapshot');
      assert.equal(client.state.queued[0]?.delivery_mode, 'queue');
      assert.equal(client.state.queued[0]?.prompt, 'queue-followup-marker');
      await client.clearQueue();
      assert.equal(client.state.queued.length, 0);
      await client.send('queue-followup-marker');
      releaseRuns.get(mode)!();
    } else {
      await eventually(() => interrupted.has('steer'), 'steering interrupts the original provider stream');
    }
    await eventually(() => calls.some(call => call.stream === true && call.model === 'b' && JSON.stringify(call.messages).includes(`${mode}-followup-marker`)), `${mode} reaches provider`);
    await eventually(() => !client.state.currentSession?.is_busy, `${mode} completion`);
    assert.equal(client.state.error, '');
    await client.refreshQueue();
    assert.equal(client.state.queued.length, 0);
    clearInterval(queueTimer);
  }
  await client.connect(false);
  assert.equal(client.state.deliveryMode, 'steer');

  await client.selectSession('');
  await client.api.request(client.api.route('/permissions/skip'), 'POST', { skip: true });
  await client.send('background-task-marker');
  const taskTimer = setInterval(() => void client.refreshTasks(), 50);
  cleanup(() => clearInterval(taskTimer));
  await eventually(() => client.state.tasks.some(task => task.description === 'Native background checks'), 'native background task discovery');
  const task = client.state.tasks.find(task => task.description === 'Native background checks')!;
  assert.equal(task.ownership.parent_session_id, client.state.sessionID);
  assert.equal(task.state.status, 'running');
  const output = await client.api.request<{ output: string }>(client.api.route(`/tasks/${task.id}/output`), 'POST', { wait: false, timeout: 0 });
  assert.match(output.output, /native-task-start/);
  await eventually(() => !client.state.currentSession?.is_busy, 'background launch admission completes');
  await writeFile(path.join(workspace, 'task-release'), 'complete');
  await eventually(() => client.state.tasks.some(item => item.id === task.id && item.state.status === 'completed'), 'native background completion');
  await eventually(() => Object.values(client.state.messages).some(message => message.parts.some(part => part.type === 'text' && part.data.text.includes('<task-notification>') && part.data.text.includes(task.id))), 'native wrapped notification in transcript');
  const completed = await client.api.request<{ output: string }>(client.api.route(`/tasks/${task.id}/output`), 'POST', { wait: false, timeout: 0 });
  assert.match(completed.output, /native-task-complete/);
  await rm(path.join(workspace, 'task-release'));
  const previousOutput = task.output_ref;
  const restarted = await client.api.request<{ id: string; output_ref: string }>(client.api.route(`/tasks/${task.id}/restart`), 'POST');
  assert.equal(restarted.id, task.id);
  assert.notEqual(restarted.output_ref, previousOutput);
  await eventually(() => client.state.tasks.some(item => item.id === restarted.id && item.state.status === 'running'), 'native restarted task');
  await client.api.request(client.api.route(`/tasks/${restarted.id}/stop`), 'POST');
  await eventually(() => client.state.tasks.some(item => item.id === restarted.id && item.state.status === 'killed'), 'native stopped task');
  await eventually(() => calls.some(call => call.stream === true && JSON.stringify(call.messages).includes('<status>killed</status>')), 'stopped-task notification reaches native provider');
  await eventually(() => !client.state.currentSession?.is_busy, 'notification turn completes');
  clearInterval(taskTimer);

  client.dispose();
  await host.stop();
  assert.equal(host.child.exitCode, 0);
  await assert.rejects(fetch(origin));
  console.log('Verified: compiled assets, native settings, model ownership, streamed replies, rename/branch/plan/effort, shell persistence, file search, real Queue drain/clear, real Steer interruption, native background task output/completion/notification/restart/stop, reconnect, clean host shutdown.');
});

test('invalid explicit launch options fail without starting another mode', async () => {
  for (const [args, expected] of [
    [['--host', 'tcp://127.0.0.1:8080'], /not supported/],
    [['--connection', ''], /not supported/],
    [['--listen', '0.0.0.0:0'], /loopback IP/],
    [['--listen', '127.0.0.1:invalid'], /unknown port|invalid port/],
    [['--session', ''], /nonempty session ID/],
    [['--session', 'session', '--continue'], /none of the others can be/],
  ] as const) {
    await assert.rejects(promisify(execFile)(binary, ['web', ...args]), (error: any) => {
      assert.notEqual(error.code, 0);
      assert.match(error.stderr, expected);
      return true;
    });
  }
});

test('idle native control request shuts down the web listener', { timeout: 15000 }, async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'crux-web-control-'));
  const env = { ...process.env };
  for (const key of ['CRUX_GLOBAL_CONFIG', 'CRUX_GLOBAL_DATA', 'CRUX_CACHE_DIR', 'AI_CLI_DIR']) {
    env[key] = path.join(directory, key);
    await mkdir(env[key], { recursive: true });
  }
  const host = startHost(binary, ['--cwd', directory], { env, cwd: directory });
  t.after(async () => { await host.stop(); await rm(directory, { recursive: true, force: true }); });
  const origin = await host.ready;
  const response = await fetch(origin + '/v1/control', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: 'shutdown_if_idle' }) });
  assert.equal(response.status, 200);
  await host.exited;
  assert.equal(host.child.exitCode, 0);
  await assert.rejects(fetch(origin));
});
