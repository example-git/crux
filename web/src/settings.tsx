import { createEffect, createMemo, createSignal, For, on, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Models, Provider, RuntimeControl } from './types.ts';
import { ModelPopover } from './models.tsx';
import { ProvidersSettings } from './providers.tsx';
import { IndexSettings, IntegrationsSettings } from './integrations.tsx';
import { Markdown } from './conversation.tsx';
import { readStored, writeStored } from './storage.ts';
import { InstructionEditors } from './instructions.tsx';
import { Badge, errorText, Icon, Modal, Row, Segmented, Switch } from './ui.tsx';

const sections = [
  ['models', 'Models', 'layers'], ['behavior', 'Behavior', 'steer'], ['context', 'Context', 'database'], ['instructions', 'Instructions', 'file'],
  ['providers', 'Providers', 'key'], ['integrations', 'Integrations', 'plug'], ['index', 'Codebase index', 'search'], ['agents', 'Agents', 'user'], ['about', 'Configuration', 'info'],
] as const;
export type SettingsSection = typeof sections[number][0];

function surfaceFor(client: CruxState, slot: 'large' | 'small') {
  const selected = client.selectedModel(slot);
  return client.state.workspace?.provider_surfaces?.find(item => item.id === selected?.provider);
}

function configModelState(client: CruxState): Models {
  const state: Models = {};
  for (const slot of ['large', 'small'] as const) {
    const model = client.state.workspace?.config.models?.[slot];
    const owner = client.state.workspace?.provider_surfaces?.find(item => item.id === model?.provider)?.owner;
    if (model && owner) state[slot] = { model, owner };
  }
  return state;
}

async function applyToAgent(client: CruxState, models: Models) {
  if (!models.large && !models.small) return;
  if (client.state.agent?.is_ready) await client.api.request(client.api.route('/agent/update'), 'POST', { state: models });
  else await client.api.request(client.api.route('/agent/init'), 'POST', { interactive: true });
}

async function setConfig(client: CruxState, scope: 0 | 1, key: string, value: unknown) {
  const workspace = client.api.workspace;
  const connection = client.api.connection;
  await client.api.request(client.api.route('/config/set'), 'POST', { scope, key, value });
  if (client.api.workspace !== workspace || client.api.connection !== connection) return;
  await client.refreshWorkspace();
}

type InstructionGate = { busy: () => boolean; run: (action: () => Promise<unknown>, success?: string) => Promise<boolean>; revision: () => number };

function RuntimeControlRow(props: { client: CruxState; provider: Provider; control: RuntimeControl; gate: InstructionGate }) {
  const { client } = props;
  const [resolved, setResolved] = createSignal<Record<string, any>>();
  const [value, setValue] = createSignal('');
  const [error, setError] = createSignal('');
  let generation = 0;
  const selectedModel = () => client.state.workspace?.config.models?.large?.model;
  const target = () => ({ owner: props.provider.owner, control_id: props.control.id, descriptor_digest: props.control.descriptor_digest, selection: { model_type: 'large', model_id: selectedModel() } });
  const applies = () => props.control.available && (!props.control.available_models || props.control.available_models.includes(selectedModel() ?? ''));
  createEffect(on(() => [props.gate.revision(), selectedModel(), props.control.descriptor_digest, applies()], () => {
    const current = ++generation;
    setResolved(undefined); setError('');
    if (!applies()) return;
    void client.api.request<Record<string, any>>(client.api.route('/config/runtime-control/resolve'), 'POST', { scope: 0, target: target() }).then(state => {
      if (generation !== current) return;
      setResolved(state); setValue(state.effective?.present ? String(state.effective.value) : '');
    }).catch(reason => { if (generation === current) setError(errorText(reason)); });
  }));
  function save(remove: boolean) {
    const state = resolved();
    if (!state) return;
    const hostOption = state.binding?.kind === 'host-option';
    if (!remove && !value().trim() && hostOption) remove = true;
    let converted: unknown = value();
    if (!remove) {
      if (props.control.type === 'boolean') { if (!['true', 'false'].includes(value())) { setError('Choose true or false.'); return; } converted = value() === 'true'; }
      if (props.control.type === 'integer') { if (!/^-?\d+$/.test(value().trim())) { setError('Enter a whole number.'); return; } converted = Number(value()); }
      if (props.control.type === 'number') { converted = Number(value()); if (!value().trim() || !Number.isFinite(converted)) { setError('Enter a number.'); return; } }
      if (props.control.type === 'enum' && !props.control.values?.includes(value())) { setError('Choose one of the listed values.'); return; }
    }
    setError('');
    const model = selectedModel();
    void props.gate.run(async () => {
      const next = await client.api.request<Record<string, any>>(client.api.route('/config/runtime-control'), remove ? 'DELETE' : 'PUT', { scope: 0, target: state.target, ...(remove ? {} : { value: converted }) });
      if (next.scope !== 0 || !next.scoped_known || Boolean(next.scoped?.present) === remove || (!remove && (next.runtime_dependent || JSON.stringify(next.scoped?.value) !== JSON.stringify(converted)))) throw new Error('The saved control does not match this change. Reopen Instructions to inspect the current value.');
      if (selectedModel() !== model) throw new Error('The model changed while this control was saving. Choose the control again.');
      return next.models as Models;
    }, `${props.control.label} ${remove ? 'reset' : 'updated'}.`);
  }
  const source = () => {
    const state = resolved();
    if (!applies()) return props.control.diagnostic || 'Not available for this model';
    if (!state) return error() ? '' : 'Loading…';
    if (state.runtime_dependent) return 'Decided at request time';
    if (state.global_override) return `Global override (${state.global_override.config_key})`;
    const kind = state.source?.kind === 'host-option' ? 'global setting' : state.source?.kind ?? 'absent';
    return `${kind}${state.source?.scope === 1 ? ' · workspace' : state.source?.scope === 0 ? ' · global' : ''}${state.scoped_known ? state.scoped?.present ? '' : ' · inherited' : ' · saved scope unknown'}`;
  };
  return <div class="control" classList={{ muted: !applies() }}>
    <div class="row-text"><strong>{props.control.label}<Show when={props.control.values?.length}><span class="muted"> ({props.control.values!.join('/')})</span></Show></strong><Show when={props.control.description}><small>{props.control.description}</small></Show><small class="muted">{source()}</small></div>
    <div class="control-input">
      <Show when={props.control.type === 'enum' || props.control.type === 'boolean'} fallback={<input aria-label={props.control.label} disabled={!applies()} type={['integer', 'number'].includes(props.control.type) ? 'number' : 'text'} value={value()} onInput={event => setValue(event.currentTarget.value)} onKeyDown={event => { if (event.key === 'Enter') save(false); }} />}>
        <select aria-label={props.control.label} disabled={!applies()} value={value()} onChange={event => setValue(event.currentTarget.value)}><option value="">Not set</option><For each={props.control.type === 'boolean' ? ['true', 'false'] : props.control.values}>{choice => <option value={choice}>{choice}</option>}</For></select>
      </Show>
      <button class="small" disabled={props.gate.busy() || !resolved()} onClick={() => save(false)}>Apply</button>
      <button class="ghost small" disabled={props.gate.busy() || !resolved() || !resolved()?.scoped?.present} onClick={() => save(true)} title="Remove the saved value">Reset</button>
    </div>
    <Show when={error()}><p class="form-error">{error()}</p></Show>
  </div>;
}

function ModelsSection(props: { client: CruxState; open: (section: SettingsSection) => void }) {
  const { client } = props;
  const [scope, setScope] = createSignal<'0' | '1' | 'runtime'>('0');
  const [picker, setPicker] = createSignal<'large' | 'small' | ''>('');
  const provider = () => surfaceFor(client, 'large');
  const label = (slot: 'large' | 'small') => {
    const selected = client.selectedModel(slot);
    const surface = surfaceFor(client, slot);
    return { model: surface?.models?.find(item => item.id === selected?.model)?.name ?? selected?.model ?? 'Not selected', provider: surface?.name ?? selected?.provider ?? '' };
  };
  return <div class="settings-section">
    <div class="section-head"><div><h2>Models</h2><p>{client.state.sessionID ? 'Changes also apply to the open session.' : 'Choose the models the agent uses.'}</p></div></div>
    <Row title="Model selection scope" detail="Global and workspace choices write configuration. Runtime choices do not; the selected session still retains its model."><Segmented label="Settings scope" value={scope()} onChange={setScope} options={[{ value: '0', label: 'Global' }, { value: '1', label: 'Workspace' }, { value: 'runtime', label: 'Runtime' }]} /></Row>
    <For each={[['large', 'Main model', 'Conversation, coding, and tools'], ['small', 'Small model', 'Titles, summaries, and lightweight tasks']] as const}>{([slot, title, detail]) => <div class="row">
      <div class="row-text"><strong>{title}</strong><small>{detail}</small></div>
      <div class="row-control anchor"><button class="model-button" onClick={() => setPicker(picker() === slot ? '' : slot)}><span><strong>{label(slot).model}</strong><small>{label(slot).provider}</small></span><Icon name="down" size={13} /></button>
        <Show when={picker() === slot}><ModelPopover client={client} slot={slot} scope={scope() === 'runtime' ? 'runtime' : Number(scope())} close={() => setPicker('')} /></Show>
      </div>
    </div>}</For>
    <Show when={provider()?.runtime_controls?.length}><button class="row-button" onClick={() => props.open('instructions')}><Icon name="settings" size={14} /><span>{provider()!.name} runtime controls ({provider()!.runtime_controls!.length}) are in Instructions</span><Icon name="right" size={13} /></button></Show>
  </div>;
}

function BehaviorSection(props: { client: CruxState }) {
  const { client } = props;
  const savedTransparency = readStored<number>('backdrop-transparency');
  const [transparency, setTransparency] = createSignal(typeof savedTransparency === 'number' && Number.isFinite(savedTransparency) ? Math.max(0, Math.min(100, savedTransparency)) : 75);
  return <div class="settings-section">
    <div class="section-head"><div><h2>Behavior</h2><p>Native interaction settings and browser alerts.</p></div></div>
    <div class="subhead"><h3>Messages sent while Crux is working</h3></div>
    <div class="choice-cards"><For each={[['queue', 'Queue', 'Let the current turn finish, then run the next message.'], ['steer', 'Steer', 'Interrupt generation and tools to redirect the current turn. Summarization finishes first.']] as const}>{([mode, title, detail]) => <button class="choice-card" aria-pressed={client.state.deliveryMode === mode} disabled={client.state.deliverySaving} onClick={() => client.attempt(() => client.setDeliveryMode(mode))}><Icon name={mode} size={20} /><strong>{title}</strong><small>{detail}</small></button>}</For></div>
    <div class="subhead"><h3>Permissions</h3></div>
    <p class="muted">Choose Normal, Plan, YOLO, or Plan + YOLO from the mode dropdown beside the composer. Plan belongs to the session; YOLO auto-approves tool requests across this running workspace until changed or the host restarts.</p>
    <Show when={document.documentElement.classList.contains('desktop')}>
      <div class="subhead"><h3>Appearance</h3></div>
      <Row title="Background transparency" detail="Adjust the translucent desktop backdrop without fading text or controls.">
        <input type="range" min="0" max="100" step="1" aria-label="Background transparency" value={transparency()} onInput={event => { const value = Number(event.currentTarget.value); setTransparency(value); document.documentElement.style.setProperty('--backdrop-opacity', String(1 - value / 100)); writeStored('backdrop-transparency', value); }} />
        <output>{transparency()}%</output>
      </Row>
    </Show>
    <div class="subhead"><h3>Browser notifications</h3></div>
    <Row title="Notify when away" detail="Desktop alerts for completed turns, background tasks, permission requests, and questions when this tab is not focused. Applies to this tab until reload; terminal notification settings are unchanged.">
      <button disabled={client.state.notificationsBusy} onClick={() => void client.setNotifications(!client.state.notificationsEnabled)}>{client.state.notificationsBusy ? 'Requesting permission…' : client.state.notificationsEnabled ? 'Disable notifications' : 'Enable notifications'}</button>
    </Row>
    <p class="muted small-text" role="status">{client.state.notificationsEnabled ? 'Notifications enabled for this tab.' : 'Notifications disabled for this tab.'} Permission: {client.state.notificationPermission}.</p>
    <Show when={client.state.notificationError}><p class="form-error" role="alert">{client.state.notificationError}</p></Show>
    <button disabled={!client.state.notificationsEnabled || client.state.notificationsBusy} onClick={client.testNotification}>Send test notification</button>
  </div>;
}

function NumberSetting(props: { client: CruxState; title: string; detail: string; keyPath: string; value: number }) {
  const [value, setValue] = createSignal(String(props.value ?? 0));
  const [error, setError] = createSignal('');
  createEffect(on(() => props.value, next => setValue(String(next ?? 0)), { defer: true }));
  return <Row title={props.title} detail={<>{props.detail}<Show when={error()}><span class="bad"> {error()}</span></Show></>}>
    <form class="inline-form" onSubmit={event => {
      event.preventDefault();
      if (!/^\d+$/.test(value().trim())) { setError('Enter a non-negative whole number; 0 uses the model default.'); return; }
      setError('');
      void props.client.attempt(async () => { await setConfig(props.client, 1, props.keyPath, Number(value())); props.client.toast('Saved for this project. Applies to new turns.', 'success'); });
    }}><input class="narrow" type="number" min="0" aria-label={props.title} value={value()} onInput={event => setValue(event.currentTarget.value)} /><button class="small" disabled={value() === String(props.value ?? 0)}>Save</button></form>
  </Row>;
}

function ContextSection(props: { client: CruxState }) {
  const { client } = props;
  const options = () => client.state.workspace?.config.options ?? {};
  const toggle = (key: string, value: boolean) => client.attempt(async () => { await setConfig(client, 1, `options.${key}`, value); client.toast('Saved for this project. Applies to new turns.', 'success'); });
  return <div class="settings-section">
    <div class="section-head"><div><h2>Context & summarization</h2><p>Saved to this workspace, like the terminal client.</p></div></div>
    <Row title="Automatic summarization" detail="Summarize the session when the context window fills up."><Switch label="Automatic summarization" checked={!options().disable_auto_summarize} onChange={value => toggle('disable_auto_summarize', !value)} /></Row>
    <NumberSetting client={client} title="Context window cap" detail="Summarize earlier than the model limit. 0 uses the model default." keyPath="options.summarization_context_cap" value={options().summarization_context_cap ?? 0} />
    <NumberSetting client={client} title="Summary output tokens" detail="Maximum length of a summary. 0 uses the model default." keyPath="options.summarization_max_tokens" value={options().summarization_max_tokens ?? 0} />
    <Row title="Codex fast mode for summaries" detail="Use Codex fast mode when summarizing."><Switch label="Codex fast mode" checked={Boolean(options().summarization_fast_mode)} onChange={value => toggle('summarization_fast_mode', value)} /></Row>
    <Row title="Codex compaction v2" detail="Experimental compaction for Codex models."><Switch label="Codex compaction v2" checked={Boolean(options().codex_compaction_v2)} onChange={value => toggle('codex_compaction_v2', value)} /></Row>
  </div>;
}

const instructionSections = ['identity', 'critical_rules', 'code_references', 'workflow', 'operating_constraints', 'decision_making', 'editing_files', 'whitespace', 'task_completion', 'memory', 'code_conventions', 'tool_usage', 'proactiveness', 'final_answers'];

function InstructionsSection(props: { client: CruxState }) {
  const { client } = props;
  const [preview, setPreview] = createSignal<{ provider_id: string; model_id: string; policy: string; sections: { kind: string; stability: string; text: string; cache_boundary?: boolean }[] }>();
  const [busy, setBusy] = createSignal(false);
  const options = () => client.state.workspace?.config.options ?? {};
  const mode = () => options().instruction_mode ?? 'all';
  const disabled = () => (options().disabled_instruction_sections ?? []) as string[];
  const provider = () => client.state.workspace?.provider_surfaces?.find(item => item.id === client.state.workspace?.config.models?.large?.provider);
  const nativeText = () => { const instructions = provider()?.instructions; return Boolean(instructions?.profiles[instructions.default]?.trim()); };
  const profile = () => client.state.workspace?.config.providers?.[provider()?.id ?? '']?.tooling_instructions || provider()?.instructions?.selection_default || 'crux';
  const [revision, setRevision] = createSignal(0);
  let previewGeneration = 0;
  let attachmentGeneration = 0;
  let disposed = false;
  onCleanup(() => { disposed = true; attachmentGeneration++; previewGeneration++; });
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id], () => {
    attachmentGeneration++;
    previewGeneration++;
    setPreview(undefined);
    setBusy(false);
    setRevision(value => value + 1);
  }));
  async function loadPreview() {
    const current = ++previewGeneration;
    const connection = client.api.connection;
    const result = await client.api.request<NonNullable<ReturnType<typeof preview>>>(client.api.route('/agent/instructions'));
    if (!disposed && connection === client.api.connection && current === previewGeneration) setPreview(result);
  }
  async function change(action: () => Promise<unknown>, success = 'Instructions updated.') {
    if (busy() || disposed || !client.state.workspace) return false;
    const workspaceID = client.state.workspace.id;
    const connection = client.api.connection;
    const generation = attachmentGeneration;
    const current = () => !disposed && generation === attachmentGeneration && connection === client.api.connection && client.state.workspace?.id === workspaceID;
    const requireWorkspace = () => {
      if (!current()) throw new Error('Instructions were saved for the original workspace. Reopen it to apply the updated agent settings.');
    };
    setBusy(true);
    try {
      const models = await action();
      requireWorkspace();
      await client.refreshWorkspace();
      requireWorkspace();
      await applyToAgent(client, models && typeof models === 'object' && ('large' in models || 'small' in models) ? models as Models : configModelState(client));
      requireWorkspace();
      previewGeneration++;
      setPreview(undefined);
      client.toast(success, 'success');
      return true;
    } catch (reason) {
      if (current()) client.toast(errorText(reason), 'error');
      return false;
    } finally {
      if (current()) {
        setRevision(value => value + 1);
        setBusy(false);
      }
    }
  }
  const gate: InstructionGate = { busy, run: change, revision };
  return <div class="settings-section">
    <div class="section-head"><div><h2>Instructions</h2><p>Edit native files, session overrides, and shared tooling settings.</p></div><button disabled={busy()} onClick={() => client.attempt(loadPreview)}><Icon name="eye" size={14} />Preview</button></div>
    <InstructionEditors client={client} saved={() => { previewGeneration++; setPreview(undefined); }} />
    <div class="subhead"><h3>Shared instruction sources</h3><Badge>Saved globally</Badge></div>
    <div class="choice-cards three"><For each={[['all', 'Tooling + project', 'Crux tooling guidance and project context files.'], ['project', 'Project only', 'Project context without tooling guidance.'], ['native', 'Tooling only', 'Tooling guidance without project context.']] as const}>{([value, title, detail]) => <button class="choice-card" aria-pressed={mode() === value} disabled={busy()} onClick={() => change(() => client.api.request(client.api.route('/config/set'), 'POST', { scope: 0, key: 'options.instruction_mode', value }))}><strong>{title}</strong><small>{detail}</small></button>}</For></div>
    <Show when={nativeText() && provider()?.owner}><Row title={`Use ${provider()!.name}'s native tooling instructions`} detail="Replaces the Crux sections below with the provider's own guidance."><Switch label="Native tooling instructions" checked={profile() === 'native'} disabled={busy()} onChange={value => change(() => client.api.request(client.api.route('/config/provider-tooling'), 'PUT', { scope: 0, owner: provider()!.owner, profile: value ? 'native' : 'crux' }))} /></Row></Show>
    <Show when={provider()?.owner}><Row title="Tooling inheritance" detail="Remove the global tooling override. Workspace settings and the provider default still apply."><button disabled={busy()} onClick={() => change(() => client.api.request(client.api.route('/config/provider-tooling'), 'DELETE', { scope: 0, owner: provider()!.owner }), 'Global tooling override removed.')}>Reset global tooling override</button></Row></Show>
    <Show when={provider()}>{selected => <Show when={selected().runtime_controls?.length}><div class="subhead"><h3>{selected().name} runtime controls</h3></div><For each={selected().runtime_controls}>{control => <RuntimeControlRow client={client} provider={selected()} control={control} gate={gate} />}</For></Show>}</Show>
    <div class="subhead"><h3>Crux sections</h3><Show when={profile() === 'native'}><Badge>replaced by native</Badge></Show></div>
    <div class="check-grid"><For each={instructionSections}>{section => <label class="check" classList={{ muted: profile() === 'native' }}><input type="checkbox" disabled={busy() || profile() === 'native'} checked={!disabled().includes(section)} onChange={event => { const enabled = event.currentTarget.checked; const next = enabled ? disabled().filter(item => item !== section) : [...disabled(), section]; void change(() => client.api.request(client.api.route('/config/set'), 'POST', { scope: 0, key: 'options.disabled_instruction_sections', value: next })); }} />{section.replaceAll('_', ' ')}</label>}</For></div>
    <Show when={preview()}>{value => <Modal title="Native workspace instructions" subtitle={`${value().provider_id} / ${value().model_id}`} close={() => { previewGeneration++; setPreview(undefined); }} size="large"><div class="modal-body stack"><For each={value().sections}>{section => <section class="card"><header class="card-head"><Badge>{section.kind}</Badge><Badge>{value().policy === 'anthropic' ? (section.stability === 'static' ? 'Cached' : 'Uncached') : section.stability}</Badge><Show when={section.cache_boundary}><Badge>Cache boundary</Badge></Show></header><Markdown text={section.text} class="compact" /></section>}</For></div></Modal>}</Show>
  </div>;
}

function AgentsSection(props: { client: CruxState }) {
  const { client } = props;
  const models = createMemo(() => (client.state.workspace?.provider_surfaces ?? []).filter(provider => provider.available && !client.state.workspace?.config.providers?.[provider.id]?.disable).flatMap(provider => (provider.models ?? []).map(model => `${provider.id}/${model.id}`)).sort());
  const [form, setForm] = createSignal({ scope: 'project', name: '', description: '', instructions: '', model: '', tools: '', script: false, path: '', timeout: '', variables: '' });
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const update = (patch: Partial<ReturnType<typeof form>>) => setForm(value => ({ ...value, ...patch }));
  let disposed = false;
  let attachmentGeneration = 0;
  onCleanup(() => { disposed = true; attachmentGeneration++; });
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id], () => {
    attachmentGeneration++;
    setBusy(false);
    setError('');
  }));
  async function create(event: SubmitEvent) {
    event.preventDefault();
    if (busy() || disposed) return;
    const workspaceID = client.api.workspace?.id;
    if (!workspaceID) return;
    const connection = client.api.connection;
    const generation = attachmentGeneration;
    const current = () => !disposed && generation === attachmentGeneration && connection === client.api.connection && workspaceID === client.api.workspace?.id && workspaceID === client.state.workspace?.id;
    const route = client.api.route('/agent/definitions');
    setError('');
    const value = form();
    try {
      if (!value.name.trim()) throw new Error('A name is required.');
      if (!value.description.trim()) throw new Error('A description is required.');
      if (!value.instructions.trim()) throw new Error('Instructions are required.');
      const model = value.model || models()[0];
      if (!model) throw new Error('No configured models are available.');
      const raw = value.tools.trim();
      let tools: string[] = [];
      if (raw && raw !== 'none') {
        tools = raw.split(',').map(item => item.trim());
        if (tools.some(item => !item)) throw new Error('Tools cannot contain an empty name.');
        if (new Set(tools).size !== tools.length) throw new Error('A tool is listed more than once.');
        if (tools.includes('*') && tools.length !== 1) throw new Error('The wildcard tool must be the only tool.');
      }
      let script: Record<string, unknown> | undefined;
      if (value.script) {
        if (!value.path.trim()) throw new Error('A script path is required.');
        let variables = {};
        if (value.variables.trim()) { try { variables = JSON.parse(value.variables); } catch (reason) { throw new Error(`Invalid script variables JSON: ${errorText(reason)}`); } }
        script = { path: value.path.trim(), timeout: value.timeout.trim() || undefined, variables };
        if (!tools.includes('*') && !tools.includes('script')) tools.push('script');
      } else if (tools.includes('script')) throw new Error('Enable the script to use the script tool.');
      setBusy(true);
      const result = await client.api.request<{ path: string }>(route, 'POST', { scope: value.scope, name: value.name.trim(), description: value.description.trim(), instructions: value.instructions, model, tools, ...(script ? { script } : {}) });
      if (!current()) return;
      client.toast(`Agent definition created at ${result.path}`, 'success');
      if (form() === value) setForm({ ...value, name: '', description: '', instructions: '', tools: '', script: false, path: '', timeout: '', variables: '' });
    } catch (reason) { if (current()) setError(errorText(reason)); }
    finally { if (current()) setBusy(false); }
  }
  return <div class="settings-section">
    <div class="section-head"><div><h2>Agent definitions</h2><p>Create a custom subagent type the main agent can delegate to.</p></div></div>
    <form class="stack" onSubmit={create}>
      <Row title="Location" detail="Project definitions live in this repository; user definitions apply everywhere."><Segmented label="Definition scope" value={form().scope} onChange={scope => update({ scope })} options={[{ value: 'project', label: 'Project' }, { value: 'user', label: 'User' }]} /></Row>
      <label class="field"><span>Name</span><input aria-label="Agent name" placeholder="reviewer" value={form().name} onInput={event => update({ name: event.currentTarget.value })} /></label>
      <label class="field"><span>Description</span><input aria-label="Agent description" placeholder="Reviews changes for correctness" value={form().description} onInput={event => update({ description: event.currentTarget.value })} /></label>
      <label class="field"><span>Instructions</span><textarea required aria-label="Agent instructions" rows={8} placeholder="Describe the agent’s task, scope, and expected output." value={form().instructions} onInput={event => update({ instructions: event.currentTarget.value })} /></label>
      <label class="field"><span>Model</span><select aria-label="Agent model" value={form().model || models()[0] || ''} onChange={event => update({ model: event.currentTarget.value })}><For each={models()} fallback={<option value="">No configured models</option>}>{model => <option value={model}>{model}</option>}</For></select></label>
      <label class="field"><span>Tools</span><input aria-label="Agent tools" placeholder="none, *, or comma-separated tool names" value={form().tools} onInput={event => update({ tools: event.currentTarget.value })} /></label>
      <Row title="Run a script" detail="Give the agent a bounded script tool."><Switch label="Run a script" checked={form().script} onChange={script => update({ script })} /></Row>
      <Show when={form().script}>
        <label class="field"><span>Script path</span><input aria-label="Script path" placeholder="./scripts/task.py" value={form().path} onInput={event => update({ path: event.currentTarget.value })} /></label>
        <label class="field"><span>Timeout</span><input aria-label="Script timeout" placeholder="2m" value={form().timeout} onInput={event => update({ timeout: event.currentTarget.value })} /></label>
        <label class="field"><span>Variables (JSON)</span><textarea aria-label="Script variables" rows={3} placeholder={'{"input":{"required":true}}'} value={form().variables} onInput={event => update({ variables: event.currentTarget.value })} /></label>
      </Show>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <div class="actions"><span class="spacer" /><button class="primary" disabled={busy()}>Create definition</button></div>
    </form>
  </div>;
}

function AdvancedConfiguration(props: { client: CruxState }) {
  const { client } = props;
  const workspaceID = client.state.workspace!.id;
  const root = client.api.route('');
  const connection = client.api.connection;
  const [scope, setScope] = createSignal('1');
  const [key, setKey] = createSignal('');
  const [value, setValue] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  const [status, setStatus] = createSignal('');
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && client.api.connection === connection && client.state.workspace?.id === workspaceID && client.api.workspace?.id === workspaceID;
  async function save(remove: boolean) {
    if (busy() || !current()) return;
    setError(''); setStatus('');
    const path = key().trim();
    const selectedScope = scope();
    if (!path) { setError('Enter a configuration key.'); return; }
    if (selectedScope !== '0' && selectedScope !== '1') { setError('Choose Global or Workspace scope.'); return; }
    let parsed: unknown;
    if (!remove) {
      try { parsed = JSON.parse(value()); }
      catch { setError('Enter a JSON value, such as true, 1024, "text", an array, or an object.'); return; }
    }
    const providerID = !remove && path.startsWith('providers.') && path.endsWith('.disable') ? path.slice('providers.'.length, -'.disable'.length) : '';
    const owner = providerID ? client.state.workspace?.provider_surfaces?.find(provider => provider.id === providerID)?.owner : undefined;
    if (providerID && !owner) { setError('The provider has no active native owner. Open Providers to inspect its availability.'); return; }
    setBusy(true);
    let saved = false;
    try {
      await client.api.request(`${root}/config/${remove ? 'remove' : 'set'}`, 'POST', { scope: Number(selectedScope), key: path, ...(!remove ? { value: parsed, ...(owner ? { owner } : {}) } : {}) });
      saved = true;
      if (!current()) return;
      await client.refreshWorkspace();
      if (!current()) return;
      setStatus(`${remove ? 'Removed' : 'Saved'} ${path} ${remove ? 'from' : 'in'} ${selectedScope === '0' ? 'global' : 'workspace'} configuration.`);
    } catch (reason) {
      if (current()) setError(`${saved ? 'Configuration was saved, but refreshing the current view failed: ' : ''}${errorText(reason)}`);
    } finally { if (current()) setBusy(false); }
  }
  return <details class="card">
    <summary>Advanced configuration</summary>
    <form class="stack" aria-label="Advanced configuration" onSubmit={event => { event.preventDefault(); void save(false); }}>
      <p class="muted small-text">Set a native configuration key or remove its saved override. Hooks, LSP definitions, tool lists, and other native settings use the same configuration as the CLI. Use Models and Providers for model selection and sign-in.</p>
      <fieldset class="stack" disabled={busy()}>
        <label class="field"><span>Scope</span><select aria-label="Configuration write scope" value={scope()} onChange={event => { setScope(event.currentTarget.value); setStatus(''); }}><option value="1">Workspace</option><option value="0">Global</option></select></label>
        <label class="field"><span>Configuration key</span><input required aria-label="Configuration key" placeholder="options.disabled_tools" value={key()} onInput={event => { setKey(event.currentTarget.value); setStatus(''); }} /></label>
        <label class="field"><span>JSON value</span><textarea aria-label="Configuration JSON value" rows={5} spellcheck={false} autocomplete="off" placeholder={'["bash"]'} value={value()} onInput={event => { setValue(event.currentTarget.value); setStatus(''); }} /></label>
        <div class="actions"><button type="button" disabled={!key().trim()} onClick={() => void save(true)}>Remove saved override</button><span class="spacer" /><button class="primary" disabled={!key().trim() || !value().trim()}>Save configuration</button></div>
      </fieldset>
      <Show when={busy()}><p class="muted" role="status">Updating native configuration…</p></Show>
      <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
      <Show when={status()}><p role="status">{status()}</p></Show>
      <p class="muted small-text">Removing an override exposes any inherited value. Native settings determine when a change takes effect; LSP settings may require restarting language servers in Integrations. Values below are redacted and are not loaded into this editor.</p>
    </form>
  </details>;
}

function AboutSection(props: { client: CruxState }) {
  const { client } = props;
  const [provenance, setProvenance] = createSignal<Record<string, any>>();
  const [needsInit, setNeedsInit] = createSignal<boolean>();
  const [errors, setErrors] = createSignal<string[]>([]);
  const [loading, setLoading] = createSignal(false);
  let controller: AbortController | undefined;
  async function load() {
    controller?.abort();
    setLoading(false); setErrors([]); setProvenance(undefined); setNeedsInit(undefined);
    if (!client.state.connected || !client.api.workspace) return;
    const request = new AbortController(); controller = request;
    const root = client.api.route('');
    setLoading(true);
    const failures: string[] = [];
    await Promise.all([
      client.api.request<Record<string, any>>(`${root}/config-view`, 'GET', undefined, request.signal).then(value => { if (!request.signal.aborted) setProvenance(value); }).catch(reason => failures.push(`Model provenance: ${errorText(reason)}`)),
      client.api.request<{ needs_init: boolean }>(`${root}/project/needs-init`, 'GET', undefined, request.signal).then(value => { if (!request.signal.aborted) setNeedsInit(value.needs_init); }).catch(reason => failures.push(`Project initialization: ${errorText(reason)}`)),
    ]);
    if (!request.signal.aborted) { setErrors(failures); setLoading(false); }
  }
  createEffect(on(() => [client.state.workspace?.connection_name, client.state.workspace?.id, client.state.connected, client.state.workspace?.authority?.revision, client.state.workspace?.authority?.digest], () => void load()));
  onCleanup(() => controller?.abort());
  return <div class="settings-section">
    <div class="section-head"><div><h2>Configuration</h2><p>Native system, user, workspace, and runtime precedence applies.</p></div><button disabled={loading() || !client.state.connected} onClick={() => void load()}>Refresh configuration</button></div>
    <Show when={loading()}><p role="status">Loading configuration details…</p></Show>
    <For each={errors()}>{error => <p class="form-error" role="alert">{error}</p>}</For>
    <dl class="meta wide">
      <dt>Version</dt><dd>{client.state.startup?.version}</dd>
      <dt>Global settings</dt><dd><code>{client.state.startup?.settings_path}</code></dd>
      <dt>Workspace</dt><dd><code>{client.state.workspace?.path}</code></dd>
      <dt>Workspace data</dt><dd><code>{client.state.workspace?.data_dir}</code></dd>
    </dl>
    <Row title="Project instructions" detail={needsInit() === undefined ? 'Initialization status is unavailable.' : needsInit() ? 'This project has not been initialized.' : 'Generate or refresh the project instructions file with the agent.'}><button disabled={!client.state.agent?.is_ready} onClick={() => client.attempt(async () => { await client.initializeProject(); client.toast('Project initialization started.', 'success'); })}>Initialize</button></Row>
    <details class="card"><summary>Model provenance</summary><Show when={provenance()} fallback={<p class="muted small-text">{loading() ? 'Loading…' : 'Model provenance is unavailable.'}</p>}><pre>{JSON.stringify(provenance(), null, 2)}</pre></Show></details>
    <Show when={client.state.workspace && JSON.stringify([client.state.workspace.connection_name ?? '', client.state.workspace.id])} keyed>{_attachment => <AdvancedConfiguration client={client} />}</Show>
    <details class="card"><summary>Effective configuration (redacted)</summary><pre>{JSON.stringify(client.state.workspace?.config, null, 2)}</pre></details>
  </div>;
}

export function Settings(props: { client: CruxState; close: () => void; section?: SettingsSection }) {
  const [section, setSection] = createSignal<SettingsSection>(props.section ?? 'models');
  createEffect(on(() => props.section, value => { if (value) setSection(value); }));
  onMount(() => void props.client.refreshWorkspace().catch(props.client.fail));
  return <Modal title="Settings" close={props.close} size="large" class="settings">
    <div class="settings-layout">
      <nav class="settings-nav" aria-label="Settings sections"><For each={sections}>{([value, label, icon]) => <button aria-current={section() === value ? 'page' : undefined} onClick={() => setSection(value)}><Icon name={icon} size={15} />{label}</button>}</For></nav>
      <div class="settings-body">
        <Show when={section() === 'models'}><ModelsSection client={props.client} open={setSection} /></Show>
        <Show when={section() === 'behavior'}><BehaviorSection client={props.client} /></Show>
        <Show when={section() === 'context'}><ContextSection client={props.client} /></Show>
        <Show when={section() === 'instructions'}><InstructionsSection client={props.client} /></Show>
        <Show when={section() === 'providers'}><ProvidersSettings client={props.client} /></Show>
        <Show when={section() === 'integrations'}><IntegrationsSettings client={props.client} /></Show>
        <Show when={section() === 'index'}><IndexSettings client={props.client} /></Show>
        <Show when={section() === 'agents'}><AgentsSection client={props.client} /></Show>
        <Show when={section() === 'about'}><AboutSection client={props.client} /></Show>
      </div>
    </div>
  </Modal>;
}
