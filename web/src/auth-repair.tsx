import { createSignal, For, onCleanup, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { repairLocalAuthentication, type LocalRepairRequest, type LocalRepairResponse, type LocalRepairSummary } from './auth.ts';
import { Badge, errorText } from './ui.tsx';

export function AuthenticationRepair(props: { client: CruxState; operation?: { workspace: string; id: string } }) {
  const { client } = props;
  const workspaceID = client.state.workspace!.id;
  const connection = client.api.connection;
  const [originalWorkspace, setOriginalWorkspace] = createSignal(props.operation?.workspace ?? workspaceID);
  const [operationID, setOperationID] = createSignal(props.operation?.id ?? '');
  const [result, setResult] = createSignal<LocalRepairResponse['result']>();
  const [reviewed, setReviewed] = createSignal<LocalRepairSummary>();
  const [retry, setRetry] = createSignal<LocalRepairRequest>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [notice, setNotice] = createSignal('');
  const controller = new AbortController();
  let disposed = false;
  const current = () => !disposed && client.api.connection === connection && client.state.workspace?.id === workspaceID && client.api.workspace?.id === workspaceID;
  onCleanup(() => { disposed = true; controller.abort(); });
  const reset = () => { setReviewed(undefined); setRetry(undefined); setResult(undefined); setError(''); setNotice(''); };
  async function submit(action: 'review' | 'apply' | 'abandon', retained?: LocalRepairRequest) {
    if (!current() || busy() || !client.state.connected) return;
    const review = reviewed();
    if (action !== 'review' && !review && !retained) return;
    const request = retained ?? { workspace_id: workspaceID, operation_workspace_id: originalWorkspace().trim(), operation_id: operationID().trim(), revision: action === 'review' ? 0 : review!.revision, apply: action === 'apply', abandon: action === 'abandon' };
    setBusy(true); setError(''); setNotice(''); setReviewed(undefined);
    setRetry(action === 'review' ? undefined : request);
    if (action === 'review') setResult(undefined);
    try {
      const response = await repairLocalAuthentication(client, request, controller.signal);
      if (!current()) return;
      if (response.result.summary?.operation_id) setResult(response.result);
      if (response.error) { setError(response.error.message); return; }
      setRetry(undefined);
      if (action === 'review') { setReviewed(response.result.summary); setNotice('Reviewed saved progress. No files were changed.'); }
      else if (action === 'apply') setNotice('Storage repair completed. Restart the local web host to reload saved settings, then review the active account. This operation did not publish a runtime.');
      else setNotice('Recovery intent abandoned. Existing saved files and original operation progress were preserved.');
    } catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  return <details class="card" open={Boolean(props.operation)}>
    <summary>Authentication storage repair</summary>
    <div class="stack pad">
      <p class="muted small-text">Review a retained authentication operation using its original workspace and operation IDs. Repair finishes its recorded file writes; abandonment retires recovery intent without undoing saved changes. Neither action signs in or publishes a runtime.</p>
      <form class="stack" onSubmit={event => { event.preventDefault(); void submit('review'); }}>
        <label class="field"><span>Original workspace ID</span><input aria-label="Authentication operation workspace" required value={originalWorkspace()} disabled={busy()} onInput={event => { setOriginalWorkspace(event.currentTarget.value); reset(); }} /></label>
        <label class="field"><span>Operation ID</span><input aria-label="Authentication operation ID" required value={operationID()} disabled={busy()} onInput={event => { setOperationID(event.currentTarget.value); reset(); }} /></label>
        <button type="submit" disabled={busy() || !client.state.connected || !originalWorkspace().trim() || !operationID().trim()}>{busy() ? 'Working…' : 'Review authentication operation'}</button>
      </form>
      <Show when={result()}>{value => <>
        <div class="subhead"><strong>{value().summary.provider_id} · {value().summary.action}</strong><Badge>Revision {value().summary.revision}</Badge></div>
        <Show when={value().summary.account_id || value().summary.removed_account_id}><p class="small-text">Account: {value().summary.account_id || value().summary.removed_account_id}</p></Show>
        <div class="section-label">Original operation</div>
        <For each={Object.entries(value().summary.original ?? {})}>{([name, observed]) => <div class="row-inline"><span>{name.replaceAll('_', ' ')}</span><span class="spacer" /><Badge>{observed ? 'Observed' : 'Not recorded'}</Badge></div>}</For>
        <Show when={value().summary.refresh_started && !value().summary.refresh_observed}><p class="warn small-text">The refresh outcome is unknown. Sign in again; repair will not repeat the token exchange.</p></Show>
        <Show when={value().summary.abandoned}><p class="muted small-text">Recovery intent is abandoned.</p></Show>
        <Show when={value().summary.no_effects}><p class="muted small-text">This operation recorded no storage effects.</p></Show>
        <Show when={value().summary.needs_reload}><p class="warn small-text">Restart the local web host to reload saved settings, then review the active account. Original runtime publication is unchanged.</p></Show>
        <details><summary>Recorded storage progress</summary><pre>{JSON.stringify(value(), null, 2)}</pre></details>
      </>}</Show>
      <div class="actions">
        <button disabled={busy() || !client.state.connected || !reviewed()?.repair_ready || reviewed()?.abandoned} onClick={() => void submit('apply')}>Repair saved authentication</button>
        <button disabled={busy() || !client.state.connected || !reviewed() || reviewed()?.abandoned} onClick={() => void submit('abandon')}>Abandon storage recovery</button>
      </div>
      <Show when={retry()}>{request => <button disabled={busy() || !client.state.connected} onClick={() => void submit(request().abandon ? 'abandon' : 'apply', request())}>{request().abandon ? 'Retry storage abandonment' : 'Retry storage repair'}</button>}</Show>
      <Show when={notice()}><p class="small-text" role="status">{notice()}</p></Show>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    </div>
  </details>;
}
