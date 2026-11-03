import { createMemo, createSignal, For, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import { Icon, Kbd, Modal, relativeTime } from './ui.tsx';

export type Command = { id: string; name: string; detail?: string; icon: string; shortcut?: string; group: string; run: () => void };

export function Palette(props: { client: CruxState; commands: Command[]; close: () => void }) {
  const [query, setQuery] = createSignal('');
  const [active, setActive] = createSignal(0);
  const items = createMemo(() => {
    const q = query().toLowerCase().trim();
    const commands = props.commands.filter(item => !q || `${item.name} ${item.detail ?? ''} ${item.group}`.toLowerCase().includes(q));
    const sessions = q ? props.client.state.sessions.filter(session => !session.parent_session_id && (session.title || '').toLowerCase().includes(q)).slice(0, 8).map(session => ({
      id: `session:${session.id}`, name: session.title || 'Untitled session', detail: relativeTime(session.updated_at), icon: 'chat', group: 'Sessions',
      run: () => void props.client.attempt(() => props.client.selectSession(session.id)),
    } as Command)) : [];
    return [...commands, ...sessions];
  });
  const run = (item?: Command) => { if (!item) return; props.close(); item.run(); };
  return <Modal title="Command palette" close={props.close} size="medium" class="palette">
    <div class="palette-search"><Icon name="search" /><input autofocus aria-label="Search commands" placeholder="Search commands…" value={query()} onInput={event => { setQuery(event.currentTarget.value); setActive(0); }} onKeyDown={event => {
      if (event.key === 'ArrowDown') { event.preventDefault(); setActive(Math.min(items().length - 1, active() + 1)); }
      if (event.key === 'ArrowUp') { event.preventDefault(); setActive(Math.max(0, active() - 1)); }
      if (event.key === 'Enter') { event.preventDefault(); run(items()[active()]); }
    }} /></div>
    <div class="palette-list" role="listbox">
      <For each={items()} fallback={<p class="muted pad">Nothing matches.</p>}>{(item, index) => <>
        <Show when={index() === 0 || items()[index() - 1].group !== item.group}><div class="group-label">{item.group}</div></Show>
        <button role="option" aria-selected={index() === active()} classList={{ active: index() === active() }} onMouseEnter={() => setActive(index())} onClick={() => run(item)}>
          <Icon name={item.icon} size={15} /><span class="label">{item.name}</span><span class="detail">{item.detail}</span><Show when={item.shortcut}><Kbd>{item.shortcut!}</Kbd></Show>
        </button>
      </>}</For>
    </div>
  </Modal>;
}
