import { APIError } from './api.ts';
import type { CruxState } from './state.ts';

export type Target = { workspace_id: string; owner: Record<string, any>; generation: { epoch: string; sequence: number } };
export type ProviderStatus = {
  owner: Target['owner']; configured: boolean; disabled: boolean; account_state: string;
  active_account_id?: string; credentials: { kind: string; state: string }[]; credential_slots?: { id: string; kind: string; configured: boolean }[];
};
export type AuthSnapshot = { workspace_id: string; generation: Target['generation']; providers: ProviderStatus[] };
export type AccountSummary = { id: string; display_name: string; active: boolean; credential_state: string; refreshable?: boolean; expires_at?: number };
export type Accounts = { target: Target; status?: ProviderStatus; accounts: AccountSummary[] };
export type LoginRef = { login_id: string; operation_id: string; target: Target };
export type LoginState = {
  login: LoginRef; recovery?: { original_workspace_id: string; original_operation_id: string }; sequence: number; phase: string;
  expires_at?: number; authorization_url?: string; user_code?: string;
};
export type RecordedResult = { original_workspace_id: string; operation_id: string; state: string; abandoned?: boolean };

export const operationID = () => crypto.randomUUID().replaceAll('-', '');
export const same = (left: any, right: any): boolean => left === right || Boolean(left && right && typeof left === 'object' && typeof right === 'object' && Object.keys(left).length === Object.keys(right).length && Object.keys(left).every(key => same(left[key], right[key])));

export function validateAuthReceipt(receipt: any, request: { operation_id: string; target: Target; check_id?: string; account_id?: string; login_id?: string }, clientOwned = false) {
  const outcome = receipt?.outcome;
  if (receipt?.error) throw new Error(receipt.error.message);
  if (!outcome || outcome.operation_id !== request.operation_id || !same(outcome.previous, request.target) || (request.check_id && outcome.check_id !== request.check_id) || (request.login_id && outcome.login_id !== request.login_id)) throw new Error('The authentication receipt does not match this operation.');
  if (outcome.superseded || !outcome.change || !outcome.progress?.runtime_published || (clientOwned ? receipt.client_owned !== true : receipt.workspace?.id !== request.target.workspace_id)) throw new Error('Authentication changed, but a current runtime receipt was not returned. Refresh to inspect the current state.');
  const current = outcome.change.current;
  if (!same(current?.target?.owner, request.target.owner) || current?.target?.workspace_id !== request.target.workspace_id || current?.target?.generation?.epoch !== request.target.generation.epoch || current.target.generation.sequence <= request.target.generation.sequence || (request.account_id && current?.status?.active_account_id !== request.account_id)) throw new Error('The authentication receipt contains a different account or generation.');
}

export function describeAuthError(reason: unknown) {
  const progress = reason instanceof APIError ? reason.payload?.outcome?.progress : undefined;
  const effects = progress ? Object.entries(progress).filter(([, value]) => value).map(([key]) => key.replaceAll('_', ' ')).join(', ') : '';
  return `${reason instanceof Error ? reason.message : String(reason)}${effects ? ` Recorded effects: ${effects}.` : ''}`;
}

export type LoginCallbacks = { status: (state: LoginState) => void; error: (message: string) => void; retryAvailable: (available: boolean) => void; done: () => void; closed: () => void };

// Drives one native OAuth login (browser, hosted paste, device code, or a
// recorded-result recovery) until the original login's runtime receipt.
export function createLogin(client: CruxState, target: Target, callbacks: LoginCallbacks, recovery?: RecordedResult) {
  const login: LoginRef = { login_id: operationID(), operation_id: operationID(), target };
  const connection = client.api.connection;
  const path = new URL(client.api.endpoint(`/v1/workspaces/${target.workspace_id}/auth/oauth`, connection), location.origin).href;
  const loopbackPath = new URL(client.api.endpoint(`/ui/workspaces/${target.workspace_id}/oauth/loopback`, connection), location.origin).href;
  let controller = new AbortController();
  let closed = false, completed = false, committing = false, relayStarted = false, canceling = false;
  let saving: AbortController | undefined;
  const ownsWorkspace = () => client.api.connection === connection && client.state.workspace?.id === target.workspace_id && client.api.workspace?.id === target.workspace_id;
  const active = () => !closed && !completed && ownsWorkspace();
  let submitted: { login: LoginRef; submission_id: string; input: string } | undefined;
  const request = (action: string, body: unknown) => client.api.request<any>(`${path}/${action}`, 'POST', body, controller.signal);
  function verify(result: any, after = 0): LoginState {
    if (!same(result.login, login) || !same(result.state?.login, login) || Boolean(result.state.recovery) !== Boolean(recovery) || result.state.sequence < after) throw new Error('The sign-in response does not match the original login.');
    if (result.error) throw new Error(result.error.message);
    if (result.state.authorization_url && !/^https?:\/\//.test(result.state.authorization_url)) throw new Error('The provider returned an invalid sign-in URL.');
    return result.state;
  }
  function fail(reason: unknown) { if (active()) callbacks.error(describeAuthError(reason)); }
  async function complete() {
    if (!active() || saving && !saving.signal.aborted) return;
    if (controller.signal.aborted) controller = new AbortController();
    const attempt = controller;
    saving = attempt;
    committing = true; callbacks.retryAvailable(false); callbacks.error('');
    try {
      const receipt = await request('complete', login);
      if (!active() || attempt.signal.aborted) return;
      validateAuthReceipt(receipt, login, Boolean(connection));
      completed = true; attempt.abort();
      callbacks.done();
    } catch (reason) { if (active() && !attempt.signal.aborted) { fail(reason); callbacks.retryAvailable(true); } }
    finally { if (saving === attempt) saving = undefined; }
  }
  async function advance(initial: any) {
    const polling = controller;
    if (!active() || polling.signal.aborted) return;
    let current = verify(initial);
    while (active() && !polling.signal.aborted) {
      callbacks.status(current);
      if (['authorized', 'committing', 'complete'].includes(current.phase)) { await complete(); return; }
      if (['canceled', 'failed', 'expired'].includes(current.phase)) throw new Error(`Sign-in ${current.phase}.`);
      if (current.phase === 'waiting-for-loopback' && !relayStarted) {
        relayStarted = true;
        const relay = controller;
        void client.api.request(loopbackPath, 'POST', login, relay.signal).catch(reason => { if (!relay.signal.aborted) fail(reason); relay.abort(); });
      }
      const result = await request('wait', { login, after: current.sequence });
      if (!active() || polling.signal.aborted) return;
      const next = verify(result, current.sequence);
      if (next.sequence === current.sequence && next.phase !== 'complete') throw new Error('Sign-in returned a stale progress cursor.');
      current = next;
    }
  }
  const lifetime = controller;
  const begin = recovery
    ? request('recover', { original_workspace_id: recovery.original_workspace_id, login, original_operation_id: recovery.operation_id })
    : request('begin', login);
  void begin.then(result => { if (!lifetime.signal.aborted && active()) return advance(result); }).catch(reason => { if (!lifetime.signal.aborted) fail(reason); lifetime.abort(); });
  return {
    login,
    async submitCode(input: string) {
      if (!active() || canceling || controller.signal.aborted) return;
      const attempt = controller;
      if (!submitted || submitted.input !== input) submitted = { login, submission_id: operationID(), input };
      const submission = submitted;
      const result = await request('code', submission);
      if (!active() || attempt.signal.aborted) return;
      verify(result);
      if (result.submission_id !== submission.submission_id) throw new Error('The code receipt does not match this submission.');
      submitted = undefined;
    },
    retry: () => canceling ? Promise.resolve() : complete(),
    async cancel() {
      if (!active() || canceling) return;
      canceling = true;
      controller.abort();
      try {
        const result = await client.api.request<any>(`${path}/cancel`, 'POST', login);
        if (!active()) return;
        const current = verify(result);
        if (['committing', 'complete'].includes(current.phase)) { controller = new AbortController(); await complete(); }
        else if (current.phase === 'canceled') { closed = true; callbacks.closed(); }
        else throw new Error('The provider has not acknowledged cancellation.');
      } catch (reason) {
        if (!active()) return;
        if (reason instanceof APIError && same(reason.payload?.state?.login, login) && reason.payload?.state?.phase === 'canceled') { closed = true; callbacks.closed(); }
        else fail(reason);
      } finally { canceling = false; }
    },
    dispose() {
      const shouldCancel = !closed && !completed && !committing && !canceling;
      closed = true; controller.abort();
      if (shouldCancel) void client.api.request(`${path}/cancel`, 'POST', login).catch(reason => {
        if (ownsWorkspace() && !(reason instanceof APIError && same(reason.payload?.state?.login, login) && reason.payload?.state?.phase === 'canceled')) client.fail(reason);
      });
    },
    get committing() { return committing; },
  };
}

export type LocalRepairRequest = { workspace_id: string; operation_workspace_id: string; operation_id: string; revision: number; apply: boolean; abandon: boolean };
export type LocalRepairSummary = {
  workspace_id: string; operation_id: string; revision: number; action: string; provider_id: string;
  account_id?: string; removed_account_id?: string; original: Record<string, boolean>;
  refresh_started: boolean; refresh_observed: boolean; finished: boolean; coherent: boolean;
  repair_ready: boolean; needs_reload: boolean; abandoned: boolean; no_effects: boolean;
  repair_started: boolean; repair_accounts_written: boolean; repair_config_written: boolean;
};
export type LocalRepairResponse = {
  request: LocalRepairRequest;
  result: { summary: LocalRepairSummary; accounts_written: boolean; accounts_matched: boolean; config_written: boolean; config_matched: boolean; needs_reload: boolean; abandoned: boolean };
  error?: { code: string; message: string };
};

export async function repairLocalAuthentication(client: CruxState, request: LocalRepairRequest, signal?: AbortSignal): Promise<LocalRepairResponse> {
  let response: LocalRepairResponse;
  try { response = await client.api.request<LocalRepairResponse>(`/v1/workspaces/${request.workspace_id}/auth/local-repair`, 'POST', request, signal); }
  catch (reason) {
    if (!(reason instanceof APIError) || reason.status !== 422 || !reason.payload?.error) throw reason;
    response = reason.payload;
  }
  if (!same(response.request, request) || !response.result) throw new Error('The repair response does not match the requested operation.');
  const result = response.result, summary = result.summary;
  if (summary?.operation_id) {
    if (summary.workspace_id !== request.operation_workspace_id || summary.operation_id !== request.operation_id || !summary.provider_id || !Number.isSafeInteger(summary.revision) || summary.revision <= 0) throw new Error('The repair summary belongs to a different operation or revision.');
  } else if (!response.error) throw new Error('The repair response contains no operation summary.');
  if (!request.apply && (result.accounts_written || result.config_written || result.accounts_matched || result.config_matched || result.needs_reload)) throw new Error('The repair review unexpectedly reported storage changes.');
  if (result.abandoned && (!request.abandon || !summary?.abandoned) || !response.error && request.abandon && !result.abandoned) throw new Error('The repair abandonment was not confirmed.');
  if (result.needs_reload && (!request.apply || !summary?.needs_reload) || !response.error && request.apply && !result.needs_reload) throw new Error('The repair completion was not confirmed.');
  return response;
}

export async function listRecordedLogins(client: CruxState, target: Target) {
  const result = await client.api.request<{ list: { target: Target; results: RecordedResult[] }; error?: { message: string } }>(client.api.route('/auth/oauth/results'), 'POST', target);
  if (result.error) throw new Error(result.error.message);
  if (!same(result.list?.target, target)) throw new Error('The recovery list does not match this provider.');
  return result.list.results ?? [];
}

export async function abandonRecordedLogin(client: CruxState, target: Target, recorded: RecordedResult) {
  const request = { target, original_workspace_id: recorded.original_workspace_id, original_operation_id: recorded.operation_id };
  const result = await client.api.request<{ outcome: { request: typeof request; abandoned: boolean; exchange_outcome?: string }; error?: { message: string } }>(client.api.route('/auth/oauth/abandon'), 'POST', request);
  if (result.error) throw new Error(result.error.message);
  if (!same(result.outcome?.request, request) || !result.outcome.abandoned) throw new Error('The provider did not confirm the abandoned login.');
  return result.outcome;
}

export function providerUsage(client: CruxState, owner: Record<string, any>) {
  const snapshot = client.state.providerUsage[owner.provider_id];
  const authority = client.state.workspace?.authority;
  const revision = authority?.mode === 'client' ? authority.revision ?? 0 : 0;
  const digest = authority?.mode === 'client' ? authority.digest ?? '' : '';
  if (!snapshot || !client.state.connected || snapshot.connection !== client.api.connection || snapshot.workspaceID !== client.state.workspace?.id || !same(snapshot.owner, owner) || snapshot.revision !== revision || snapshot.digest !== digest) return undefined;
  return snapshot.usage;
}
