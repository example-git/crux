export type TerminalTarget = { kind: 'shell' } | { kind: 'tmux'; socket: string; id: string; name: string };
export type TerminalExit = { type: 'exit'; code: number; error?: string };
export type TmuxSession = { socket: string; id: string; name: string; windows: number; attached: number; activity: string };

export function terminalURL(origin: string, workspaceID: string, target: TerminalTarget, cols: number, rows: number) {
  const url = new URL(`/ui/workspaces/${encodeURIComponent(workspaceID)}/terminal`, origin);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  url.searchParams.set('cols', String(Math.max(2, Math.min(1000, Math.floor(cols)))));
  url.searchParams.set('rows', String(Math.max(2, Math.min(1000, Math.floor(rows)))));
  if (target.kind === 'tmux') {
    url.searchParams.set('tmux_session', target.id);
    if (target.socket) url.searchParams.set('tmux_socket', target.socket);
  }
  return url;
}

export function resizeMessage(cols: number, rows: number) {
  return JSON.stringify({ type: 'resize', cols: Math.max(2, Math.min(1000, Math.floor(cols))), rows: Math.max(2, Math.min(1000, Math.floor(rows))) });
}

export function parseTerminalExit(data: string): TerminalExit {
  const value = JSON.parse(data);
  if (value?.type !== 'exit' || typeof value.code !== 'number') throw new Error('The terminal sent an unrecognized control message.');
  return { type: 'exit', code: value.code, error: typeof value.error === 'string' && value.error ? value.error : undefined };
}

export function exitText(exit: TerminalExit) {
  if (exit.error) return exit.error;
  return exit.code === 0 ? 'Process exited.' : `Process exited with status ${exit.code}.`;
}

export function terminalLabel(target: TerminalTarget) {
  return target.kind === 'shell' ? 'Shell' : `tmux: ${target.name}`;
}
