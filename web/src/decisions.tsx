import { createSignal, For, onCleanup, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Permission, Question } from './types.ts';
import { errorText, Icon } from './ui.tsx';
import { Markdown } from './conversation.tsx';

function PermissionCard(props: { client: CruxState; permission: Permission }) {
  const { client } = props;
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const workspaceID = client.api.workspace?.id;
  const connection = client.api.connection;
  const route = client.api.route('/permissions/grant');
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && client.api.connection === connection && client.api.workspace?.id === workspaceID;
  const params = () => { try { return JSON.stringify(props.permission.params, null, 2); } catch { return String(props.permission.params); } };
  async function grant(action: string) {
    if (busy() || !current()) return;
    setBusy(true); setError('');
    try { await client.api.request(route, 'POST', { permission: props.permission, action }); if (current()) await client.refreshPending(); }
    catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  return <section class="decision" aria-label="Permission request">
    <header><Icon name="shield" size={16} /><div><span class="eyebrow">Permission needed</span><h3>{props.permission.tool_name} <span class="muted">· {props.permission.action}</span></h3></div></header>
    <Show when={props.permission.description}><p>{props.permission.description}</p></Show>
    <Show when={props.permission.path}><code class="path">{props.permission.path}</code></Show>
    <details><summary>Request details</summary><pre>{params()}</pre></details>
    <Show when={props.permission.session_id !== client.state.sessionID}><button class="link" onClick={() => client.attempt(() => client.selectSession(props.permission.session_id))}>Open the requesting session</button></Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <div class="actions">
      <button disabled={busy() || !client.state.connected} onClick={() => grant('deny')}>Deny</button>
      <span class="spacer" />
      <button disabled={busy() || !client.state.connected} onClick={() => grant('allow_session')}>Allow for session</button>
      <button class="primary" disabled={busy() || !client.state.connected} onClick={() => grant('allow')}>Allow once</button>
    </div>
  </section>;
}

function QuestionCard(props: { client: CruxState; question: Question }) {
  const { client } = props;
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [step, setStep] = createSignal(0);
  const [answers, setAnswers] = createSignal<Record<string, { yes?: boolean; selected: string[]; text: string }>>({});
  const workspaceID = client.api.workspace?.id;
  const connection = client.api.connection;
  const root = client.api.route('/questions');
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const ownsRequest = () => !disposed && client.api.connection === connection && client.api.workspace?.id === workspaceID;
  const questions = () => props.question.questions;
  const confirming = () => questions().length > 1 && step() >= questions().length;
  const answer = (id: string) => answers()[id] ?? { selected: [], text: '' };
  const update = (id: string, patch: Partial<{ yes?: boolean; selected: string[]; text: string }>) => setAnswers(value => ({ ...value, [id]: { ...answer(id), ...patch } }));
  const complete = (item: Question['questions'][number]) => {
    const value = answer(item.id);
    if (item.type === 'yes_no') return value.yes !== undefined || Boolean(value.text.trim());
    if (item.type === 'free_text') return Boolean(value.text.trim());
    return value.selected.length > 0 || Boolean(value.text.trim());
  };
  async function submit() {
    if (busy() || !client.state.connected || !ownsRequest()) return;
    setBusy(true); setError('');
    try {
      const responses = questions().map(item => {
        const value = answer(item.id);
        return { request_id: item.id, ...(item.type === 'yes_no' && value.yes !== undefined ? { yes: value.yes } : {}), selected_ids: item.type.endsWith('choice') ? value.selected : undefined, fill_in_text: value.text };
      });
      const result = await client.api.request<{ resolved: boolean }>(`${root}/answer`, 'POST', { batch_request_id: props.question.id, responses });
      if (!ownsRequest()) return;
      await client.refreshPending();
      if (!ownsRequest()) return;
      if (!result.resolved) throw new Error('This question is no longer pending. The current request was refreshed.');
    } catch (reason) { if (ownsRequest()) setError(errorText(reason)); }
    finally { if (ownsRequest()) setBusy(false); }
  }
  async function cancel() {
    if (busy() || !client.state.connected || !ownsRequest()) return;
    setBusy(true); setError('');
    try { await client.api.request(`${root}/cancel`, 'POST'); if (ownsRequest()) await client.refreshPending(); }
    catch (reason) { if (ownsRequest()) setError(errorText(reason)); }
    finally { if (ownsRequest()) setBusy(false); }
  }
  const current = () => questions()[Math.min(step(), questions().length - 1)];
  return <section class="decision" aria-label="Question from Crux">
    <header><Icon name="info" size={16} /><div><span class="eyebrow">Input needed{questions().length > 1 ? ` · ${Math.min(step() + 1, questions().length)} of ${questions().length}` : ''}</span><h3>{confirming() ? props.question.confirm_title || 'Submit your answers?' : current().question}</h3></div></header>
    <Show when={questions().length > 1}><div class="steps"><For each={questions()}>{(item, index) => <button aria-label={item.label || item.question} aria-current={index() === step() ? 'step' : undefined} disabled={busy()} classList={{ active: index() === step(), done: complete(item) }} onClick={() => setStep(index())}>{item.label || `${index() + 1}`}</button>}</For><button classList={{ active: confirming() }} disabled={!questions().every(complete)} onClick={() => setStep(questions().length)}>✓</button></div></Show>
    <Show when={!confirming()} fallback={<div class="confirm-summary"><Show when={props.question.confirm_description}><Markdown text={props.question.confirm_description!} /></Show><For each={questions()}>{item => <div><strong>{item.question}</strong><span>{item.type === 'yes_no' ? answer(item.id).yes === undefined ? '' : answer(item.id).yes ? 'Yes' : 'No' : item.choices?.filter(choice => answer(item.id).selected.includes(choice.id)).map(choice => choice.label).join(', ')}{answer(item.id).text ? ` ${answer(item.id).text}` : ''}</span></div>}</For></div>}>
      {(() => {
        const item = current;
        return <fieldset disabled={busy() || !client.state.connected}>
          <Show when={item().description}><div class="muted"><Markdown text={item().description} /></div></Show>
          <Show when={item().type === 'yes_no'}><div class="choice-row"><button type="button" classList={{ selected: answer(item().id).yes === true }} onClick={() => update(item().id, { yes: true })}>Yes</button><button type="button" classList={{ selected: answer(item().id).yes === false }} onClick={() => update(item().id, { yes: false })}>No</button></div></Show>
          <Show when={item().choices?.length}><div class="choices"><For each={item().choices}>{choice => {
            const checked = () => answer(item().id).selected.includes(choice.id);
            return <label class="choice" classList={{ selected: checked() }}><input type={item().type === 'multi_choice' ? 'checkbox' : 'radio'} name={item().id} checked={checked()} onChange={event => update(item().id, { selected: item().type === 'multi_choice' ? event.currentTarget.checked ? [...answer(item().id).selected, choice.id] : answer(item().id).selected.filter(id => id !== choice.id) : [choice.id] })} /><span><strong>{choice.label}</strong><Show when={choice.description}><small>{choice.description}</small></Show></span></label>;
          }}</For></div></Show>
          <textarea rows={2} aria-label={`${item().question}: written answer`} placeholder={item().type === 'free_text' ? 'Your answer' : 'Add details or write your own answer (optional)'} value={answer(item().id).text} onInput={event => update(item().id, { text: event.currentTarget.value })} />
        </fieldset>;
      })()}
    </Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <div class="actions">
      <button disabled={busy() || !client.state.connected} onClick={cancel}>Cancel</button>
      <span class="spacer" />
      <Show when={questions().length > 1 && step() > 0}><button disabled={busy()} onClick={() => setStep(step() - 1)}>Back</button></Show>
      <Show when={questions().length > 1 && !confirming()} fallback={<button class="primary" disabled={busy() || !client.state.connected || !questions().every(complete)} onClick={submit}>{busy() ? 'Submitting…' : 'Submit'}</button>}>
        <button class="primary" disabled={busy() || !complete(current())} onClick={() => setStep(step() + 1)}>Next</button>
      </Show>
    </div>
  </section>;
}

export function Decisions(props: { client: CruxState }) {
  return <>
    <Show when={props.client.state.permission} keyed>{permission => <PermissionCard client={props.client} permission={permission} />}</Show>
    <Show when={props.client.state.question} keyed>{question => <QuestionCard client={props.client} question={question} />}</Show>
  </>;
}
