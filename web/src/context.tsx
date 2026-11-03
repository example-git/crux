import { createEffect, createSignal, For, onCleanup, onMount, Show } from 'solid-js';
import type { CruxState } from './state.ts';
import type { Attachment, MCPPrompt } from './types.ts';
import { Markdown } from './conversation.tsx';
import { errorText, Icon, Modal, Segmented } from './ui.tsx';

type Resource = { mcp_name: string; uri: string; name: string; mime_type?: string; description?: string };
type Content = { uri: string; text?: string; blob?: string; mime_type?: string };

export function ContextDialog(props: { client: CruxState; prompt?: MCPPrompt; close: () => void; insert: (text: string, attachments: Attachment[]) => void }) {
  const { api } = props.client;
  const workspaceID = props.client.state.workspace?.id;
  const root = api.route('/mcp');
  const connection = api.connection;
  let disposed = false;
  onCleanup(() => { disposed = true; });
  const current = () => !disposed && api.connection === connection && api.workspace?.id === workspaceID;
  createEffect(() => { if (props.client.state.workspace?.id !== workspaceID || (props.client.state.workspace?.connection_name ?? '') !== connection) props.close(); });
  const [tab, setTab] = createSignal<'prompts' | 'resources'>('prompts');
  const [prompts, setPrompts] = createSignal<MCPPrompt[]>([]);
  const [resources, setResources] = createSignal<Resource[]>([]);
  const [query, setQuery] = createSignal('');
  const [selected, setSelected] = createSignal<MCPPrompt | undefined>(props.prompt);
  const [args, setArgs] = createSignal<Record<string, string>>({});
  const [preview, setPreview] = createSignal<{ text: string; files: Attachment[] }>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal('');
  async function run(action: () => Promise<void>) {
    if (busy() || !current()) return;
    setBusy(true); setError('');
    try { await action(); } catch (reason) { if (current()) setError(errorText(reason)); } finally { if (current()) setBusy(false); }
  }
  onMount(() => void run(async () => {
    const [list, entries] = await Promise.all([api.request<MCPPrompt[]>(`${root}/prompts`), api.request<Resource[]>(`${root}/resources`)]);
    if (!current()) return;
    setPrompts(list ?? []); setResources(entries ?? []);
  }));
  async function refreshCatalog() {
    await run(async () => {
      const kind = tab();
      const states = await api.request<Record<string, { state: string; error?: string }>>(`${root}/states`);
      const failures: string[] = [];
      for (const [name, state] of Object.entries(states)) {
        if (!current()) return;
        if (!['connected', 'error'].includes(state.state)) { failures.push(`${name}: ${state.error || state.state}`); continue; }
        await api.request(`${root}/refresh-${kind}`, 'POST', { name });
        if (!current()) return;
        const updated = await api.request<typeof states>(`${root}/states`);
        if (updated[name]?.error) failures.push(`${name}: ${updated[name].error}`);
      }
      if (!current()) return;
      if (kind === 'prompts') {
        const list = await api.request<MCPPrompt[]>(`${root}/prompts`);
        if (!current()) return;
        setPrompts(list ?? []);
      } else {
        const list = await api.request<Resource[]>(`${root}/resources`);
        if (!current()) return;
        setResources(list ?? []);
      }
      setSelected(undefined); setPreview(undefined);
      if (failures.length) setError(failures.join('\n'));
    });
  }
  const choose = (prompt: MCPPrompt) => { setSelected(prompt); setArgs({}); setPreview(undefined); };
  const filteredPrompts = () => prompts().filter(item => `${item.client_id} ${item.prompt_id} ${item.title ?? ''} ${item.description ?? ''}`.toLowerCase().includes(query().toLowerCase()));
  const filteredResources = () => resources().filter(item => `${item.mcp_name} ${item.name} ${item.uri}`.toLowerCase().includes(query().toLowerCase()));
  return <Modal title="MCP prompts & resources" subtitle="Resolve context from connected MCP servers and add it to your message." close={props.close} size="large">
    <div class="split">
      <aside class="split-list">
        <Segmented label="Context type" value={tab()} onChange={value => { if (busy()) return; setTab(value); setSelected(undefined); setPreview(undefined); }} options={[{ value: 'prompts', label: `Prompts ${prompts().length}` }, { value: 'resources', label: `Resources ${resources().length}` }]} />
        <button disabled={busy()} onClick={() => void refreshCatalog()}><Icon name="refresh" size={14} />Refresh catalog</button>
        <div class="search"><Icon name="search" size={14} /><input aria-label="Search MCP context" placeholder="Search…" value={query()} onInput={event => setQuery(event.currentTarget.value)} /></div>
        <div class="list">
          <Show when={tab() === 'prompts'}><For each={filteredPrompts()} fallback={<p class="muted pad">{busy() ? 'Loading…' : 'No prompts.'}</p>}>{prompt => <button class="list-item" classList={{ active: selected()?.id === prompt.id }} disabled={busy()} onClick={() => choose(prompt)}><Icon name="chat" size={14} /><span><strong>{prompt.title || prompt.prompt_id}</strong><small>{prompt.client_id}{prompt.description ? ` · ${prompt.description}` : ''}</small></span></button>}</For></Show>
          <Show when={tab() === 'resources'}><For each={filteredResources()} fallback={<p class="muted pad">{busy() ? 'Loading…' : 'No resources.'}</p>}>{resource => <button class="list-item" disabled={busy()} onClick={() => void run(async () => {
            setSelected(undefined); setPreview(undefined);
            const contents = await api.request<Content[]>(`${root}/read-resource`, 'POST', { name: resource.mcp_name, uri: resource.uri });
            if (!current()) return;
            setPreview({
              text: contents.filter(item => item.text !== undefined).map(item => `## ${item.uri}\n\n${item.text}`).join('\n\n'),
              files: contents.filter(item => item.blob).map((item, index) => ({ file_name: `${resource.name}${contents.length > 1 ? `-${index + 1}` : ''}`, mime_type: item.mime_type || 'application/octet-stream', content: item.blob! })),
            });
          })}><Icon name="file" size={14} /><span><strong>{resource.name}</strong><small>{resource.mcp_name} · {resource.uri}</small></span></button>}</For></Show>
        </div>
      </aside>
      <section class="split-detail">
        <Show when={error()}><p class="form-error" role="alert">{error()}</p></Show>
        <Show when={selected()}>{prompt => <form class="stack" onSubmit={event => { event.preventDefault(); void run(async () => {
          setPreview(undefined);
          const result = await api.request<{ prompt: string }>(`${root}/get-prompt`, 'POST', { client_id: prompt().client_id, prompt_id: prompt().prompt_id, args: args() });
          if (current()) setPreview({ text: result.prompt, files: [] });
        }); }}>
          <div><h3>{prompt().title || prompt().prompt_id}</h3><p class="muted">{prompt().client_id}{prompt().description ? ` · ${prompt().description}` : ''}</p></div>
          <For each={prompt().arguments}>{arg => <label class="field"><span>{arg.title || arg.id}{arg.required ? ' *' : ''}</span><input aria-label={`Prompt argument ${arg.id}`} required={arg.required} disabled={busy()} value={args()[arg.id] ?? ''} onInput={event => { setArgs(value => ({ ...value, [arg.id]: event.currentTarget.value })); setPreview(undefined); }} /><Show when={arg.description}><small>{arg.description}</small></Show></label>}</For>
          <div class="actions"><button class="primary" disabled={busy()}>Resolve prompt</button></div>
        </form>}</Show>
        <Show when={preview()}>{value => <div class="stack">
          <div class="preview-box"><Markdown text={value().text || '_No text content._'} /><For each={value().files}>{file => <span class="chip"><Icon name="file" size={13} />{file.file_name}</span>}</For></div>
          <div class="actions"><button class="primary" disabled={busy()} onClick={() => { if (!current()) return; props.insert(value().text, value().files); props.close(); }}>Add to message</button></div>
        </div>}</Show>
        <Show when={!selected() && !preview()}><p class="muted pad">Select a prompt or resource.</p></Show>
      </section>
    </div>
  </Modal>;
}
