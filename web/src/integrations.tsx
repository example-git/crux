import { createEffect, createSignal, For, on, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Skill } from './types.ts';
import { Markdown } from './conversation.tsx';
import { Badge, errorText, Icon, Modal, Row, Segmented, Switch } from './ui.tsx';

type ServiceInfo = { state?: string; error?: string; tool_count?: number; prompt_count?: number; resource_count?: number; diagnostic_count?: number };
type SkillCatalog = { skills: (Skill & { enabled: boolean })[]; states: { name: string; path: string; state: number; error?: string }[] };
const stateTone = (state = '') => ['connected', 'ready', 'running', 'started'].includes(state) ? 'good' : ['error', 'failed'].includes(state) ? 'bad' : ['disabled', 'stopped'].includes(state) ? 'neutral' : 'warn';

function AddServer(props: { client: CruxState; name?: string; close: () => void }) {
  const initial = props.name ? props.client.state.workspace?.config.mcp?.[props.name] ?? {} : {};
  const workspaceID = props.client.api.workspace?.id;
  const connection = props.client.api.connection;
  const route = props.client.api.route('/config/set');
  let disposed = false;
  createEffect(() => { if (props.client.state.workspace?.id !== workspaceID || (props.client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  onCleanup(() => { disposed = true; });
  const [name, setName] = createSignal(props.name ?? '');
  const [type, setType] = createSignal<'stdio' | 'http' | 'sse'>(initial.type || 'stdio');
  const [command, setCommand] = createSignal<string>(initial.command ?? '');
  const [args, setArgs] = createSignal(initial.args ? JSON.stringify(initial.args) : '');
  const [url, setURL] = createSignal<string>(initial.url ?? '');
  const initialEnvironment = Object.entries(initial.env ?? {}).map(([key, value]) => `${key}=${value}`).join('\n');
  const [env, setEnv] = createSignal(initialEnvironment);
  const [headers, setHeaders] = createSignal(initial.headers ? JSON.stringify(initial.headers, null, 2) : '');
  const [timeout, setTimeoutValue] = createSignal(initial.timeout ? String(initial.timeout) : '');
  const [enabledTools, setEnabledTools] = createSignal<string>((initial.enabled_tools ?? []).join('\n'));
  const [disabledTools, setDisabledTools] = createSignal<string>((initial.disabled_tools ?? []).join('\n'));
  const [disabled, setDisabled] = createSignal(Boolean(initial.disabled));
  const [oauth, setOAuth] = createSignal(Boolean(initial.oauth));
  const [clientID, setClientID] = createSignal<string>(initial.oauth_client_id ?? '');
  const [clientSecret, setClientSecret] = createSignal<string>(initial.oauth_client_secret ?? '');
  const [callbackPort, setCallbackPort] = createSignal(initial.oauth_callback_port ? String(initial.oauth_callback_port) : '');
  const [error, setError] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy() || disposed || props.client.api.workspace?.id !== workspaceID || props.client.api.connection !== connection) return;
    setError('');
    const key = name().trim();
    if (!/^[A-Za-z0-9_-]+$/.test(key)) { setError('Use letters, numbers, dashes, or underscores for the name.'); return; }
    if (!props.name && props.client.state.workspace?.config.mcp?.[key]) { setError('A server with this name already exists. Edit that server instead.'); return; }
    const retainEnvironment = !!props.name && type() === 'stdio' && env() === initialEnvironment;
    const environment: Record<string, string> = retainEnvironment ? { ...initial.env } : {};
    for (const line of (type() === 'stdio' && !retainEnvironment ? env() : '').split('\n').filter(item => item.trim())) {
      const index = line.indexOf('=');
      const variable = line.slice(0, index).trim();
      if (index < 1 || !variable) { setError('Each environment line must use NAME=value.'); return; }
      environment[variable] = line.slice(index + 1);
    }
    let argumentsList: string[] = [];
    if (type() === 'stdio' && args().trim()) {
      try {
        const parsed: unknown = JSON.parse(args());
        if (!Array.isArray(parsed) || parsed.some(item => typeof item !== 'string')) throw new Error();
        argumentsList = parsed;
      } catch { setError('Arguments must be a JSON array of strings.'); return; }
    }
    let headerValues: Record<string, string> | undefined;
    if (type() !== 'stdio' && headers().trim()) {
      try {
        const parsed: unknown = JSON.parse(headers());
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed) || Object.values(parsed).some(value => typeof value !== 'string')) throw new Error();
        headerValues = parsed as Record<string, string>;
      } catch { setError('Headers must be a JSON object with string values.'); return; }
    }
    const seconds = timeout().trim() ? Number(timeout()) : undefined;
    if (seconds !== undefined && (!Number.isSafeInteger(seconds) || seconds <= 0)) { setError('Timeout must be a positive whole number of seconds.'); return; }
    const port = type() === 'http' && oauth() && callbackPort().trim() ? Number(callbackPort()) : undefined;
    if (port !== undefined && (!Number.isSafeInteger(port) || port < 1 || port > 65535)) { setError('OAuth callback port must be between 1 and 65535.'); return; }
    const tools = (value: string) => value.split('\n').map(item => item.trim()).filter(Boolean);
    const value = {
      ...(type() === 'stdio'
        ? { type: 'stdio', command: command().trim(), args: argumentsList, env: Object.keys(environment).length ? environment : undefined }
        : { type: type(), url: url().trim(), headers: headerValues }),
      timeout: seconds,
      disabled: disabled(),
      enabled_tools: tools(enabledTools()),
      disabled_tools: tools(disabledTools()),
      ...(type() === 'http' ? { oauth: oauth(), ...(oauth() ? { oauth_client_id: clientID().trim() || undefined, oauth_client_secret: clientSecret() || undefined, oauth_callback_port: port } : {}) } : {}),
      ...(props.name ? {
        timeout: seconds ?? 0,
        args: type() === 'stdio' ? argumentsList : [],
        env: type() === 'stdio' ? environment : {},
        headers: type() === 'stdio' ? {} : headerValues ?? {},
        oauth: type() === 'http' && oauth(),
        oauth_client_id: type() === 'http' && oauth() ? clientID().trim() : '',
        oauth_client_secret: type() === 'http' && oauth() ? clientSecret() : '',
        oauth_callback_port: port ?? 0,
      } : {}),
    };
    if (type() === 'stdio' && !command().trim()) { setError('A command is required.'); return; }
    if (type() !== 'stdio' && !/^https?:\/\//.test(url().trim())) { setError('Enter an http(s) URL.'); return; }
    setBusy(true);
    try {
      await props.client.api.request(route, 'POST', { scope: 1, key: `mcp.${key}`, value });
      if (disposed || props.client.api.workspace?.id !== workspaceID || props.client.api.connection !== connection) return;
      await props.client.refreshWorkspace();
      if (disposed || props.client.api.workspace?.id !== workspaceID || props.client.api.connection !== connection) return;
      props.client.toast(`MCP server ${key} ${props.name ? 'updated in' : 'added to'} this workspace.`, 'success');
      props.close();
    } catch (reason) { setError(errorText(reason)); } finally { setBusy(false); }
  }
  return <Modal title={props.name ? 'Edit MCP server' : 'Add MCP server'} subtitle="Saved to this workspace's .crux/crux.json." close={props.close} size="small">
    <form class="modal-body stack" onSubmit={save}>
      <label class="field"><span>Name</span><input autofocus aria-label="Server name" disabled={!!props.name} value={name()} onInput={event => setName(event.currentTarget.value)} placeholder="github" /></label>
      <Segmented label="Transport" value={type()} onChange={setType} options={[{ value: 'stdio', label: 'Command' }, { value: 'http', label: 'HTTP' }, { value: 'sse', label: 'SSE' }]} />
      <Show when={type() === 'stdio'} fallback={<label class="field"><span>URL</span><input aria-label="Server URL" value={url()} onInput={event => setURL(event.currentTarget.value)} placeholder="https://example.com/mcp" /></label>}>
        <label class="field"><span>Command</span><input aria-label="Server command" value={command()} onInput={event => setCommand(event.currentTarget.value)} placeholder="npx" /></label>
        <label class="field"><span>Arguments (JSON array)</span><input aria-label="Server arguments" value={args()} onInput={event => setArgs(event.currentTarget.value)} placeholder={'["-y", "@modelcontextprotocol/server-github"]'} /></label>
        <label class="field"><span>Environment</span><textarea aria-label="Server environment" rows={3} value={env()} onInput={event => setEnv(event.currentTarget.value)} placeholder={'KEY=value\nTOKEN=$GITHUB_TOKEN'} /></label>
      </Show>
      <Show when={type() !== 'stdio'}><label class="field"><span>Headers (JSON object)</span><textarea aria-label="Server headers" rows={3} value={headers()} onInput={event => setHeaders(event.currentTarget.value)} placeholder={'{"Authorization": "Bearer $MCP_TOKEN"}'} /></label></Show>
      <Show when={type() === 'http'}>
        <label class="check"><input type="checkbox" checked={oauth()} onChange={event => setOAuth(event.currentTarget.checked)} />Enable OAuth sign-in</label>
        <Show when={oauth()}>
          <label class="field"><span>OAuth client ID (optional)</span><input aria-label="OAuth client ID" value={clientID()} onInput={event => setClientID(event.currentTarget.value)} /></label>
          <label class="field"><span>OAuth client secret (optional)</span><input type="password" autocomplete="off" aria-label="OAuth client secret" value={clientSecret()} onInput={event => setClientSecret(event.currentTarget.value)} /></label>
          <label class="field"><span>OAuth callback port (optional)</span><input type="number" min="1" max="65535" step="1" aria-label="OAuth callback port" value={callbackPort()} onInput={event => setCallbackPort(event.currentTarget.value)} placeholder="Automatic" /></label>
        </Show>
      </Show>
      <details><summary>Connection and tool options</summary><div class="stack">
        <label class="field"><span>Connection timeout (seconds)</span><input type="number" min="1" step="1" aria-label="Server timeout" value={timeout()} onInput={event => setTimeoutValue(event.currentTarget.value)} placeholder="Native default" /></label>
        <label class="field"><span>Enabled tools (one per line; blank allows all)</span><textarea aria-label="Enabled server tools" rows={3} value={enabledTools()} onInput={event => setEnabledTools(event.currentTarget.value)} /></label>
        <label class="field"><span>Disabled tools (one per line)</span><textarea aria-label="Disabled server tools" rows={3} value={disabledTools()} onInput={event => setDisabledTools(event.currentTarget.value)} /></label>
        <label class="check"><input type="checkbox" checked={disabled()} onChange={event => setDisabled(event.currentTarget.checked)} />Save without starting this server</label>
      </div></details>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <div class="actions"><span class="spacer" /><button type="button" onClick={props.close}>Cancel</button><button class="primary" disabled={busy()}>{props.name ? 'Save server' : 'Add server'}</button></div>
    </form>
  </Modal>;
}

type Diagnostic = {
  range: { start: { line: number; character: number }; end: { line: number; character: number } };
  severity?: number; source?: string; code?: string | number; message: string;
};

function DiagnosticsDialog(props: { client: CruxState; name: string; close: () => void }) {
  const workspaceID = props.client.state.workspace?.id;
  const connection = props.client.api.connection;
  const route = props.client.api.route(`/lsps/${encodeURIComponent(props.name)}/diagnostics`);
  const [files, setFiles] = createSignal<Record<string, Diagnostic[]>>({});
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [query, setQuery] = createSignal('');
  let controller: AbortController | undefined;
  createEffect(() => { if (props.client.state.workspace?.id !== workspaceID || (props.client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  onCleanup(() => controller?.abort());
  async function refresh() {
    controller?.abort();
    const request = new AbortController(); controller = request;
    setBusy(true); setError('');
    try {
      const result = await props.client.api.request<Record<string, Diagnostic[]>>(route, 'GET', undefined, request.signal);
      if (!request.signal.aborted) setFiles(result ?? {});
    } catch (reason) { if (!request.signal.aborted) setError(errorText(reason)); }
    finally { if (!request.signal.aborted) setBusy(false); }
  }
  onMount(() => void refresh());
  const severity = (value?: number) => ['Diagnostic', 'Error', 'Warning', 'Information', 'Hint'][value ?? 0] ?? 'Diagnostic';
  const label = (uri: string) => {
    try { const parsed = new URL(uri); return parsed.protocol === 'file:' ? decodeURIComponent(parsed.pathname) : uri; } catch { return uri; }
  };
  const filtered = () => Object.entries(files()).map(([uri, diagnostics]) => [uri, diagnostics.filter(item => `${label(uri)} ${item.message} ${item.source ?? ''} ${item.code ?? ''} ${severity(item.severity)}`.toLowerCase().includes(query().toLowerCase()))] as const).filter(([, diagnostics]) => diagnostics.length);
  const total = () => Object.values(files()).reduce((count, diagnostics) => count + diagnostics.length, 0);
  return <Modal title={`${props.name} diagnostics`} close={props.close} size="large">
    <div class="modal-body stack">
      <div class="actions"><span role="status">{busy() ? 'Loading diagnostics…' : `${total()} diagnostics`}</span><span class="spacer" /><button disabled={busy()} onClick={() => void refresh()}><Icon name="refresh" size={14} />Refresh diagnostics</button></div>
      <input aria-label="Search diagnostics" placeholder="Filter by file, message, severity, or source" value={query()} onInput={event => setQuery(event.currentTarget.value)} />
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <For each={filtered()} fallback={<Show when={!busy() && !error()}><p class="muted">{query() ? 'No matching diagnostics.' : 'No diagnostics.'}</p></Show>}>{([uri, diagnostics]) => <section class="card">
        <header class="card-head"><strong title={uri}>{label(uri)}</strong><span class="spacer" /><Badge>{diagnostics.length}</Badge></header>
        <For each={diagnostics}>{item => <div class="stack">
          <div class="actions wrap"><Badge tone={item.severity === 1 ? 'bad' : item.severity === 2 ? 'warn' : 'neutral'}>{severity(item.severity)}</Badge><code>{item.range.start.line + 1}:{item.range.start.character + 1}–{item.range.end.line + 1}:{item.range.end.character + 1}</code><span class="muted">{item.source}{item.code !== undefined ? ` · ${item.code}` : ''}</span></div>
          <pre class="code-preview">{item.message}</pre>
          <details><summary>Diagnostic details</summary><pre class="code-preview">{JSON.stringify(item, null, 2)}</pre></details>
        </div>}</For>
      </section>}</For>
    </div>
  </Modal>;
}

export function IntegrationsSettings(props: { client: CruxState }) {
  const { client } = props;
  const { api, state } = client;
  const [mcp, setMCP] = createSignal<Record<string, ServiceInfo>>({});
  const [lsps, setLSPs] = createSignal<Record<string, ServiceInfo>>({});
  const [skills, setSkills] = createSignal<(Skill & { enabled?: boolean })[]>([]);
  const [skillStates, setSkillStates] = createSignal<SkillCatalog['states']>([]);
  const [reloadingSkills, setReloadingSkills] = createSignal(false);
  const [filter, setFilter] = createSignal('');
  const [preview, setPreview] = createSignal<{ title: string; text: string; markdown?: boolean }>();
  const [adding, setAdding] = createSignal(false);
  const [editing, setEditing] = createSignal('');
  const [signIn, setSignIn] = createSignal<{ name: string; url: string; error: string }>();
  const [lspPath, setLSPPath] = createSignal('');
  const [diagnostics, setDiagnostics] = createSignal('');
  const [error, setError] = createSignal('');
  const [pendingAuth, setPendingAuth] = createSignal<string[]>([]);
  let auth: AbortController | undefined;
  let refreshGeneration = 0;
  let previewGeneration = 0;
  let attachmentGeneration = 0;
  const captureAttachment = () => {
    const generation = attachmentGeneration;
    const connection = api.connection;
    const id = api.workspace?.id;
    return () => generation === attachmentGeneration && connection === api.connection && id === api.workspace?.id;
  };
  async function refresh() {
    const generation = ++refreshGeneration;
    if (!api.workspace || !state.connected) return;
    const workspaceID = api.workspace?.id;
    const current = captureAttachment();
    const root = api.route('');
    try {
      const [states, servers, list, waiting] = await Promise.all([
        api.request<Record<string, ServiceInfo>>(`${root}/mcp/states`),
        api.request<Record<string, ServiceInfo>>(`${root}/lsps`),
        api.request<SkillCatalog>(`${root}/skills/catalog`, 'POST', { force_reload: false }),
        api.request<{ name: string; url?: string }[]>(`${root}/mcp/pending-auth`),
      ]);
      if (!current() || generation !== refreshGeneration || api.workspace?.id !== workspaceID) return;
      setMCP(states ?? {}); setLSPs(servers ?? {}); setSkills(list.skills ?? []); setSkillStates(list.states ?? []); setPendingAuth((waiting ?? []).map(item => item.name)); setError('');
    } catch (reason) { if (current() && generation === refreshGeneration && api.workspace?.id === workspaceID) setError(errorText(reason)); }
  }
  onMount(() => void refresh());
  createEffect(on(() => [state.servicesRevision, state.skillsRevision, state.workspace?.config.mcp, state.connected], () => void refresh(), { defer: true }));
  createEffect(on(() => [state.workspace?.connection_name, state.workspace?.id], () => {
    attachmentGeneration++;
    refreshGeneration++;
    previewGeneration++;
    auth?.abort();
    setRefreshing([]); setReloadingSkills(false);
    setSignIn(undefined); setMCP({}); setLSPs({}); setSkills([]); setSkillStates([]); setPendingAuth([]); setPreview(undefined);
    if (api.workspace) void refresh();
  }, { defer: true }));
  onCleanup(() => { attachmentGeneration++; refreshGeneration++; previewGeneration++; auth?.abort(); });
  const configured = () => state.workspace?.config.mcp ?? {};
  const servers = () => [...new Set([...Object.keys(configured()), ...Object.keys(mcp())])].map(name => [name, mcp()[name] ?? ({ state: configured()[name]?.disabled ? 'disabled' : 'starting' } as ServiceInfo)] as const);
  const dockerEnabled = () => Boolean(configured().docker);
  async function act(action: () => Promise<unknown>, success?: string) {
    const current = captureAttachment();
    try { await action(); if (!current()) return; if (success) client.toast(success, 'success'); await refresh(); } catch (reason) { if (current()) client.toast(errorText(reason), 'error'); }
  }
  const [refreshing, setRefreshing] = createSignal<string[]>([]);
  async function refreshServer(name: string) {
    if (refreshing().includes(name)) return;
    const workspaceID = api.workspace?.id;
    const current = captureAttachment();
    const root = api.route('/mcp');
    setRefreshing(names => [...names, name]);
    const failures: string[] = [];
    try {
      for (const kind of ['tools', 'prompts', 'resources']) {
        if (!current()) return;
        try {
          await api.request(`${root}/refresh-${kind}`, 'POST', { name });
          if (!current()) return;
          const states = await api.request<Record<string, ServiceInfo>>(`${root}/states`);
          const info = states?.[name];
          if (!info || info.state !== 'connected' || info.error) failures.push(`${kind}: ${info?.error || info?.state || 'Server unavailable'}`);
        } catch (reason) { failures.push(`${kind}: ${errorText(reason)}`); }
      }
      if (!current() || api.workspace?.id !== workspaceID) return;
      await refresh();
      if (!current() || api.workspace?.id !== workspaceID) return;
      client.toast(failures.length ? `${name}: ${failures.join('; ')}` : `${name} refreshed.`, failures.length ? 'error' : 'success');
    } finally { if (current()) setRefreshing(names => names.filter(item => item !== name)); }
  }
  async function previewSkill(skill: Skill) {
    const generation = ++previewGeneration;
    const workspaceID = api.workspace?.id;
    const current = captureAttachment();
    const route = api.route('/skills/read');
    try {
      const result = await api.request<{ content: string }>(route, 'POST', { skill_id: skill.id });
      if (!current() || generation !== previewGeneration || api.workspace?.id !== workspaceID) return;
      setPreview({ title: skill.name, text: new TextDecoder().decode(Uint8Array.from(atob(result.content), character => character.charCodeAt(0))), markdown: true });
    } catch (reason) { if (current() && generation === previewGeneration && api.workspace?.id === workspaceID) client.toast(errorText(reason), 'error'); }
  }
  async function reloadSkills() {
    if (reloadingSkills()) return;
    const workspaceID = api.workspace?.id;
    const current = captureAttachment();
    const route = api.route('/skills/catalog');
    setReloadingSkills(true);
    try {
      const result = await api.request<SkillCatalog>(route, 'POST', { force_reload: true });
      if (!current() || api.workspace?.id !== workspaceID) return;
      refreshGeneration++;
      setSkills(result.skills ?? []); setSkillStates(result.states ?? []);
      client.set('skillsRevision', value => value + 1);
    } catch (reason) { if (current() && api.workspace?.id === workspaceID) client.toast(errorText(reason), 'error'); }
    finally { if (current()) setReloadingSkills(false); }
  }
  async function authenticate(name: string) {
    auth?.abort();
    const controller = new AbortController(); auth = controller;
    const root = api.route('/mcp');
    const workspaceID = api.workspace?.id;
    const current = captureAttachment();
    setSignIn({ name, url: '', error: '' });
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try { const result = await api.request<{ auth_url?: string }>(`${root}/auth-url?name=${encodeURIComponent(name)}`, 'GET', undefined, controller.signal); if (!controller.signal.aborted && current() && result.auth_url) setSignIn(value => value && { ...value, url: result.auth_url! }); } catch {}
      if (!controller.signal.aborted && current()) timer = setTimeout(poll, 750);
    };
    void poll();
    try { await api.request(`${root}/auth`, 'POST', { name }, controller.signal); if (controller.signal.aborted || !current() || api.workspace?.id !== workspaceID) return; client.toast(`Signed in to ${name}.`, 'success'); setSignIn(undefined); await refresh(); }
    catch (reason) { if (!controller.signal.aborted && current()) setSignIn(value => value && { ...value, error: errorText(reason) }); }
    finally { controller.abort(); clearTimeout(timer); }
  }
  return <div class="settings-section">
    <div class="section-head"><div><h2>Integrations</h2><p>MCP servers, language servers, and skills for this workspace.</p></div><button onClick={() => void refresh()}><Icon name="refresh" size={14} />Refresh</button></div>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>

    <div class="subhead"><h3>MCP servers</h3><span class="spacer" /><button class="small" onClick={() => setAdding(true)}><Icon name="plus" size={13} />Add server</button></div>
    <For each={servers()} fallback={<p class="muted small-text">No MCP servers configured.</p>}>{([name, info]) => <div class="service">
      <Icon name="plug" size={15} />
      <div class="service-text"><strong>{name}</strong><small>{info.tool_count ?? 0} tools · {info.prompt_count ?? 0} prompts · {info.resource_count ?? 0} resources</small><Show when={info.error}><small class="bad">{info.error}</small></Show></div>
      <Badge tone={stateTone(info.state)}>{info.state ?? 'unknown'}</Badge>
      <Show when={configured()[name]}><button class="small" aria-label={`Edit ${name}`} onClick={() => setEditing(name)}>Edit</button></Show>
      <Show when={pendingAuth().includes(name) || configured()[name]?.type === 'http' && configured()[name]?.oauth && !configured()[name]?.disabled}><button class="small" aria-label={`Sign in to ${name}`} onClick={() => void authenticate(name)}>{pendingAuth().includes(name) ? 'Sign in required' : 'Sign in'}</button></Show>
      <Show when={configured()[name]}><button class="small" aria-label={`${configured()[name]?.disabled ? 'Enable' : 'Disable'} ${name}`} onClick={() => act(async () => { await api.request(api.route('/config/set'), 'POST', { scope: 1, key: `mcp.${name}.disabled`, value: !configured()[name]?.disabled }); await client.refreshWorkspace(); })}>{configured()[name]?.disabled ? 'Enable' : 'Disable'}</button></Show>
      <button class="ghost icon" title="Refresh tools, prompts and resources" aria-label={`Refresh ${name}`} disabled={refreshing().includes(name)} onClick={() => void refreshServer(name)}><Icon name="refresh" size={14} /></button>
      <Show when={configured()[name] && name !== 'docker'}><button class="ghost icon" aria-label={`Remove ${name}`} onClick={() => { if (confirm(`Remove MCP server “${name}” from this workspace?`)) void act(() => api.request(api.route('/config/remove'), 'POST', { scope: 1, key: `mcp.${name}` }), `${name} removed.`); }}><Icon name="trash" size={14} /></button></Show>
    </div>}</For>
    <Show when={signIn()}>{value => <div class="callout">
      <p role="status">Signing in to <strong>{value().name}</strong>…</p>
      <Show when={value().url}><a class="button primary" href={value().url} target="_blank" rel="noopener noreferrer"><Icon name="external" size={14} />Open sign-in page</a></Show>
      <Show when={value().error}><p class="form-error">{value().error}</p></Show>
      <button onClick={() => { auth?.abort(); setSignIn(undefined); }}>Cancel</button>
    </div>}</Show>
    <Row title="Docker MCP catalog" detail="Adds the Docker MCP Toolkit gateway as a server named docker."><Switch label="Docker MCP catalog" checked={dockerEnabled()} onChange={value => act(() => api.request(api.route(`/mcp/docker/${value ? 'enable' : 'disable'}`), 'POST'), value ? 'Docker MCP enabled.' : 'Docker MCP disabled.')} /></Row>

    <div class="subhead"><h3>Language servers</h3></div>
    <For each={Object.entries(lsps())} fallback={<p class="muted small-text">No language servers are running.</p>}>{([name, info]) => <div class="service">
      <Icon name="file" size={15} />
      <div class="service-text"><strong>{name}</strong><small>{info.diagnostic_count ?? 0} diagnostics</small><Show when={info.error}><small class="bad">{info.error}</small></Show></div>
      <Badge tone={stateTone(info.state)}>{info.state ?? 'unknown'}</Badge>
      <button class="small" onClick={() => setDiagnostics(name)}>Diagnostics</button>
    </div>}</For>
    <form class="inline-form" onSubmit={event => { event.preventDefault(); void act(() => api.request(api.route('/lsps/start'), 'POST', { path: lspPath() }), 'Language server started.'); }}>
      <input aria-label="File path to start a language server" placeholder="File path to start its language server" value={lspPath()} onInput={event => setLSPPath(event.currentTarget.value)} />
      <button disabled={!lspPath().trim()}>Start</button>
      <button type="button" disabled={!Object.keys(lsps()).length} onClick={() => act(() => api.request(api.route('/lsps/stop'), 'POST'), 'Language servers stopped.')}>Stop all</button>
    </form>

    <div class="subhead"><h3>Skills <span class="muted">{skills().length}</span></h3><span class="spacer" /><button disabled={reloadingSkills()} onClick={() => void reloadSkills()}>Reload skills</button></div>
    <For each={skillStates().filter(item => item.state !== 0 || item.error)}>{item => <div class="callout" role="alert"><strong>{item.name || item.path}</strong><p>{item.error || 'Skill discovery failed.'}</p><small>{item.path}</small></div>}</For>
    <div class="search"><Icon name="search" size={14} /><input aria-label="Search skills" placeholder="Search skills" value={filter()} onInput={event => setFilter(event.currentTarget.value)} /></div>
    <div class="skill-grid"><For each={skills().filter(skill => `${skill.name} ${skill.description}`.toLowerCase().includes(filter().toLowerCase()))} fallback={<p class="muted small-text">No skills match.</p>}>{skill => <button class="skill" onClick={() => void previewSkill(skill)}>
      <strong>{skill.name}</strong><small>{skill.description}</small><span class="skill-meta"><Badge>{skill.source}</Badge><Show when={skill.enabled === false}><Badge tone="warn">Disabled</Badge></Show><Show when={skill.user_invocable}><Badge tone="accent">/command</Badge></Show></span>
    </button>}</For></div>

    <Show when={diagnostics()}>{name => <DiagnosticsDialog client={client} name={name()} close={() => setDiagnostics('')} />}</Show>
    <Show when={adding()}><AddServer client={client} close={() => setAdding(false)} /></Show>
    <Show when={editing()} keyed>{name => <AddServer client={client} name={name} close={() => setEditing('')} />}</Show>
    <Show when={preview()}>{value => <Modal title={value().title} close={() => { previewGeneration++; setPreview(undefined); }} size="large"><div class="modal-body"><Show when={value().markdown} fallback={<pre>{value().text}</pre>}><Markdown text={value().text} /></Show></div></Modal>}</Show>
  </div>;
}

export type IndexStatus = {
  enabled: boolean; state: string; serving?: boolean; project_root?: string; database_path?: string; store_directory?: string; source_mode?: string;
  credential_status?: string; model?: string; include_paths?: string[]; exclude_paths?: string[]; configured_database_path?: string; configured_store_directory?: string;
  files_total?: number; files_processed?: number; chunks_created?: number; files_skipped?: number; current_path?: string; stage?: string; finished_at?: string; error?: string; memory_activity?: string;
};

function indexLabel(status: IndexStatus) {
  switch (status.state) {
    case 'disabled': return 'Disabled';
    case 'missing': return 'Index not built';
    case 'indexing': {
      const activity = status.serving ? 'Refreshing' : 'Indexing';
      if (status.files_total) return `${activity}: ${status.files_processed ?? 0}/${status.files_total} files, ${status.chunks_created ?? 0} chunks${status.files_skipped ? `, ${status.files_skipped} skipped` : ''}`;
      return status.stage ? `${status.serving ? `${activity}: ` : ''}${status.stage}…` : `${activity}…`;
    }
    case 'ready': return `Ready${status.files_total ? `: ${status.files_total} files, ${status.chunks_created ?? 0} chunks${status.files_skipped ? `, ${status.files_skipped} skipped` : ''}` : status.chunks_created ? `: ${status.chunks_created} chunks` : ''}`;
    case 'stale': return status.serving ? 'Serving the current index; refresh pending' : 'Index changed; update recommended';
    case 'failed': return status.serving ? 'Serving the current index; refresh failed' : 'Index failed; retry available';
    default: return 'Checking…';
  }
}
const credentialLabel: Record<string, string> = { 'signed-in': 'Signed in', missing: 'Not signed in (sign in to GitHub Codebase Index under Providers)', invalid: 'Credential invalid' };
const splitPaths = (value: string) => value.split(',').map(item => item.trim()).filter(Boolean);

export function IndexSettings(props: { client: CruxState }) {
  const { client } = props;
  const [status, setStatus] = createSignal<IndexStatus>();
  const [form, setForm] = createSignal({ enabled: false, database: '', store: '', include: '', exclude: '' });
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  let timer: ReturnType<typeof setTimeout> | undefined;
  let initialized = false;
  let disposed = false;
  let revision = 0;
  function apply(value: IndexStatus, reset: boolean) {
    setStatus(value); setError(value.error ?? '');
    if (reset || !initialized) {
      initialized = true;
      setForm({ enabled: value.enabled, database: value.configured_database_path ?? '', store: value.configured_store_directory ?? '', include: (value.include_paths ?? []).join(', '), exclude: (value.exclude_paths ?? []).join(', ') });
    }
    clearTimeout(timer);
    if (value.state === 'indexing') timer = setTimeout(() => void load(), 1500);
  }
  async function load() {
    if (disposed || busy() || !client.api.workspace) return;
    const current = ++revision;
    const connection = client.api.connection;
    const workspaceID = client.api.workspace.id;
    const attached = () => !disposed && current === revision && connection === client.api.connection && workspaceID === client.api.workspace?.id;
    try {
      const value = await client.api.request<IndexStatus>(client.api.route('/codebase-index'));
      if (attached()) apply(value, false);
    } catch (reason) { if (attached()) setError(errorText(reason)); }
  }
  async function save(reindex: boolean) {
    if (busy() || disposed || !client.api.workspace) return;
    const current = ++revision;
    const connection = client.api.connection;
    const workspaceID = client.api.workspace.id;
    const attached = () => !disposed && current === revision && connection === client.api.connection && workspaceID === client.api.workspace?.id;
    clearTimeout(timer);
    setBusy(true); setError('');
    try {
      const value = form();
      const saved = await client.api.request<IndexStatus>(client.api.route('/codebase-index'), 'POST', { enabled: value.enabled, reindex, database_path: value.database.trim(), store_directory: value.store.trim(), include_paths: splitPaths(value.include), exclude_paths: splitPaths(value.exclude) });
      if (!attached()) return;
      apply(saved, true);
      client.toast(reindex ? 'Indexing started.' : 'Index settings saved.', 'success');
    } catch (reason) { if (attached()) setError(errorText(reason)); } finally { if (attached()) setBusy(false); }
  }
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id], () => {
    revision++;
    clearTimeout(timer);
    initialized = false;
    setBusy(false); setStatus(undefined); setError('');
    setForm({ enabled: false, database: '', store: '', include: '', exclude: '' });
    if (client.api.workspace) void load();
  }, { defer: true }));
  onMount(() => void load());
  onCleanup(() => { disposed = true; revision++; clearTimeout(timer); });
  const update = (patch: Partial<ReturnType<typeof form>>) => setForm(value => ({ ...value, ...patch }));
  return <div class="settings-section">
    <div class="section-head"><div><h2>Codebase index</h2><p>Semantic search over this project, used automatically by the agent.</p></div><button onClick={() => void load()}><Icon name="refresh" size={14} />Check status</button></div>
    <Show when={status()} fallback={<p class="muted">{error() || 'Loading…'}</p>}>{value => <div class="card">
      <div class="index-status"><span class={`status-dot ${value().state === 'ready' ? 'completed' : value().state === 'failed' ? 'failed' : value().state === 'indexing' ? 'running' : ''}`} /><strong>{indexLabel(value())}</strong></div>
      <Show when={value().state === 'indexing' && value().files_total}><div class="meter"><span style={{ width: `${Math.min(100, (value().files_processed ?? 0) / value().files_total! * 100)}%` }} /></div></Show>
      <dl class="meta">
        <Show when={value().credential_status}><dt>GitHub</dt><dd>{credentialLabel[value().credential_status!] ?? value().credential_status}</dd></Show>
        <Show when={value().source_mode}><dt>Source</dt><dd>{value().source_mode}{value().model ? ` · ${value().model}` : ''}</dd></Show>
        <Show when={value().project_root}><dt>Project</dt><dd>{value().project_root}</dd></Show>
        <Show when={value().database_path}><dt>Database</dt><dd>{value().database_path}</dd></Show>
        <Show when={value().current_path}><dt>Current</dt><dd>{value().current_path}</dd></Show>
        <Show when={value().memory_activity}><dt>Memory</dt><dd>{value().memory_activity}</dd></Show>
      </dl>
    </div>}</Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <Row title="Enable indexing" detail="Saved to this workspace."><Switch label="Enable indexing" checked={form().enabled} onChange={enabled => update({ enabled })} /></Row>
    <label class="field"><span>Include paths</span><input aria-label="Include paths" placeholder="All files (comma-separated)" value={form().include} onInput={event => update({ include: event.currentTarget.value })} /></label>
    <label class="field"><span>Exclude paths</span><input aria-label="Exclude paths" placeholder="None (comma-separated)" value={form().exclude} onInput={event => update({ exclude: event.currentTarget.value })} /></label>
    <details><summary>Storage locations</summary>
      <label class="field"><span>Database</span><input aria-label="Index database path" placeholder="Default" value={form().database} onInput={event => update({ database: event.currentTarget.value })} /></label>
      <label class="field"><span>Store directory</span><input aria-label="Index store directory" placeholder="Default" value={form().store} onInput={event => update({ store: event.currentTarget.value })} /></label>
    </details>
    <div class="actions"><span class="spacer" /><button disabled={busy()} onClick={() => void save(false)}>Save settings</button><button class="primary" disabled={busy() || !form().enabled} onClick={() => void save(true)}>Index now</button></div>
  </div>;
}
