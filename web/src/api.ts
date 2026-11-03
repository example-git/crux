import type { Event, Startup, Workspace } from './types.ts';

export class APIError extends Error {
  status: number;
  readonly payload?: any;
  constructor(status: number, message: string, payload?: any) {
    super(message);
    this.status = status;
    this.payload = payload;
  }
}

export class CruxClient {
  readonly clientID = crypto.randomUUID();
  workspace?: Workspace;
  connection = '';
  private socket?: WebSocket;
  private generation = 0;

  async request<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
    return (await this.exchange<T>(path, method, body, signal)).value;
  }

  async exchange<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<{ value: T; headers: Headers }> {
    const url = new URL(this.endpoint(path), location.origin);
    url.searchParams.set('client_id', this.clientID);
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    const authority = this.workspace?.authority;
    if (authority && url.origin === location.origin && url.pathname.startsWith(`/v1/workspaces/${this.workspace!.id}/`)) {
      headers['Crux-Workspace-Authority-Mode'] = authority.mode;
      headers['Crux-Workspace-Authority-Revision'] = String(authority.revision ?? 0);
      headers['Crux-Workspace-Authority-Digest'] = authority.digest ?? '';
    }
    const response = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store', signal });
    const text = await response.text();
    if (!response.ok) {
      let message = text || response.statusText;
      let payload: any;
      try { payload = JSON.parse(text); message = payload.error?.message || payload.message || message; } catch {}
      throw new APIError(response.status, message, payload);
    }
    return { value: (text ? JSON.parse(text) : undefined) as T, headers: response.headers };
  }

  endpoint(path: string, connection = this.connection) {
    if (!connection) return path;
    const prefix = `/ui/connections/${encodeURIComponent(connection)}`;
    if (path.startsWith('/v1/')) return `${prefix}${path}`;
    if (path === '/ui/tmux-sessions' && this.workspace) return `${prefix}/v1/workspaces/${this.workspace.id}/_ui/tmux-sessions`;
    if (path.startsWith('/ui/workspaces/')) return `${prefix}/v1/${path.slice(4).replace(/(workspaces\/[^/]+)\//, '$1/_ui/')}`;
    return path;
  }

  route(path: string) {
    if (!this.workspace) throw new Error('No workspace is connected.');
    return new URL(this.endpoint(`/v1/workspaces/${this.workspace.id}${path}`), location.origin).href;
  }

  async attach(startup: Startup, path = startup.path, dataDir = startup.data_dir, connection = ''): Promise<Workspace> {
    this.disconnect();
    const target = connection ? this.endpoint('/v1/workspaces', connection) : '/v1/workspaces';
    const previousConnection = this.connection;
    this.connection = connection;
    let workspace: Workspace;
    try {
      workspace = await this.request<Workspace>(target, 'POST', {
        path, data_dir: dataDir, debug: startup.debug, yolo: startup.yolo,
        channels: startup.channels, client_id: this.clientID, version: startup.version, authority_mode: connection ? 'client' : 'server',
      });
      if (workspace.authority?.mode !== (connection ? 'client' : 'server')) throw new Error('The web host returned an unexpected workspace authority.');
    } catch (error) {
      this.connection = previousConnection;
      throw error;
    }
    workspace.connection_name = connection;
    this.workspace = workspace;
    return workspace;
  }

  connect(onEvent: (event: Event) => void, onClose: () => void): Promise<void> {
    this.disconnect();
    const url = new URL(this.route('/channel'), location.origin);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('client_id', this.clientID);
    url.searchParams.set('features', 'message_delta,session_busy');
    const socket = new WebSocket(url, 'crux-workspace-channel-v1');
    this.socket = socket;
    return new Promise((resolve, reject) => {
      let opened = false;
      socket.onopen = () => { opened = true; resolve(); };
      socket.onmessage = ({ data }) => {
        try {
          const frame = JSON.parse(data);
          if (frame.type === 'event') onEvent(frame.event);
        } catch { socket.close(1002, 'Invalid workspace event'); }
      };
      socket.onerror = () => { if (!opened) reject(new Error('Could not open the workspace event stream.')); };
      socket.onclose = () => { if (opened) onClose(); else reject(new Error('Workspace event stream closed before connecting.')); };
    });
  }

  async selectSession(id: string) {
    await this.request(this.route('/current-session'), 'POST', { session_id: id, selection_generation: ++this.generation });
  }

  disconnect() {
    if (this.socket) {
      this.socket.onclose = null;
      this.socket.close();
      this.socket = undefined;
    }
  }

  async release() {
    this.disconnect();
    if (this.workspace) await this.request(`/v1/workspaces/${this.workspace.id}`, 'DELETE');
    this.workspace = undefined;
    this.connection = '';
  }
}
