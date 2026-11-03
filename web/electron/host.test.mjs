import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, writeFile, readFile, realpath, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { startHost } from './host.mjs';

test('desktop launches exact native host arguments and joins shutdown', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'crux-desktop-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const binary = path.join(directory, 'crux');
  const record = path.join(directory, 'record.json');
  await writeFile(binary, `#!/usr/bin/env node\nimport{writeFileSync}from'node:fs';writeFileSync(${JSON.stringify(record)},JSON.stringify({args:process.argv.slice(2),cwd:process.cwd()}));console.log(JSON.stringify({url:'http://127.0.0.1:41001'}));setInterval(()=>{},1000);process.on('SIGTERM',()=>process.exit(0));`, { mode: 0o700 });
  const host = startHost(binary, ['--cwd', '/a workspace', '--data-dir', '/some state'], { cwd: directory });
  t.after(() => host.stop());
  assert.equal(await host.ready, 'http://127.0.0.1:41001');
  const saved = JSON.parse(await readFile(record, 'utf8'));
  assert.deepEqual(saved.args, ['web', '--json', '--cwd', '/a workspace', '--data-dir', '/some state']);
  assert.equal(saved.cwd, await realpath(directory));
  await host.stop();
  await host.stop();
  assert.equal(host.child.exitCode, 0);
});

test('desktop entry finishes evaluation before Electron becomes ready', { timeout: 1000 }, async () => {
  const entry = new URL('./main.mjs', import.meta.url);
  const source = (await readFile(entry, 'utf8')).replace(/^import .*;\n/gm, '').replaceAll('import.meta.url', JSON.stringify(entry.href));
  const { fileURLToPath } = await import('node:url');
  const ready = Promise.withResolvers();
  let started = false;
  const app = { isPackaged: false, setName() {}, on() {}, whenReady: () => ready.promise };
  const menu = { buildFromTemplate: value => value, setApplicationMenu() {} };
  const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
  const evaluate = new AsyncFunction('app', 'Menu', 'fileURLToPath', 'path', 'process', 'startHost', source);
  await evaluate(app, menu, fileURLToPath, path, process, () => {
    started = true;
    return { ready: new Promise(() => {}) };
  });
  assert.equal(started, false);
  ready.resolve();
  await Promise.resolve();
  assert.equal(started, true);
});

test('an invalid explicit binary fails without fallback', async () => {
  const host = startHost('/not/a/crux/binary', []);
  await assert.rejects(host.ready, /ENOENT/);
});

test('desktop refuses a non-local ready URL', async t => {
  const directory = await mkdtemp(path.join(tmpdir(), 'crux-desktop-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const binary = path.join(directory, 'crux');
  await writeFile(binary, `#!/usr/bin/env node\nconsole.log(JSON.stringify({url:'https://example.com/'}));setInterval(()=>{},1000);`, { mode: 0o700 });
  const host = startHost(binary, []);
  t.after(() => host.stop());
  await assert.rejects(host.ready, /invalid local host/);
});

test('desktop preload exposes explicit window and project actions', async () => {
  const { runInNewContext } = await import('node:vm');
  let api;
  const calls = [];
  const source = await readFile(new URL('./preload.cjs', import.meta.url), 'utf8');
  runInNewContext(source, { require(name) {
    assert.equal(name, 'electron');
    return { contextBridge: { exposeInMainWorld(name, value) { assert.equal(name, 'cruxDesktop'); api = value; } }, ipcRenderer: { invoke(channel, ...args) { calls.push([channel, ...args]); return Promise.resolve(channel === 'crux:select-directory' ? '/selected directory' : undefined); } } };
  } });
  assert.deepEqual(Object.keys(api), ['windowControl', 'selectDirectory', 'loadProjects', 'saveProjects']);
  assert.equal(Object.isFrozen(api), true);
  assert.equal(await api.selectDirectory(), '/selected directory');
  await api.windowControl('minimize');
  await api.loadProjects();
  const projects = { projects: [{ path: '/project', connection: 'remote' }] };
  await api.saveProjects(projects);
  assert.deepEqual(calls, [['crux:select-directory'], ['crux:window-control', 'minimize'], ['crux:projects-load'], ['crux:projects-save', projects]]);
});

test('macOS desktop command launches from a spaced path and preserves explicit host arguments', async t => {
  const { mkdir, copyFile, chmod } = await import('node:fs/promises');
  const { execFile } = await import('node:child_process');
  const { promisify } = await import('node:util');
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), 'crux desktop launcher ')));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const launcher = path.join(directory, 'Crux Desktop.command');
  await copyFile(new URL('../../Crux Desktop.command', import.meta.url), launcher);
  await chmod(launcher, 0o700);
  const exec = promisify(execFile);
  await assert.rejects(exec(launcher), error => { assert.match(error.stderr, /npm --prefix web ci/); return true; });
  const executable = path.join(directory, 'web/node_modules/electron/dist/Electron.app/Contents/MacOS/Electron');
  await mkdir(path.dirname(executable), { recursive: true });
  await writeFile(executable, '#!/bin/sh\nprintf "%s\\n" "$PWD" "${ELECTRON_RUN_AS_NODE-unset}" "$@"\n', { mode: 0o700 });
  await assert.rejects(exec(launcher), error => { assert.match(error.stderr, /build.sh --build/); return true; });
  const binary = path.join(directory, 'custom crux');
  await writeFile(binary, '#!/bin/sh\nexit 0\n', { mode: 0o700 });
  const { stdout } = await exec(launcher, ['--cwd', '/workspace with spaces', '--session', 'exact-session'], { cwd: tmpdir(), env: { ...process.env, CRUX_BINARY: binary, ELECTRON_RUN_AS_NODE: '1' } });
  assert.deepEqual(stdout.trim().split('\n'), [directory, 'unset', path.join(directory, 'web'), '--cwd', '/workspace with spaces', '--session', 'exact-session']);
  await assert.rejects(exec(launcher, [], { env: { ...process.env, CRUX_BINARY: '/missing/explicit/binary' } }), error => { assert.match(error.stderr, /Crux binary not found: \/missing\/explicit\/binary/); return true; });
});
