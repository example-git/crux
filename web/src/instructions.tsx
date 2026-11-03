import { createSignal, For, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { APIError } from './api.ts';
import type { Session, SessionInstructions } from './types.ts';
import { Badge, errorText } from './ui.tsx';

type InstructionFile = { path: string; scope: string; provider?: string; source?: 'client' | 'remote' };
const instructionKey = (file: InstructionFile) => file.source ? JSON.stringify([file.source, file.path]) : file.path;
type InstructionDocument = { path: string; text: string; revision: string; exists: boolean };

export function InstructionEditors(props: { client: CruxState; saved: () => void }) {
  return <>
    <Show when={JSON.stringify([props.client.state.workspace?.connection_name ?? '', props.client.state.workspace?.id ?? '', props.client.state.sessionID])} keyed>{_sessionKey => <SessionEditor client={props.client} />}</Show>
    <Show when={props.client.state.workspace && JSON.stringify([props.client.state.workspace.connection_name ?? '', props.client.state.workspace.id])} keyed>{_workspaceID => <FileEditor client={props.client} saved={props.saved} />}</Show>
  </>;
}

function SessionEditor(props: { client: CruxState }) {
  const { client } = props;
  const workspaceID = client.state.workspace?.id;
  const connection = client.api.connection;
  const sessionID = client.state.sessionID;
  const route = sessionID ? client.api.route(`/sessions/${sessionID}`) : '';
  const [saved, setSaved] = createSignal<SessionInstructions>();
  const [mode, setMode] = createSignal<SessionInstructions['mode']>('inherit');
  const [text, setText] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  let disposed = false;
  let read: AbortController | undefined;
  onCleanup(() => { disposed = true; read?.abort(); });
  const current = () => !disposed && connection === client.api.connection && workspaceID === client.state.workspace?.id && workspaceID === client.api.workspace?.id && sessionID === client.state.sessionID;
  const dirty = () => Boolean(saved() && (saved()!.mode !== mode() || saved()!.text !== text()));
  const accept = (value: Session) => {
    if (value.id !== sessionID) throw new Error('Instructions belong to a different session.');
    const instructions = value.instructions ?? { mode: 'inherit', text: '', revision: 0 };
    setSaved(instructions); setMode(instructions.mode); setText(instructions.text);
    if (client.state.currentSession?.id === sessionID && (client.state.currentSession.instructions?.revision ?? 0) <= instructions.revision) client.set('currentSession', 'instructions', instructions);
  };
  async function load() {
    if (!sessionID || busy()) return;
    read?.abort(); read = new AbortController();
    setBusy(true); setError('');
    try { const result = await client.api.request<Session>(route, 'GET', undefined, read.signal); if (current()) accept(result); }
    catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  onMount(() => void load());
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!saved() || busy() || !current()) return;
    setBusy(true); setError('');
    try {
      const result = await client.api.request<Session>(route, 'PUT', { instruction_update: { mode: mode(), text: text(), revision: saved()!.revision } });
      if (!current()) return;
      accept(result);
      client.toast('Session instructions saved. They apply to the next model response.', 'success');
    } catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  return <section class="card stack">
    <div class="subhead"><h3>Session instructions</h3><Badge>This conversation only</Badge></div>
    <Show when={sessionID} fallback={<><p class="muted">Create a session to give this conversation its own instructions.</p><button disabled={!client.state.connected || busy()} onClick={() => { setBusy(true); void client.attempt(client.newSession).finally(() => { if (current()) setBusy(false); }); }}>Create session for instructions</button></>}>
      <form class="stack" onSubmit={save}>
        <label class="field"><span>Session instruction mode</span><select aria-label="Session instruction mode" value={mode()} disabled={busy() || !saved()} onChange={event => setMode(event.currentTarget.value as SessionInstructions['mode'])}><option value="inherit">Inherit native instructions</option><option value="append">Add session instructions</option><option value="replace">Replace all system instructions</option></select></label>
        <p class="muted small-text">Replacement sends only the text below as this session's system prompt, without the tooling, provider, project, memory, goal, or plan prompt layers. Existing conversation history and tool permissions are unchanged. Titles and summarization keep their own task prompts. New, branched, and child sessions do not inherit this override.</p>
        <label class="field"><span>Session prompt</span><textarea class="instruction-editor" aria-label="Session prompt" value={text()} disabled={busy() || !saved() || mode() === 'inherit'} placeholder="Write the instructions for this conversation…" onInput={event => setText(event.currentTarget.value)} /></label>
        <Show when={mode() === 'inherit'}><p class="muted small-text">The saved session text is inactive while inheriting native instructions.</p></Show>
        <Show when={saved() && (client.state.currentSession?.instructions?.revision ?? 0) !== saved()!.revision}><p class="warn small-text">Session instructions changed elsewhere. Reload before saving.</p></Show>
        <div class="actions"><button type="submit" class="primary" disabled={busy() || !dirty() || !client.state.connected}>{busy() ? 'Working…' : 'Save session instructions'}</button><button type="button" disabled={busy() || !client.state.connected} onClick={() => void load()}>{dirty() ? 'Discard edits and reload' : 'Reload session instructions'}</button></div>
      </form>
    </Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
  </section>;
}

function FileEditor(props: { client: CruxState; saved: () => void }) {
  const { client } = props;
  const workspaceID = client.state.workspace?.id;
  const connection = client.api.connection;
  const route = new URL(client.api.endpoint(`/ui/workspaces/${encodeURIComponent(workspaceID ?? '')}/instructions`, connection), location.origin).href;
  const fileRoute = (file: InstructionFile) => `${route}?path=${encodeURIComponent(file.path)}${file.source ? `&source=${file.source}` : ''}`;
  const [files, setFiles] = createSignal<InstructionFile[]>([]);
  const [path, setPath] = createSignal('');
  const [document, setDocument] = createSignal<InstructionDocument>();
  const [publicationPending, setPublicationPending] = createSignal(false);
  const [text, setText] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  let disposed = false;
  let read: AbortController | undefined;
  const current = () => !disposed && connection === client.api.connection && workspaceID === client.state.workspace?.id && workspaceID === client.api.workspace?.id;
  const dirty = () => Boolean(document() && document()!.text !== text());
  onCleanup(() => { disposed = true; read?.abort(); });
  async function load(selected = path()) {
    if (busy()) return;
    read?.abort(); read = new AbortController();
    setBusy(true); setError('');
    try {
      const catalog = await client.api.request<InstructionFile[]>(route, 'GET', undefined, read.signal);
      if (!current()) return;
      setFiles(catalog);
      const next = catalog.find(file => instructionKey(file) === selected) || catalog.find(file => file.path === client.state.workspace?.config.options.initialize_as) || catalog[0];
      setPath(next ? instructionKey(next) : ''); setDocument(undefined); setText(''); setPublicationPending(false);
      if (!next) return;
      const result = await client.api.request<InstructionDocument>(fileRoute(next), 'GET', undefined, read.signal);
      if (!current()) return;
      if (result.path !== next.path) throw new Error('Instructions belong to a different file.');
      setDocument(result); setText(result.text);
    } catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  onMount(() => void load());
  async function save(event: SubmitEvent) {
    event.preventDefault();
    const source = document();
    const file = files().find(item => instructionKey(item) === path());
    if (!source || !file || busy() || !current()) return;
    setBusy(true); setError('');
    try {
      const result = await client.api.request<InstructionDocument>(fileRoute(file), 'PUT', { text: text(), revision: source.revision });
      if (!current()) return;
      if (result.path !== source.path) throw new Error('The saved instructions belong to a different file.');
      setDocument(result); setText(result.text); setPublicationPending(false); props.saved();
      client.toast('Native instruction file saved. Included files are reread for the next model response.', 'success');
    } catch (reason) {
      if (!current()) return;
      const saved = reason instanceof APIError ? reason.payload?.saved_document : undefined;
      if (saved?.path === source.path && typeof saved.text === 'string' && typeof saved.revision === 'string' && saved.exists === true) {
        setDocument(saved); setPublicationPending(true);
      }
      setError(errorText(reason));
    }
    finally { if (current()) setBusy(false); }
  }
  return <section class="card stack">
    <div class="subhead"><h3>Native instruction files</h3><Badge>Shared files</Badge></div>
    <p class="muted small-text">Edit the files used by native Crux. Project and global files follow the instruction-source settings below. Provider files apply when that provider is selected. Session replacement overrides all of these for its conversation.</p>
    <form class="stack" onSubmit={save}>
      <label class="field"><span>Instruction file</span><select aria-label="Instruction file" value={path()} disabled={busy() || dirty()} onChange={event => void load(event.currentTarget.value)}><For each={files()}>{file => <option value={instructionKey(file)}>{file.source ? `${file.source} · ` : ''}{file.scope}{file.provider ? ` · ${file.provider}` : ''} · {file.path}</option>}</For></select></label>
      <Show when={document()}>{source => <><p class="muted small-text">{source().path}<Show when={!source().exists}> · Created when saved</Show></p><label class="field"><span>Native instructions</span><textarea class="instruction-editor" aria-label="Native instructions" value={text()} disabled={busy()} onInput={event => setText(event.currentTarget.value)} /></label></>}</Show>
      <div class="actions"><button class="primary" type="submit" disabled={busy() || (!dirty() && !publicationPending()) || !client.state.connected}>{busy() ? 'Working…' : publicationPending() && !dirty() ? 'Retry instruction publication' : 'Save native instructions'}</button><button type="button" disabled={busy() || !client.state.connected} onClick={() => void load()}>{dirty() ? 'Discard edits and reload file' : 'Reload instruction files'}</button></div>
    </form>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
  </section>;
}
