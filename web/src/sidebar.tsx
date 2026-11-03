import { createEffect, createMemo, createSignal, For, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Session } from './types.ts';
import { Icon, Kbd, Menu, relativeTime } from './ui.tsx';

function group(session: Session) {
  const day = 86400;
  const age = Date.now() / 1000 - session.updated_at;
  if (age < day) return 'Today';
  if (age < 2 * day) return 'Yesterday';
  if (age < 7 * day) return 'This week';
  if (age < 30 * day) return 'This month';
  return 'Older';
}

export function SessionList(props: { client: CruxState; rename: (session: Session) => void; remove: (session: Session) => void; close: () => void }) {
  const { client } = props;
  const { state } = client;
  const [query, setQuery] = createSignal('');
  const [filter, setFilter] = createSignal<'all' | 'running' | 'archived'>('all');
  const [menu, setMenu] = createSignal('');
  createEffect(() => {
    if (state.archived) setFilter('archived');
    else setFilter(value => value === 'archived' ? 'all' : value);
  });
  const sessions = createMemo(() => {
    const q = query().toLowerCase();
    return state.sessions
      .filter(session => !session.parent_session_id && (session.title || 'Untitled').toLowerCase().includes(q) && (filter() !== 'running' || session.is_busy))
      .sort((a, b) => b.updated_at - a.updated_at);
  });
  const groups = createMemo(() => {
    const result: { label: string; items: Session[] }[] = [];
    for (const session of sessions()) {
      const label = group(session);
      const last = result.at(-1);
      if (last?.label === label) last.items.push(session);
      else result.push({ label, items: [session] });
    }
    return result;
  });
  const setView = (value: 'all' | 'running' | 'archived') => client.attempt(async () => {
    setFilter(value);
    if ((value === 'archived') !== state.archived) { client.set('archived', value === 'archived'); await client.refreshSessions(); }
  });
  const open = (id: string) => { props.close(); void client.attempt(() => client.selectSession(id)); };
  return <div class="session-browser" aria-label="Sessions">
    <div class="session-tools">
      <button class="primary" disabled={!state.connected} onClick={() => { props.close(); void client.attempt(() => client.selectSession('')); }}><Icon name="plus" size={14} />New session<Kbd>⌘N</Kbd></button>
    </div>
    <div class="search"><Icon name="search" size={14} /><input aria-label="Search sessions" placeholder="Search sessions" value={query()} onInput={event => setQuery(event.currentTarget.value)} /></div>
    <div class="tabs small" role="tablist" aria-label="Session filter">
      <For each={[['all', 'All'], ['running', 'Running'], ['archived', 'Archived']] as const}>{([value, label]) => <button role="tab" aria-selected={filter() === value} onClick={() => setView(value)}>{label}</button>}</For>
    </div>
    <nav class="session-list">
      <For each={groups()} fallback={<p class="muted pad">{state.loading ? 'Loading…' : query() ? 'No matching sessions.' : filter() === 'archived' ? 'No archived sessions.' : 'No sessions yet.'}</p>}>{section => <>
        <div class="group-label">{section.label}</div>
        <For each={section.items}>{session => <div class="session" classList={{ active: session.id === state.sessionID }}>
          <button class="session-main" onClick={() => open(session.id)}>
            <span class="session-icon"><Show when={session.is_busy} fallback={<Icon name={session.goal?.objective ? 'target' : session.forked_from_session_id ? 'branch' : 'chat'} size={14} />}><span class="live-dot" /></Show></span>
            <span class="session-title">{session.title || 'Untitled session'}</span>
            <time>{relativeTime(session.updated_at)}</time>
          </button>
          <div class="anchor">
            <button class="ghost icon session-more" aria-label={`Actions for ${session.title || 'session'}`} onClick={() => setMenu(menu() === session.id ? '' : session.id)}><Icon name="more" size={14} /></button>
            <Menu open={menu() === session.id} close={() => setMenu('')}>
              <button role="menuitem" onClick={() => props.rename(session)}><Icon name="edit" size={14} />Rename</button>
              <button role="menuitem" onClick={() => client.attempt(() => client.forkSession(session.id))}><Icon name="branch" size={14} />Branch</button>
              <button role="menuitem" onClick={() => client.attempt(() => client.archiveSession(session.id, session.archived ?? false))}><Icon name="archive" size={14} />{session.archived ? 'Restore' : 'Archive'}</button>
              <button role="menuitem" class="danger" onClick={() => props.remove(session)}><Icon name="trash" size={14} />Delete…</button>
            </Menu>
          </div>
        </div>}</For>
      </>}</For>
    </nav>
  </div>;
}
