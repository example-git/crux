import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { projectStore } from './projects.mjs';

test('desktop shortcuts survive restart and serialized layout changes', async t => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'crux-projects-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const store = projectStore(directory);
  const initial = await store.load();
  assert.deepEqual(initial, { version: 1, pinned: true, open: true, projects: [] });
  const projects = [{ path: directory, expanded: true }, { path: path.join(directory, 'second'), dataDir: '.alternate', expanded: false }];
  const first = store.save({ ...initial, projects });
  const second = store.save({ ...initial, projects, pinned: false, open: false });
  await Promise.all([first, second]);
  assert.deepEqual(await projectStore(directory).load(), { ...initial, projects, pinned: false, open: false });
  await store.save({ ...initial, projects: projects.slice(1) });
  assert.deepEqual((await store.load()).projects, projects.slice(1));
});

test('invalid snapshots cannot replace the saved shortcuts', async t => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'crux-projects-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const store = projectStore(directory);
  const initial = await store.load();
  await store.save(initial);
  assert.throws(() => store.save({ ...initial, projects: [{ path: 'relative', expanded: true }] }), /absolute|shortcut/i);
  assert.throws(() => store.save({ ...initial, projects: [{ path: directory, expanded: true }, { path: directory, expanded: false }] }), /duplicate/i);
  assert.deepEqual(await store.load(), initial);
  await writeFile(path.join(directory, 'projects.json'), '{broken');
  await assert.rejects(() => store.load());
  assert.equal(await readFile(path.join(directory, 'projects.json'), 'utf8'), '{broken');
});
