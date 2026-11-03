import assert from 'node:assert/strict';
import test from 'node:test';
import { coalesceEvents, resolveSession } from './events.ts';
import type { Event } from './types.ts';

test('stream batches keep latest full messages without dropping decisions or completion', () => {
  const message = (text: string, id = 'message'): Event => ({ type: 'message', payload: { type: 'updated', payload: { id, session_id: 'session', text } } });
  const permission: Event = { type: 'permission_request', payload: { type: 'created', payload: { id: 'permission' } } };
  const complete: Event = { type: 'run_complete', payload: { type: 'created', payload: { run_id: 'run' } } };
  const events = coalesceEvents([message('a'), permission, message('ab'), message('other', 'second'), message('abc'), complete]);
  assert.deepEqual(events, [message('abc'), permission, message('other', 'second'), complete]);
});

test('explicit session IDs never fall back to another session', () => {
  const sessions = [{ id: 'abc-one' }, { id: 'abc-two' }, { id: 'unique' }];
  assert.equal(resolveSession(sessions, 'uniq'), 'unique');
  assert.equal(resolveSession(sessions, 'abc-one'), 'abc-one');
  assert.throws(() => resolveSession(sessions, 'abc'), /ambiguous/);
  assert.throws(() => resolveSession(sessions, 'missing'), /not found/);
});

test('large stream batches coalesce by message identity', () => {
  const updates = Array.from({ length: 100000 }, (_, i): Event => ({ type: 'message', payload: { type: 'updated', payload: { id: `message-${i % 100}`, session_id: 'session', text: String(i) } } }));
  const start = performance.now();
  const result = coalesceEvents(updates);
  assert.equal(result.length, 100);
  assert.equal(result[0].payload.payload.text, '99900');
  console.log(`100,000 stream updates → 100 message updates in ${(performance.now() - start).toFixed(1)} ms`);
});
