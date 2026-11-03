import { createMemo, createSignal, For, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Model, Provider, SelectedModel } from './types.ts';
import { errorText, Icon, Segmented } from './ui.tsx';

type Entry = { provider: Provider; model: Model; recent?: boolean };

export function ModelPopover(props: { client: CruxState; close: () => void; slot?: 'large' | 'small'; scope?: number | 'runtime' }) {
  const { client } = props;
  const [slot, setSlot] = createSignal<'large' | 'small'>(props.slot ?? 'large');
  const [query, setQuery] = createSignal('');
  const [busy, setBusy] = createSignal('');
  const [error, setError] = createSignal('');
  const [active, setActive] = createSignal(0);
  let root!: HTMLDivElement;
  let search!: HTMLInputElement;
  onMount(() => {
    search.focus();
    const outside = (event: MouseEvent) => { if (!root.parentElement?.contains(event.target as Node)) props.close(); };
    document.addEventListener('mousedown', outside);
    onCleanup(() => document.removeEventListener('mousedown', outside));
  });
  const providers = () => (client.state.workspace?.provider_surfaces ?? []).filter(provider => !client.state.workspace?.config.providers?.[provider.id]?.disable);
  const selected = () => client.selectedModel(slot());
  const mainProvider = () => providers().find(provider => provider.id === client.selectedModel('large')?.provider);
  let recommendation: AbortController | undefined;
  onCleanup(() => recommendation?.abort());
  async function chooseDefaultSmall() {
    const provider = mainProvider();
    if (busy() || !provider) return;
    const workspaceID = client.state.workspace?.id;
    const connection = client.api.connection;
    const sessionID = client.state.sessionID;
    const scope = props.scope ?? 0;
    const controller = new AbortController(); recommendation = controller;
    setBusy('default-small'); setError('');
    try {
      const model = await client.api.request<SelectedModel>(client.api.route(`/agent/default-small-model?provider_id=${encodeURIComponent(provider.id)}`), 'GET', undefined, controller.signal);
      if (controller.signal.aborted) return;
      if (connection !== client.api.connection || workspaceID !== client.state.workspace?.id || sessionID !== client.state.sessionID) throw new Error('The workspace or session changed. Choose the default small model again.');
      if (model.provider !== provider.id) throw new Error('The default small model belongs to a different provider.');
      await client.chooseModel(provider, model, scope, 'small');
      client.toast(`Small model changed to ${model.model}.`, 'success');
      props.close();
    } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); }
    finally { if (!controller.signal.aborted) setBusy(''); }
  }
  const entries = createMemo(() => {
    const q = query().toLowerCase().trim();
    const matches = (entry: Entry) => !q || `${entry.provider.name} ${entry.model.name} ${entry.model.id}`.toLowerCase().includes(q);
    const recent: Entry[] = [];
    for (const item of client.state.workspace?.config.recent_models?.[slot()] ?? []) {
      const provider = providers().find(candidate => candidate.id === item.provider);
      const model = provider?.models?.find(candidate => candidate.id === item.model);
      if (provider && model) recent.push({ provider, model, recent: true });
    }
    const groups = providers().map(provider => ({ provider, items: (provider.models ?? []).map(model => ({ provider, model })).filter(matches) })).filter(group => group.items.length);
    return { recent: q ? [] : recent.filter(matches), groups };
  });
  const flat = createMemo(() => [...entries().recent, ...entries().groups.flatMap(group => group.items)]);
  async function choose(entry: Entry) {
    if (busy()) return;
    if (!entry.provider.available || !entry.provider.owner) { setError(entry.provider.diagnostic || `${entry.provider.name} is not available. Sign in from Settings → Providers.`); return; }
    const current = selected();
    const model = current?.provider === entry.provider.id && current.model === entry.model.id ? current : { provider: entry.provider.id, model: entry.model.id };
    setBusy(`${entry.provider.id}/${entry.model.id}`); setError('');
    try {
      await client.chooseModel(entry.provider, model, props.scope ?? 0, slot());
      client.toast(`${slot() === 'large' ? 'Main' : 'Small'} model changed to ${entry.model.name || entry.model.id}.`, 'success');
      props.close();
    } catch (reason) { setError(errorText(reason)); }
    finally { setBusy(''); }
  }
  const isSelected = (entry: Entry) => selected()?.provider === entry.provider.id && selected()?.model === entry.model.id;
  const item = (entry: Entry) => {
    const index = () => flat().indexOf(entry);
    return <button type="button" role="option" class="model-option" aria-selected={isSelected(entry)} classList={{ active: index() === active(), unavailable: !entry.provider.available }} disabled={Boolean(busy())} onMouseEnter={() => setActive(index())} onClick={() => void choose(entry)}>
      <span class="model-name">{entry.model.name || entry.model.id}<Show when={entry.recent}><small>{entry.provider.name}</small></Show></span>
      <span class="model-meta"><Show when={entry.model.context_window}>{Math.round(entry.model.context_window / 1000)}k</Show><Show when={entry.model.can_reason}><Icon name="spark" size={12} /></Show><Show when={entry.model.supports_attachments}><Icon name="eye" size={12} /></Show></span>
      <Show when={isSelected(entry)}><Icon name="check" size={14} /></Show>
    </button>;
  };
  return <div ref={root} class="popover model-popover" role="dialog" aria-label="Choose model" onKeyDown={event => {
    const list = flat();
    if (event.key === 'Escape') { event.stopPropagation(); props.close(); }
    if (event.key === 'ArrowDown') { event.preventDefault(); setActive(Math.min(list.length - 1, active() + 1)); }
    if (event.key === 'ArrowUp') { event.preventDefault(); setActive(Math.max(0, active() - 1)); }
    if (event.key === 'Enter' && list[active()]) { event.preventDefault(); void choose(list[active()]); }
  }}>
    <div class="popover-head">
      <Segmented label="Model slot" value={slot()} onChange={value => { if (busy()) return; setSlot(value); setActive(0); }} options={[{ value: 'large', label: 'Main' }, { value: 'small', label: 'Small' }]} />
      <div class="search"><Icon name="search" size={14} /><input ref={search} aria-label="Search models" placeholder="Search models…" value={query()} onInput={event => { setQuery(event.currentTarget.value); setActive(0); }} /></div>
    </div>
    <Show when={slot() === 'small' && mainProvider()}><button class="wide" disabled={Boolean(busy()) || !client.state.connected} onClick={() => void chooseDefaultSmall()}>Use {mainProvider()?.name} default small model</button></Show>
    <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
    <div class="popover-body" role="listbox">
      <Show when={entries().recent.length}><div class="group-label">Recent</div><For each={entries().recent}>{item}</For></Show>
      <For each={entries().groups} fallback={<p class="muted pad">No models match.</p>}>{group => <>
        <div class="group-label"><span>{group.provider.name}</span><Show when={!group.provider.available}><span class="bad">{group.provider.availability || 'unavailable'}</span></Show></div>
        <For each={group.items}>{item}</For>
      </>}</For>
    </div>
    <p class="popover-foot">{props.scope === 1 ? 'Saved to this workspace' : 'Saved globally'}{client.state.sessionID ? ' and applied to this session.' : ' and applied to the agent.'}</p>
  </div>;
}
