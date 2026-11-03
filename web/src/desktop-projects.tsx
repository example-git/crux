import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from 'solid-js';
import { createStore } from 'solid-js/store';
import type { CruxState } from './state.ts';
import type { Session } from './types.ts';
import { SessionList } from './sidebar.tsx';
import { errorText, Icon } from './ui.tsx';

export type DesktopProject = { path: string; dataDir?: string; connection?: string; expanded: boolean };
export type DesktopProjects = { version: number; pinned: boolean; open: boolean; projects: DesktopProject[] };
type SessionEntry = { id: string; title: string; message_count: number; updated_at: number };
type Catalog = { sessions: SessionEntry[]; more: boolean; checked: number; loading: boolean; error: string };
const projectKey = (project: DesktopProject) => JSON.stringify([project.connection ?? '', project.path, project.dataDir ?? '']);
const projectName = (path: string) => path.split(/[\\/]/).filter(Boolean).pop() || path;

export function DesktopProjectSidebar(props: { client: CruxState; detailsOpen: boolean; toggleDetails: () => void; rename: (session: Session) => void; remove: (session: Session) => void; registerSessions: (open: () => void) => void }) {
  const { client } = props;
  const [saved, setSaved] = createSignal<DesktopProjects>({ version: 1, pinned: true, open: false, projects: [] });
  const [loaded, setLoaded] = createSignal(false);
  const [error, setError] = createSignal('');
  const [query, setQuery] = createSignal('');
  const [opening, setOpening] = createSignal(false);
  const [remoteAdding, setRemoteAdding] = createSignal(false);
  const [connections, setConnections] = createSignal<{ name: string; address: string }[]>([]);
  const [remoteName, setRemoteName] = createSignal('');
  const [remotePath, setRemotePath] = createSignal('');
  const [remoteListing, setRemoteListing] = createSignal<{ roots: string[]; path: string; parent?: string; entries: { name: string; path: string; directory: boolean }[] }>();
  const [remoteBusy, setRemoteBusy] = createSignal(false);
  let browseGeneration = 0;
  const [catalogs, setCatalogs] = createStore<Record<string, Catalog>>({});
  const controllers = new Map<string, AbortController>();
  let disposed = false;
  let refreshing = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const current = (project: DesktopProject) => (client.state.workspace?.connection_name ?? '') === (project.connection ?? '') && client.state.workspace?.path === project.path && (client.state.workspace?.requested_data_dir ?? '') === (project.dataDir ?? '');
  const visible = createMemo(() => saved().projects.filter(project => `${project.connection ?? ''} ${project.path}`.toLowerCase().includes(query().toLowerCase())));

  async function load() {
    try {
      const value = await window.cruxDesktop!.loadProjects();
      if (disposed) return;
      setSaved(value);
      setLoaded(true);
      setError('');
      void refresh();
    } catch (reason) { if (!disposed) setError(errorText(reason)); }
  }
  function update(value: DesktopProjects) {
    setSaved(value);
    void window.cruxDesktop!.saveProjects(value).catch(reason => { if (!disposed) setError(errorText(reason)); });
  }
  function changeProject(project: DesktopProject, patch: Partial<DesktopProject>) {
    update({ ...saved(), projects: saved().projects.map(value => projectKey(value) === projectKey(project) ? { ...value, ...patch } : value) });
  }
  async function read(project: DesktopProject, more = false) {
    const key = projectKey(project);
    if (controllers.has(key)) return;
    const controller = new AbortController();
    controllers.set(key, controller);
    const previous = catalogs[key];
    setCatalogs(key, { sessions: previous?.sessions ?? [], more: previous?.more ?? false, checked: previous?.checked ?? 0, loading: true, error: '' });
    try {
      const params = new URLSearchParams({ path: project.path, data_dir: project.dataDir ?? '', offset: String(more ? previous?.sessions.length ?? 0 : 0) });
      const root = project.connection ? `/ui/connections/${encodeURIComponent(project.connection)}/project-sessions` : '/ui/project-sessions';
      const value = await client.api.request<{ sessions: SessionEntry[]; more: boolean }>(`${root}?${params}`, 'GET', undefined, controller.signal);
      if (controller.signal.aborted) return;
      const sessions = more ? [...(previous?.sessions ?? []), ...value.sessions] : value.sessions;
      setCatalogs(key, { sessions: [...new Map(sessions.map(session => [session.id, session])).values()], more: value.more, checked: Date.now(), loading: false, error: '' });
    } catch (reason) {
      if (!controller.signal.aborted) setCatalogs(key, { loading: false, checked: Date.now(), error: errorText(reason) });
    } finally {
      controllers.delete(key);
      if (!disposed && catalogs[key]) setCatalogs(key, 'loading', false);
    }
  }
  async function refresh(force = false) {
    if (refreshing || !loaded() || !saved().open || document.hidden) return;
    refreshing = true;
    try {
      for (const project of saved().projects) {
        if (disposed || !saved().open) break;
        if (!saved().projects.some(value => projectKey(value) === projectKey(project) && value.expanded)) continue;
        const catalog = catalogs[projectKey(project)];
        if (force || !catalog || Date.now() - catalog.checked >= 30000) await read(project);
      }
    } finally { refreshing = false; }
  }
  async function add() {
    try {
      const path = await window.cruxDesktop!.selectDirectory();
      if (!path || disposed) return;
      const project = { path, expanded: true };
      if (!saved().projects.some(value => projectKey(value) === projectKey(project))) update({ ...saved(), projects: [...saved().projects, project] });
      await read(project);
    } catch (reason) { if (!disposed) setError(errorText(reason)); }
  }
  async function chooseRemote() {
    setRemoteAdding(true); setError('');
    try { setConnections(await client.api.request<{ name: string; address: string }[]>('/ui/connections')); }
    catch (reason) { if (!disposed) setError(errorText(reason)); }
  }
  async function browseRemote(path: string) {
    const name = remoteName();
    const generation = ++browseGeneration;
    if (!name) { setRemoteListing(undefined); setRemoteBusy(false); return; }
    setRemoteBusy(true); setError('');
    try {
      const listing = await client.api.request<NonNullable<ReturnType<typeof remoteListing>>>(`/ui/connections/${encodeURIComponent(name)}/browser?${new URLSearchParams({ path })}`);
      if (!disposed && generation === browseGeneration) { setRemoteListing(listing); setRemotePath(listing.path); }
    } catch (reason) { if (!disposed && generation === browseGeneration) setError(errorText(reason)); }
    finally { if (!disposed && generation === browseGeneration) setRemoteBusy(false); }
  }
  async function addRemote() {
    if (!remoteName() || !remotePath().trim()) return;
    const project: DesktopProject = { connection: remoteName(), path: remotePath().trim(), expanded: true };
    if (!saved().projects.some(value => projectKey(value) === projectKey(project))) update({ ...saved(), projects: [...saved().projects, project] });
    setRemoteAdding(false);
    await read(project);
  }
  async function open(project: DesktopProject, sessionID?: string) {
    if (opening() || client.state.loading) return;
    setOpening(true); setError('');
    try {
      if (!current(project)) await client.switchWorkspace(project.path, project.dataDir ?? '', project.connection ?? '');
      if (sessionID !== undefined) await client.selectSession(sessionID);
      if (!saved().pinned) update({ ...saved(), open: false });
    } catch (reason) { setError(errorText(reason)); }
    finally { setOpening(false); }
  }
  createEffect(on(() => [loaded(), client.state.workspace?.path, client.state.workspace?.requested_data_dir, client.state.workspace?.connection_name] as const, ([ready, path, dataDir, connection]) => {
    if (!ready || !path) return;
    const project: DesktopProject = { path, ...(connection ? { connection } : {}), ...(dataDir ? { dataDir } : {}), expanded: true };
    if (!saved().projects.some(value => projectKey(value) === projectKey(project))) update({ ...saved(), projects: [...saved().projects, project] });
  }));
  createEffect(() => {
    for (const [key, controller] of controllers) {
      if (!saved().open || !saved().projects.some(project => projectKey(project) === key && project.expanded)) controller.abort();
    }
    if (loaded() && saved().open) {
      for (const project of saved().projects) {
        if (project.expanded && !catalogs[projectKey(project)]) void refresh();
      }
    }
  });
  async function openSessions() {
    if (!loaded()) await load();
    if (!loaded() || disposed) return;
    const workspace = client.state.workspace;
    let projects = saved().projects.map(project => current(project) ? { ...project, expanded: true } : project);
    if (workspace && !projects.some(current)) projects = [...projects, { path: workspace.path, dataDir: workspace.requested_data_dir, connection: workspace.connection_name, expanded: true }];
    setQuery('');
    update({ ...saved(), open: true, projects });
    requestAnimationFrame(() => document.querySelector('.project-sidebar [aria-label="Search sessions"]')?.scrollIntoView?.({ block: 'nearest' }));
  }
  onMount(() => {
    props.registerSessions(() => void openSessions());
    void load();
    const poll = () => { void refresh(); timer = setTimeout(poll, 5000); };
    timer = setTimeout(poll, 5000);
  });
  onCleanup(() => { disposed = true; clearTimeout(timer); for (const controller of controllers.values()) controller.abort(); });

  const windowControl = (action: 'minimize' | 'maximize' | 'close') => client.attempt(() => window.cruxDesktop!.windowControl(action));

  return <>
    <header class="desktop-header">
      <button aria-label={saved().open ? 'Collapse projects' : 'Open projects'} aria-expanded={saved().open} title="Projects" onClick={() => loaded() ? update({ ...saved(), open: !saved().open }) : void load()}><Icon name="folder" size={16} /></button>
      <span class="desktop-title">Crux</span><span class="spacer" />
      <button aria-label={props.detailsOpen ? 'Close details' : 'Open details'} aria-expanded={props.detailsOpen} title="Side panel (⌘.)" onClick={props.toggleDetails}><Icon name="panel" size={16} /></button>
      <div class="window-controls">
        <button aria-label="Minimize window" title="Minimize" onClick={() => windowControl('minimize')}><svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor"><path d="M2 7h10" /></svg></button>
        <button aria-label="Maximize or restore window" title="Maximize / restore" onClick={() => windowControl('maximize')}><svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor"><rect x="2.5" y="2.5" width="9" height="9" rx="1" /></svg></button>
        <button class="window-close" aria-label="Close window" title="Close" onClick={() => windowControl('close')}><Icon name="close" size={16} /></button>
      </div>
    </header>
    <Show when={saved().open && !saved().pinned}><button class="project-scrim" aria-label="Close projects" onClick={() => update({ ...saved(), open: false })} /></Show>
    <Show when={saved().open}>
      <aside class="project-sidebar" classList={{ floating: !saved().pinned }} aria-label="Projects">
        <header><strong>Projects</strong><span class="spacer" /><button class="ghost small" aria-pressed={saved().pinned} title={saved().pinned ? 'Unpin projects' : 'Pin projects'} onClick={() => update({ ...saved(), pinned: !saved().pinned })}>{saved().pinned ? 'Unpin' : 'Pin'}</button></header>
        <div class="project-controls"><input aria-label="Find project" placeholder="Find project…" value={query()} onInput={event => setQuery(event.currentTarget.value)} /><button class="ghost icon" aria-label="Add project" title="Add folder" onClick={() => void add()}><Icon name="plus" size={15} /></button><button class="ghost icon" aria-label="Refresh project sessions" onClick={() => void refresh(true)}><Icon name="refresh" size={14} /></button></div>
        <button class="ghost small" onClick={() => void chooseRemote()}>Add remote project</button>
        <Show when={remoteAdding()}><form class="stack" onSubmit={event => { event.preventDefault(); void addRemote(); }}>
          <select aria-label="Saved connection" value={remoteName()} onChange={event => { setRemoteName(event.currentTarget.value); setRemotePath(''); setRemoteListing(undefined); void browseRemote(''); }}><option value="">Choose a connection…</option><For each={connections()}>{connection => <option value={connection.name}>{connection.name} · {connection.address}</option>}</For></select>
          <input aria-label="Remote project directory" placeholder="Remote directory" value={remotePath()} onInput={event => setRemotePath(event.currentTarget.value)} />
          <div class="actions"><button type="button" disabled={!remoteName() || remoteBusy()} onClick={() => void browseRemote(remotePath())}>Browse</button><button type="button" disabled={!remoteName() || remoteBusy()} onClick={() => void browseRemote('')}>Roots</button></div>
          <Show when={remoteListing()}>{listing => <div class="list"><For each={listing().roots}>{root => <button type="button" class="ghost small" disabled={remoteBusy()} onClick={() => void browseRemote(root)}>{root}</button>}</For><Show when={listing().parent}><button type="button" disabled={remoteBusy()} onClick={() => void browseRemote(listing().parent!)}>Parent directory</button></Show><For each={listing().entries.filter(entry => entry.directory)}>{entry => <button type="button" class="project-session" disabled={remoteBusy()} onClick={() => void browseRemote(entry.path)}>{entry.name}</button>}</For></div>}</Show>
          <div class="actions"><button type="button" onClick={() => { browseGeneration++; setRemoteBusy(false); setRemoteAdding(false); }}>Cancel</button><button disabled={!remoteName() || !remotePath().trim() || remoteBusy()}>Add project</button></div>
        </form></Show>
        <Show when={error()}><p class="form-error" role="alert">{error()}<Show when={!loaded()}><button onClick={() => void load()}>Retry</button></Show></p></Show>
        <div class="project-scroll"><For each={visible()}>{project => {
          const key = projectKey(project);
          return <section class="project-item" classList={{ current: current(project) }}>
            <div class="project-heading"><button class="ghost icon" aria-label={`${project.expanded ? 'Collapse' : 'Expand'} ${project.path}`} aria-expanded={project.expanded} onClick={() => { changeProject(project, { expanded: !project.expanded }); if (!project.expanded) void refresh(); }}><Icon name={project.expanded ? 'down' : 'right'} size={13} /></button><button class="project-name" title={project.path} disabled={opening() || client.state.loading} onClick={() => void open(project)}><Icon name="folder" size={14} /><span>{projectName(project.path)}<Show when={project.connection}><small> · {project.connection}</small></Show></span></button><button class="ghost icon project-remove" aria-label={`Remove shortcut ${project.path}`} title="Remove shortcut, keep files and sessions" onClick={() => { controllers.get(key)?.abort(); update({ ...saved(), projects: saved().projects.filter(value => projectKey(value) !== key) }); setCatalogs(key, undefined!); }}><Icon name="close" size={12} /></button></div>
            <Show when={project.expanded}><div class="project-sessions" classList={{ 'active-project-sessions': current(project) }}>
              <Show when={current(project)} fallback={<>
              <For each={catalogs[key]?.sessions ?? []}>{session => <button class="project-session" classList={{ selected: current(project) && client.state.sessionID === session.id }} disabled={opening() || client.state.loading} title={`${session.title || 'Untitled session'} · ${session.message_count} messages`} onClick={() => void open(project, session.id)}><span>{session.title || 'Untitled session'}</span><small>{new Date(session.updated_at * 1000).toLocaleDateString()}</small></button>}</For>
              <Show when={catalogs[key]?.loading}><small class="muted" role="status">Refreshing…</small></Show>
              <Show when={catalogs[key]?.error}><button class="project-session form-error" onClick={() => void read(project)} title={catalogs[key]?.error}>Could not read sessions · Retry</button></Show>
              <Show when={catalogs[key] && !catalogs[key].loading && !catalogs[key].error && !catalogs[key].sessions.length}><small class="muted">No sessions yet</small></Show>
              <Show when={catalogs[key]?.more}><button class="ghost small" disabled={catalogs[key]?.loading} onClick={() => void read(project, true)}>More sessions</button></Show>
              <button class="ghost small" disabled={opening() || client.state.loading} onClick={() => void open(project, '')}><Icon name="plus" size={12} />New conversation</button>
              </>}><SessionList client={client} rename={props.rename} remove={props.remove} close={() => { if (!saved().pinned) update({ ...saved(), open: false }); }} /></Show>
            </div></Show>
          </section>;
        }}</For></div>
      </aside>
    </Show>
    <Show when={!saved().open && error()}><div class="project-load-error" role="alert">{error()}<button onClick={() => void load()}>Retry projects</button></div></Show>
  </>;
}
