import { createEffect, createMemo, createSignal, For, on, onCleanup, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Provider } from './types.ts';
import {
  abandonRecordedLogin, createLogin, describeAuthError, listRecordedLogins, operationID, providerUsage, validateAuthReceipt,
  type Accounts, type AuthSnapshot, type LoginState, type ProviderStatus, type RecordedResult, type Target,
} from './auth.ts';
import { Badge, errorText, Icon, Switch } from './ui.tsx';
import { AuthenticationRepair } from './auth-repair.tsx';

const phaseText: Record<string, string> = {
  preparing: 'Preparing sign-in…', 'waiting-for-loopback': 'Opening a local callback listener…', 'waiting-for-browser': 'Finish signing in on the provider page.',
  'waiting-for-code': 'Paste the code the provider shows after you sign in.', 'waiting-for-device': 'Enter this code on the provider page.',
  authorizing: 'Exchanging the authorization…', authorized: 'Authorized. Saving the account…', committing: 'Saving the account…', complete: 'Signed in.',
};

function OAuthFlow(props: { client: CruxState; target: Target; recovery?: RecordedResult; done: () => void; close: () => void; recordOperation: (workspace: string, id: string) => void }) {
  const [status, setStatus] = createSignal<LoginState>();
  const [error, setError] = createSignal('');
  const [retry, setRetry] = createSignal(false);
  const [code, setCode] = createSignal('');
  const [submitting, setSubmitting] = createSignal(false);
  const [copied, setCopied] = createSignal(false);
  let loginOperationID = '';
  const flow = createLogin(props.client, props.target, { status: setStatus, error: message => { setError(message); if (message) props.recordOperation(props.target.workspace_id, loginOperationID); }, retryAvailable: setRetry, done: props.done, closed: props.close }, props.recovery);
  loginOperationID = flow.login.operation_id;
  onCleanup(() => flow.dispose());
  const expires = () => status()?.expires_at ? new Date(status()!.expires_at!).toLocaleTimeString() : '';
  return <div class="oauth">
    <div class="oauth-status"><Show when={!error()} fallback={<Icon name="warning" size={16} />}><span class="spinner" /></Show><span role="status">{props.recovery ? 'Recovering a recorded sign-in. ' : ''}{phaseText[status()?.phase ?? 'preparing'] ?? status()?.phase}</span></div>
    <Show when={status()?.user_code}><div class="device-code">
      <code>{status()!.user_code}</code>
      <button class="ghost small" onClick={() => void navigator.clipboard.writeText(status()!.user_code!).then(() => setCopied(true))}><Icon name={copied() ? 'check' : 'copy'} size={13} />{copied() ? 'Copied' : 'Copy code'}</button>
    </div></Show>
    <Show when={status()?.authorization_url}><a class="button primary" href={status()!.authorization_url} target="_blank" rel="noopener noreferrer"><Icon name="external" size={14} />{status()?.user_code ? 'Open verification page' : 'Open provider sign-in'}</a></Show>
    <Show when={status()?.phase === 'waiting-for-code'}><form class="inline-form" onSubmit={event => {
      event.preventDefault();
      setSubmitting(true); setError('');
      void flow.submitCode(code()).then(() => setCode('')).catch(reason => setError(describeAuthError(reason))).finally(() => setSubmitting(false));
    }}><input type="password" autocomplete="off" aria-label="Authorization code" placeholder="Authorization code or callback URL" value={code()} onInput={event => setCode(event.currentTarget.value)} /><button class="primary" disabled={submitting() || !code()}>Submit</button></form></Show>
    <Show when={expires()}><p class="muted small-text">Expires at {expires()}.</p></Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <div class="actions"><Show when={retry()}><button onClick={() => void flow.retry()}>Retry saving</button></Show><span class="spacer" /><button onClick={() => void flow.cancel()}>Cancel sign-in</button></div>
  </div>;
}

export function Usage(props: { client: CruxState; owner: Record<string, any>; compact?: boolean }) {
  const usage = () => providerUsage(props.client, props.owner);
  const remaining = (percent: number) => Math.max(0, Math.min(100, 100 - percent));
  return <div class="usage" classList={{ 'usage-compact': props.compact }}>
    <Show when={!props.compact}><div class="section-label">Usage</div></Show>
    <Show when={!usage()}><p class="muted small-text" role="status">No account usage data is available for this provider and account.</p></Show>
    <Show when={usage()}>{value => <>
      <Show when={!props.compact && value().plan}><p class="small-text">Plan: <strong>{value().plan}</strong></p></Show>
      <For each={value().windows} fallback={<p class="muted small-text">The provider reported no usage windows.</p>}>{window => <div class="usage-window">
        <span>{window.name}</span>
        <div class="meter" classList={{ warn: remaining(window.percent) <= 25, bad: remaining(window.percent) <= 10 }}><span style={{ width: `${remaining(window.percent)}%` }} /></div>
        <span>{remaining(window.percent)}% {props.compact ? 'left' : 'remaining'}</span>
        <Show when={!props.compact && window.resets_at && !window.resets_at.startsWith('0001')}><small class="muted">resets {new Date(window.resets_at!).toLocaleString()}</small></Show>
      </div>}</For>
    </>}</Show>
  </div>;
}

function ProviderCard(props: { client: CruxState; provider: Provider; status?: ProviderStatus; snapshot?: AuthSnapshot; refresh: () => Promise<void>; recordOperation: (workspace: string, id: string) => void }) {
  const { client } = props;
  const workspaceID = client.state.workspace?.id;
  const root = client.api.route('');
  const connection = client.api.connection;
  let disposed = false;
  const current = () => !disposed && connection === client.api.connection && workspaceID === client.state.workspace?.id && workspaceID === client.api.workspace?.id;
  onCleanup(() => { disposed = true; });
  const [open, setOpen] = createSignal(false);
  const [accounts, setAccounts] = createSignal<Accounts>();
  const [recorded, setRecorded] = createSignal<RecordedResult[]>([]);
  const [login, setLogin] = createSignal<{ target: Target; recovery?: RecordedResult }>();
  const [slot, setSlot] = createSignal('');
  const [secret, setSecret] = createSignal('');
  const [refreshScope, setRefreshScope] = createSignal('0');
  const [refreshNotice, setRefreshNotice] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const target = (): Target | undefined => accounts()?.target ?? (props.snapshot && props.status ? { workspace_id: props.snapshot.workspace_id, generation: props.snapshot.generation, owner: props.status.owner } : undefined);
  const oauth = () => props.provider.authentication?.find(item => item.kind === 'oauth' || item.adapter);
  const disabled = () => Boolean(client.state.workspace?.config.providers?.[props.provider.id]?.disable);
  const credentialSummary = () => props.status?.credentials.filter(item => item.state !== 'absent').map(item => `${item.kind}: ${item.state}`).join(' · ');
  async function run(action: () => Promise<unknown>, success?: string) {
    if (!current() || !client.state.connected) return false;
    setBusy(true); setError('');
    try { await action(); if (!current()) return false; if (success) client.toast(success, 'success'); return true; }
    catch (reason) { if (current()) setError(describeAuthError(reason)); return false; }
    finally { if (current()) setBusy(false); }
  }
  async function inspect() {
    const value = target();
    if (!value) return;
    setSlot(props.status?.credential_slots?.[0]?.id ?? '');
    await run(async () => {
      const result = await client.api.request<Accounts>(`${root}/auth/accounts`, 'POST', value);
      if (!current()) return;
      setAccounts(result); setRecorded([]);
      if (props.status?.owner.has_oauth) {
        const entries = await listRecordedLogins(client, result.target);
        if (current()) setRecorded(entries);
      }
    });
  }
  async function reload() {
    if (!current()) return;
    await props.refresh();
    if (!current()) return;
    await client.refreshWorkspace();
    if (!current()) return;
    setAccounts(undefined);
    if (open()) await inspect();
  }
  async function mutation(path: string, extra: Record<string, unknown> = {}, expectAccount?: string) {
    const body = { operation_id: operationID(), target: accounts()!.target, ...extra };
    try {
      const receipt = await client.api.request<any>(`${root}${path}`, 'POST', body);
      validateAuthReceipt(receipt, { operation_id: body.operation_id, target: body.target, account_id: expectAccount }, Boolean(connection));
    } catch (reason) { if (current()) props.recordOperation(body.target.workspace_id, body.operation_id); throw reason; }
    await reload();
  }
  async function refreshToken() {
    const owner = props.status?.owner;
    if (busy() || !owner || !current()) return;
    setRefreshNotice('');
    await run(async () => {
      await client.api.request(`${root}/config/refresh-oauth`, 'POST', { scope: Number(refreshScope()), owner });
      if (!current()) return;
      setRefreshNotice('OAuth token refreshed and saved.');
      try { await reload(); }
      catch (reason) { throw new Error(`Token refreshed, but the account view could not be refreshed: ${errorText(reason)}`); }
    });
  }
  async function saveKey(event: SubmitEvent) {
    event.preventDefault();
    const request = { check_id: operationID(), target: target()!, credential_id: slot(), source: secret() };
    await run(async () => {
      const checked = await client.api.request<any>(`${root}/auth/api-key/check`, 'POST', request);
      if (!current()) return;
      const outcome = checked.outcome;
      if (checked.error || !outcome?.checked_target || outcome.check_id !== request.check_id || outcome.credential_id !== request.credential_id) throw new Error(checked.error?.message || 'The credential check returned no matching save target.');
      setSecret('');
      const body = { operation_id: operationID(), target: outcome.checked_target, check_id: outcome.check_id };
      try {
        const receipt = await client.api.request<any>(`${root}/auth/api-key/save`, 'POST', body);
        validateAuthReceipt(receipt, body, Boolean(connection));
      } catch (reason) { if (current()) props.recordOperation(body.target.workspace_id, body.operation_id); throw reason; }
      await reload();
    }, 'Credential saved and applied.');
  }
  const accent = () => props.provider.brand?.color;
  return <section class="provider" classList={{ open: open() }}>
    <header>
      <span class="monogram" style={accent() ? { background: accent() } : undefined}>{(props.provider.brand?.short_name || props.provider.name).slice(0, 2)}</span>
      <div class="provider-text"><strong>{props.provider.name}</strong><small>{props.provider.models?.length ?? 0} models{credentialSummary() ? ` · ${credentialSummary()}` : props.status?.configured ? '' : ' · not signed in'}</small></div>
      <Show when={disabled()} fallback={<Badge tone={props.provider.available ? 'good' : 'warn'}>{props.provider.available ? 'Ready' : props.provider.availability || 'Unavailable'}</Badge>}><Badge>Disabled</Badge></Show>
      <button disabled={!props.status} onClick={() => { const next = !open(); setOpen(next); if (next) void inspect(); }}>{open() ? 'Close' : 'Manage'}</button>
    </header>
    <Show when={props.provider.diagnostic}><p class="warn small-text">{props.provider.diagnostic}</p></Show>
    <Show when={open() && props.status}><div class="provider-body">
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <Show when={login()} fallback={<>
        <div class="row-inline">
          <span>Use this provider</span><span class="spacer" />
          <Switch label={`Enable ${props.provider.name}`} checked={!disabled()} disabled={busy() || !props.provider.owner} onChange={value => void run(async () => { await client.api.request(client.api.route('/config/set'), 'POST', { scope: 0, key: `providers.${props.provider.id}.disable`, value: !value, owner: props.provider.owner }); await client.refreshWorkspace(); }, value ? `${props.provider.name} enabled.` : `${props.provider.name} disabled.`)} />
        </div>
        <Show when={props.status?.owner.has_oauth}><div class="actions wrap">
          <button class="primary" disabled={busy() || !target() || oauth()?.available === false} title={oauth()?.diagnostic} onClick={() => setLogin({ target: target()! })}><Icon name="key" size={14} />Sign in{oauth()?.adapter === 'device-code' ? ' with a device code' : oauth()?.adapter === 'hosted-paste' ? ' (paste code)' : ''}</button>
          <Show when={props.provider.id === 'copilot' && props.provider.owner}><button disabled={busy()} onClick={() => void run(async () => { const result = await client.api.request<{ success: boolean }>(client.api.route('/config/import-copilot'), 'POST', { owner: props.provider.owner }); if (!result.success) throw new Error('No VS Code Copilot sign-in was found on this machine.'); await reload(); }, 'Copilot sign-in imported.')}>Import from VS Code</button></Show>
          <Show when={accounts()?.accounts.some(account => account.active)}><button disabled={busy()} onClick={() => void run(() => mutation('/auth/logout'), 'Signed out.')}>Sign out</button></Show>
        </div></Show>
        <Show when={oauth()?.diagnostic}><p class="muted small-text">{oauth()!.diagnostic}</p></Show>
        <Show when={props.status?.owner.has_oauth}><details><summary>Token maintenance</summary><div class="stack">
          <p class="muted small-text">Refresh the configured OAuth token through this provider. Native storage and refresh rules apply; this does not start a new sign-in.</p>
          <label class="field"><span>Token refresh scope</span><select aria-label="Token refresh scope" disabled={busy()} value={refreshScope()} onChange={event => setRefreshScope(event.currentTarget.value)}><option value="0">Global</option><option value="1">Workspace</option></select></label>
          <button disabled={busy() || !client.state.connected} onClick={() => void refreshToken()}>Refresh OAuth token</button>
          <Show when={refreshNotice()}><p class="muted small-text" role="status">{refreshNotice()}</p></Show>
        </div></details></Show>
        <div class="section-label">Accounts</div>
        <For each={accounts()?.accounts} fallback={<p class="muted small-text">{busy() ? 'Loading…' : 'No saved accounts.'}</p>}>{account => <div class="account">
          <Icon name="user" size={14} />
          <div><strong>{account.display_name || account.id}</strong><small>{account.credential_state}{account.expires_at && account.expires_at > 0 ? ` · expires ${new Date(account.expires_at).toLocaleDateString()}` : ''}</small></div>
          <span class="spacer" />
          <Show when={account.active} fallback={<button class="small" disabled={busy()} onClick={() => void run(() => mutation('/auth/switch', { account_id: account.id }, account.id), 'Account switched.')}>Use</button>}><Badge tone="good">Active</Badge></Show>
          <button class="ghost icon" aria-label={`Remove ${account.display_name || account.id}`} disabled={busy()} onClick={() => { if (confirm(`Remove the saved account “${account.display_name || account.id}”?`)) void run(() => mutation('/auth/remove', { account_id: account.id }), 'Account removed.'); }}><Icon name="trash" size={14} /></button>
        </div>}</For>
        <Show when={recorded().filter(item => !item.abandoned).length}><div class="section-label">Interrupted sign-ins</div>
          <p class="muted small-text">Recorded sign-ins can be recovered through the native authentication service or abandoned.</p>
          <For each={recorded().filter(item => !item.abandoned)}>{item => <div class="account">
            <Icon name="clock" size={14} /><div><strong>{({ 'not-started': 'Exchange not started', 'exchange-outcome-unknown': 'Exchange outcome unknown', 'token-result-recorded': 'Authorization recorded' } as Record<string, string>)[item.state] ?? item.state}</strong><small>{item.operation_id.slice(0, 12)}</small></div><span class="spacer" />
            <button class="small" disabled={busy()} onClick={() => setLogin({ target: accounts()!.target, recovery: item })}>Recover</button>
            <button class="small" disabled={busy()} onClick={() => void run(async () => { await abandonRecordedLogin(client, accounts()!.target, item); await reload(); }, 'Interrupted sign-in abandoned.')}>Abandon</button>
          </div>}</For>
        </Show>
        <Show when={props.status?.credential_slots?.length}><form class="stack key-form" onSubmit={saveKey}>
          <div class="section-label">API key</div>
          <Show when={(props.status?.credential_slots?.length ?? 0) > 1}><select aria-label={`${props.provider.name} credential slot`} value={slot()} onChange={event => setSlot(event.currentTarget.value)}><For each={props.status?.credential_slots}>{item => <option value={item.id}>{item.id} ({item.kind}){item.configured ? ' · configured' : ''}</option>}</For></select></Show>
          <div class="inline-form"><input type="password" autocomplete="off" aria-label={`${props.provider.name} API key`} placeholder={props.status?.credential_slots?.find(item => item.id === slot())?.configured ? 'Replace the saved key' : 'Paste an API key or $ENV_VAR'} value={secret()} onInput={event => setSecret(event.currentTarget.value)} /><button class="primary" disabled={busy() || !secret().trim() || !slot()}>Check & save</button></div>
        </form></Show>
        <Show when={props.provider.usage_available && props.provider.owner && props.status?.configured}><Usage client={client} owner={props.provider.owner!} /></Show>
      </>}>
        {value => <OAuthFlow client={client} target={value().target} recovery={value().recovery} recordOperation={props.recordOperation} close={() => setLogin(undefined)} done={() => { setLogin(undefined); client.toast(`Signed in to ${props.provider.name}.`, 'success'); void reload().catch(reason => setError(errorText(reason))); }} />}
      </Show>
    </div></Show>
  </section>;
}

export function ProvidersSettings(props: { client: CruxState }) {
  const { client } = props;
  const [snapshot, setSnapshot] = createSignal<AuthSnapshot>();
  const [showDisabled, setShowDisabled] = createSignal(false);
  const [repairOperation, setRepairOperation] = createSignal<{ workspace: string; id: string }>();
  const recordOperation = (workspace: string, id: string) => { if (workspace === client.state.workspace?.id && workspace === client.api.workspace?.id) setRepairOperation({ workspace, id }); };
  const [query, setQuery] = createSignal('');
  const [error, setError] = createSignal('');
  let controller: AbortController | undefined;
  const refresh = async () => {
    controller?.abort();
    const request = new AbortController(); controller = request;
    const workspaceID = client.state.workspace?.id;
    const connection = client.api.connection;
    const current = () => !request.signal.aborted && connection === client.api.connection && workspaceID === client.state.workspace?.id;
    if (!workspaceID || !client.state.connected) return;
    try {
      const value = await client.api.request<AuthSnapshot>(client.api.route('/auth'), 'GET', undefined, request.signal);
      if (!current()) return;
      if (value.workspace_id !== workspaceID) throw new Error('Authentication status belongs to a different workspace.');
      setSnapshot(value); setError('');
    } catch (reason) { if (current()) setError(errorText(reason)); }
  };
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id, client.state.connected], () => {
    setSnapshot(undefined); setError('');
    void refresh();
  }));
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id], () => setRepairOperation(undefined)));
  onCleanup(() => controller?.abort());
  const providers = createMemo(() => (client.state.workspace?.provider_surfaces ?? []).filter(provider => (showDisabled() || !client.state.workspace?.config.providers?.[provider.id]?.disable) && provider.name.toLowerCase().includes(query().toLowerCase())));
  return <div class="settings-section">
    <div class="section-head"><div><h2>Providers & accounts</h2><p>Credentials stay in the native Crux account stores on this machine.</p></div><button onClick={() => void refresh()}><Icon name="refresh" size={14} />Refresh</button></div>
    <div class="search"><Icon name="search" size={14} /><input aria-label="Search providers" placeholder="Search providers" value={query()} onInput={event => setQuery(event.currentTarget.value)} /></div>
    <label class="check"><input type="checkbox" checked={showDisabled()} onChange={event => setShowDisabled(event.currentTarget.checked)} />Show disabled providers</label>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <Show when={client.state.workspace && JSON.stringify([client.state.workspace.connection_name ?? '', client.state.workspace.id])} keyed>{_workspaceID => <For each={providers()} fallback={<p class="muted">No providers match.</p>}>{provider => <ProviderCard client={client} provider={provider} snapshot={snapshot()} status={snapshot()?.providers.find(item => item.owner.provider_id === provider.id)} refresh={refresh} recordOperation={recordOperation} />}</For>}</Show>
    <Show when={client.state.workspace?.provider_load_issues?.length}><details class="card"><summary><Icon name="warning" size={14} />Provider load issues ({client.state.workspace!.provider_load_issues!.length})</summary><pre>{JSON.stringify(client.state.workspace?.provider_load_issues, null, 2)}</pre></details></Show>
    <Show when={client.state.workspace && JSON.stringify([client.state.workspace.connection_name ?? '', client.state.workspace.id, repairOperation()?.id ?? ''])} keyed>{_repairKey => <AuthenticationRepair client={client} operation={repairOperation()} />}</Show>
    <p class="muted small-text">Install or update provider plugins with the <code>crux plugins</code> command.</p>
  </div>;
}
