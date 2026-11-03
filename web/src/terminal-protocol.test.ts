import assert from 'node:assert/strict';
import test from 'node:test';
import { exitText, parseTerminalExit, resizeMessage, terminalLabel, terminalURL } from './terminal-protocol.ts';

test('terminal URLs select the native shell or an exact tmux session with clamped sizes', () => {
  const shell = terminalURL('http://127.0.0.1:8080', 'work space', { kind: 'shell' }, 120.7, 1);
  assert.equal(shell.protocol, 'ws:');
  assert.equal(shell.pathname, '/ui/workspaces/work%20space/terminal');
  assert.equal(shell.searchParams.get('cols'), '120');
  assert.equal(shell.searchParams.get('rows'), '2');
  assert.equal(shell.searchParams.has('tmux_session'), false);
  const tmux = terminalURL('https://host', 'w', { kind: 'tmux', socket: 'crux-capture', id: '$3', name: 'crux-run' }, 80, 24);
  assert.equal(tmux.protocol, 'wss:');
  assert.equal(tmux.searchParams.get('tmux_session'), '$3');
  assert.equal(tmux.searchParams.get('tmux_socket'), 'crux-capture');
  const defaultSocket = terminalURL('http://h', 'w', { kind: 'tmux', socket: '', id: '$1', name: 'crux-a' }, 80, 24);
  assert.equal(defaultSocket.searchParams.has('tmux_socket'), false);
});

test('terminal control messages are explicit and malformed exits fail visibly', () => {
  assert.deepEqual(JSON.parse(resizeMessage(5000, 0)), { type: 'resize', cols: 1000, rows: 2 });
  assert.deepEqual(parseTerminalExit('{"type":"exit","code":3}'), { type: 'exit', code: 3, error: undefined });
  assert.equal(exitText(parseTerminalExit('{"type":"exit","code":0}')), 'Process exited.');
  assert.equal(exitText(parseTerminalExit('{"type":"exit","code":-1,"error":"Too many terminals are open."}')), 'Too many terminals are open.');
  assert.throws(() => parseTerminalExit('{"type":"other"}'), /unrecognized/);
  assert.equal(terminalLabel({ kind: 'tmux', socket: '', id: '$1', name: 'crux-a' }), 'tmux: crux-a');
});
