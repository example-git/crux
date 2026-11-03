import { createEffect, createSignal, For, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { errorText, Icon, Modal, Row, Switch } from './ui.tsx';

type Target = { path: string; label?: string; recursive: boolean; censor_images: boolean };
type Policy = { scope: { kind: 'session'; session_id: string }; settings: { enabled: boolean; inherit?: boolean; censor_images?: boolean; targets: Target[] }; revision: number };

const validLabel = (label: string) => !label || (label !== '.' && label !== '..' && !/[\\/]/.test(label) && !label.includes('pt_') && label.trim() === label && !/\p{Cc}/u.test(label));

export function PathTranslation(props: { client: CruxState; close: () => void }) {
  const { client } = props;
  const sessionID = client.state.sessionID;
  const workspaceID = client.state.workspace?.id;
  const policyRoute = client.api.route('/path-translation');
  const connection = client.api.connection;
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && client.api.connection === connection && client.api.workspace?.id === workspaceID && client.state.sessionID === sessionID;
  createEffect(() => { if (client.state.sessionID !== sessionID || client.state.workspace?.id !== workspaceID || (client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  const [policy, setPolicy] = createSignal<Policy>();
  const [dirty, setDirty] = createSignal(false);
  const [path, setPath] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [notice, setNotice] = createSignal('');
  const route = () => `${policyRoute}?kind=session&session_id=${encodeURIComponent(sessionID)}`;
  async function load() {
    if (!current() || busy()) return;
    setBusy(true); setError('');
    try { const value = await client.api.request<Policy>(route()); if (!current()) return; setPolicy({ ...value, settings: { ...value.settings, targets: value.settings.targets ?? [] } }); setDirty(false); }
    catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  onMount(() => void load());
  const settings = () => policy()?.settings;
  const censor = () => settings()?.censor_images ?? (settings()?.targets ?? []).every(target => target.censor_images);
  const edit = (change: (value: Policy['settings']) => Policy['settings']) => { setPolicy(value => value && { ...value, settings: { ...change(value.settings), inherit: false } }); setDirty(true); setNotice(''); };
  function addTarget(value: string) {
    const clean = value.trim().replace(/[\\/]+$/, '') || value.trim();
    if (!clean.startsWith('/') && !/^[A-Za-z]:[\\/]/.test(clean)) { setError('Enter an absolute path.'); return; }
    if (clean === '/') { setError('The filesystem root has no name to translate. Choose a file or folder.'); return; }
    if (settings()?.targets.some(target => target.path === clean)) { setError('That path is already listed.'); return; }
    setError('');
    edit(value => ({ ...value, targets: [...value.targets, { path: clean, recursive: true, censor_images: censor() }] }));
    setPath('');
  }
  async function save() {
    const value = policy();
    if (!value || !current() || busy()) return;
    const labels = value.settings.targets.map(target => target.label ?? '').filter(Boolean);
    if (labels.some(label => !validLabel(label))) { setError('Folder labels must be a single name without slashes, control characters, outer spaces, or the reserved pt_ prefix.'); return; }
    if (new Set(labels).size !== labels.length) { setError('Folder labels must be unique.'); return; }
    setBusy(true); setError('');
    try {
      const saved = await client.api.request<Policy>(policyRoute, 'POST', value);
      if (!current()) return;
      setPolicy({ ...saved, settings: { ...saved.settings, targets: saved.settings.targets ?? [] } }); setDirty(false); setNotice('Saved. Applies to new turns in this session.');
    } catch (reason) { if (current()) setError(`${errorText(reason)} Reload to see the current settings.`); }
    finally { if (current()) setBusy(false); }
  }
  return <Modal title="Path privacy" subtitle="Replace real paths with stable aliases before they reach the model. Applies to this session." close={props.close} size="medium">
    <div class="modal-body stack">
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <Show when={notice()}><p class="form-note" role="status">{notice()}</p></Show>
      <Show when={settings()} fallback={<p class="muted">{busy() ? 'Loading…' : 'Settings are unavailable.'}</p>}>
        <Row title="Translate paths" detail={settings()?.inherit ? 'Currently inherited. Changing a setting creates a session override.' : 'Listed folders and files get aliases in prompts, tool output, and images.'}><Switch label="Translate paths" checked={Boolean(settings()?.enabled)} onChange={value => edit(current => ({ ...current, enabled: value }))} /></Row>
        <Row title="Censor image copies" detail="Images sent to the model hide translated paths."><Switch label="Censor image copies" checked={censor()} onChange={value => edit(current => ({ ...current, censor_images: value, targets: current.targets.map(target => ({ ...target, censor_images: value })) }))} /></Row>
        <div class="section-label">Paths</div>
        <For each={settings()?.targets} fallback={<p class="muted small-text">No paths yet.</p>}>{(target, index) => <div class="target-row">
          <div class="target-path" title={target.path}><Icon name="folder" size={14} />{target.path}</div>
          <input aria-label={`Label for ${target.path}`} placeholder="Automatic label" value={target.label ?? ''} onInput={event => { const label = event.currentTarget.value; edit(current => ({ ...current, targets: current.targets.map((item, i) => i === index() ? { ...item, label: label || undefined } : item) })); }} />
          <label class="check"><input type="checkbox" checked={target.recursive} onChange={event => { const recursive = event.currentTarget.checked; edit(current => ({ ...current, targets: current.targets.map((item, i) => i === index() ? { ...item, recursive } : item) })); }} />Subfolders</label>
          <button class="ghost icon" aria-label={`Remove ${target.path}`} onClick={() => edit(current => ({ ...current, targets: current.targets.filter((_, i) => i !== index()) }))}><Icon name="trash" size={14} /></button>
        </div>}</For>
        <form class="inline-form" onSubmit={event => { event.preventDefault(); addTarget(path()); }}>
          <input aria-label="Path to translate" placeholder="/absolute/path/to/folder" value={path()} onInput={event => setPath(event.currentTarget.value)} />
          <Show when={window.cruxDesktop}><button type="button" onClick={() => client.attempt(async () => { const chosen = await window.cruxDesktop!.selectDirectory(); if (chosen) addTarget(chosen); })}><Icon name="folder" size={14} />Choose…</button></Show>
          <button disabled={!path().trim()}>Add</button>
        </form>
      </Show>
    </div>
    <footer class="modal-foot"><button onClick={() => void load()} disabled={busy()}>Reload</button><button disabled={busy() || !policy() || settings()?.inherit} onClick={() => { setPolicy(value => value && { ...value, settings: { inherit: true, enabled: false, targets: [] } }); setDirty(true); setNotice(''); setError(''); }}>Use inherited settings</button><span class="spacer" /><button onClick={props.close}>Close</button><button class="primary" disabled={busy() || !dirty()} onClick={() => void save()}>{busy() ? 'Saving…' : 'Save'}</button></footer>
  </Modal>;
}
