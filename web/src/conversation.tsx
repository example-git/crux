import { createEffect, createMemo, createSignal, For, Index, Match, onCleanup, Show, Switch } from 'solid-js';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import type { Message, Part } from './types.ts';
import { taskNotification } from './notifications.ts';
import { Icon } from './ui.tsx';

export function Markdown(props: { text: string; streaming?: boolean; class?: string }) {
  const [html, setHTML] = createSignal('');
  let timer: ReturnType<typeof setTimeout> | undefined;
  let latest = '';
  const render = () => {
    timer = undefined;
    setHTML(DOMPurify.sanitize(marked.parse(latest, { async: false, breaks: true, gfm: true }), { FORBID_TAGS: ['style', 'form', 'input', 'button', 'iframe'], FORBID_ATTR: ['style'] })
      .replaceAll('<pre>', '<pre><button type="button" class="copy-code" data-copy-code="true">Copy</button>'));
  };
  createEffect(() => {
    latest = props.text;
    if (!props.streaming) { clearTimeout(timer); render(); }
    else if (!timer) timer = setTimeout(render, 80);
  });
  onCleanup(() => clearTimeout(timer));
  return <div class={`markdown ${props.class ?? ''}`} innerHTML={html()} onClick={event => {
    const target = event.target as HTMLElement;
    const button = target.closest<HTMLButtonElement>('button[data-copy-code]');
    if (button) {
      const code = button.parentElement?.querySelector('code')?.textContent ?? '';
      void navigator.clipboard.writeText(code).then(() => { button.textContent = 'Copied'; setTimeout(() => { button.textContent = 'Copy'; }, 1500); }).catch(() => { button.textContent = 'Copy failed'; });
    }
    const anchor = target.closest('a');
    if (anchor) { anchor.target = '_blank'; anchor.rel = 'noopener noreferrer'; }
  }} />;
}

function parseInput(input: string): Record<string, any> {
  try { const value = JSON.parse(input); return value && typeof value === 'object' ? value : {}; } catch { return {}; }
}

const toolLabels: Record<string, string> = {
  bash: 'Run', view: 'Read', edit: 'Edit', multiedit: 'Edit', write: 'Write', grep: 'Search', glob: 'Find files', ls: 'List', fetch: 'Fetch',
  agentic_fetch: 'Research', agent: 'Agent', download: 'Download', todos: 'Update todos', sourcegraph: 'Code search', web_search: 'Web search',
};

export function toolSummary(name: string, input: Record<string, any>) {
  const value = input.command ?? input.file_path ?? input.path ?? input.pattern ?? input.url ?? input.query ?? input.description ?? input.prompt ?? input.symbol ?? '';
  return String(value).split('\n')[0].slice(0, 200);
}

function EditDiff(props: { input: Record<string, any> }) {
  const edits = () => Array.isArray(props.input.edits) ? props.input.edits : props.input.old_string !== undefined || props.input.new_string !== undefined ? [props.input] : props.input.content !== undefined ? [{ old_string: '', new_string: props.input.content }] : [];
  return <Show when={edits().length}><div class="diff"><For each={edits()}>{edit => <div class="diff-hunk">
    <For each={String(edit.old_string ?? '').split('\n').filter((line, index, all) => line || all.length > 1)}>{line => <div class="del">- {line}</div>}</For>
    <For each={String(edit.new_string ?? '').split('\n')}>{line => <div class="add">+ {line}</div>}</For>
  </div>}</For></div></Show>;
}

function ToolCard(props: { call: Part; result?: Part; showOutput?: boolean; openTask?: (id: string) => void }) {
  const [open, setOpen] = createSignal(false);
  const input = createMemo(() => parseInput(props.call.data.input ?? ''));
  const name = () => props.call.data.name as string;
  const taskID = createMemo(() => {
    const metadata = parseInput(props.result?.data.metadata ?? '');
    const id = metadata.task_id ?? metadata.shell_id;
    return typeof id === 'string' && id ? id : undefined;
  });
  const status = () => props.result ? props.result.data.is_error ? 'failed' : 'done' : props.call.data.finished ? 'running' : 'preparing';
  const isEdit = () => ['edit', 'multiedit', 'write'].includes(name());
  return <div class={`tool ${status()}`}>
    <button class="tool-head" aria-expanded={open()} onClick={() => setOpen(!open())}>
      <span class={`tool-state ${status()}`} aria-label={status()} />
      <span class="tool-name" title={name()}>{toolLabels[name()] ?? name()}</span>
      <code class="tool-summary" title={toolSummary(name(), input())}>{toolSummary(name(), input())}</code>
      <Icon name={open() ? 'down' : 'right'} size={13} />
    </button>
    <Show when={open()}><div class="tool-body">
      <Show when={isEdit()} fallback={<pre class="tool-input">{JSON.stringify(input(), null, 2) || props.call.data.input}</pre>}><EditDiff input={input()} /></Show>
      <Show when={props.showOutput !== false && props.result}>{result => <>
        <Show when={result().data.data && String(result().data.mime_type ?? '').startsWith('image/')}><img class="inline-image" src={`data:${result().data.mime_type};base64,${result().data.data}`} alt={`${name()} result`} loading="lazy" /></Show>
        <Show when={result().data.content}><pre class="tool-output" classList={{ error: result().data.is_error }}>{result().data.content}</pre></Show>
        <Show when={taskID() && props.openTask}><button class="ghost small" onClick={() => props.openTask!(taskID()!)}><Icon name="external" size={13} />Inspect task</button></Show>
      </>}</Show>
    </div></Show>
  </div>;
}

const finishText: Record<string, string> = {
  canceled: 'Stopped', error: 'Turn failed', max_tokens: 'Output limit reached', length: 'Output limit reached',
  content_filter: 'The provider declined this response', 'content-filter': 'The provider declined this response',
};

function PartView(props: { part: Part; streaming: boolean; results: Map<string, Part>; calls?: Map<string, Part>; localResults?: Set<string>; openTask?: (id: string) => void }) {
  const data = () => props.part.data;
  return <Switch>
    <Match when={props.part.type === 'text' && data().text}><Markdown text={data().text} streaming={props.streaming} /></Match>
    <Match when={props.part.type === 'reasoning' && data().thinking}><details class="thinking"><summary><Icon name="spark" size={13} />{data().finished_at ? 'Thought' : 'Thinking…'}<Show when={data().finished_at && data().started_at}> for {Math.max(1, Math.round(data().finished_at - data().started_at))}s</Show></summary><Markdown text={data().thinking} streaming={props.streaming} /></details></Match>
    <Match when={props.part.type === 'tool_call'}><ToolCard call={props.part} result={props.results.get(data().id)} showOutput={props.localResults?.has(data().id) ?? true} openTask={props.openTask} /></Match>
    <Match when={props.part.type === 'tool_result'}><ToolCard call={props.calls?.get(data().tool_call_id) ?? { type: 'tool_call', data: { id: data().tool_call_id, name: data().name, input: '{}', finished: true } }} result={props.part} openTask={props.openTask} /></Match>
    <Match when={props.part.type === 'shell_command'}><details class="shell-entry"><summary><Icon name="terminal" size={13} /><code>$ {data().command}</code><span classList={{ bad: data().exit_code !== 0 }}>exit {data().exit_code}</span></summary><pre>{data().output || '(no output)'}</pre></details></Match>
    <Match when={props.part.type === 'image_url'}><img class="inline-image" src={data().url} alt="Attachment" loading="lazy" /></Match>
    <Match when={props.part.type === 'binary'}><Show when={String(data().MIMEType ?? data().mime_type ?? '').startsWith('image/')} fallback={<span class="file-chip"><Icon name="file" size={13} />{data().Path || data().path || data().MIMEType || 'Attachment'}</span>}><img class="inline-image" src={`data:${data().MIMEType ?? data().mime_type};base64,${data().Data ?? data().data}`} alt={data().Path || 'Image attachment'} loading="lazy" /></Show></Match>
    <Match when={props.part.type === 'skill'}><span class="file-chip"><Icon name="bolt" size={13} />{data().name}</span></Match>
    <Match when={props.part.type === 'retrying'}><p class="muted pulse">The provider is retrying…</p></Match>
    <Match when={props.part.type === 'finish' && !(data().message === 'Interrupted by steering' && !data().details && ['end_turn', 'tool_use'].includes(data().reason)) && (data().message || data().details || !['stop', 'end_turn', 'tool_use', 'tool-calls', 'tool_calls'].includes(data().reason))}>
      <div class="turn-status" classList={{ bad: data().reason === 'error' }}><Icon name={data().reason === 'canceled' ? 'stop' : 'warning'} size={14} /><div><strong>{finishText[data().reason] ?? `Turn ended: ${data().reason}`}</strong><Show when={data().message}><p>{data().message}</p></Show><Show when={data().details}><details><summary>Details</summary><pre>{data().details}</pre></details></Show></div></div>
    </Match>
  </Switch>;
}

export function messageText(message: Message) { return message.parts.filter(part => part.type === 'text').map(part => part.data.text ?? '').join('\n'); }

export function MessageView(props: { message: Message; results: Map<string, Part>; calls: Map<string, Part>; tint?: string; highlighted?: boolean; rewind?: () => void; openTask?: (id: string) => void }) {
  const [copied, setCopied] = createSignal(false);
  const notification = createMemo(() => taskNotification(props.message));
  const streaming = createMemo(() => props.message.role === 'assistant' && !props.message.parts?.some(part => part.type === 'finish'));
  const localResults = createMemo(() => {
    const calls = new Set(props.message.parts.filter(part => part.type === 'tool_call').map(part => part.data.id));
    return new Set(props.message.parts.filter(part => part.type === 'tool_result' && calls.has(part.data.tool_call_id)).map(part => part.data.tool_call_id as string));
  });
  const visibleParts = createMemo(() => props.message.parts.filter(part => {
    if (part.type === 'tool_result') return !localResults().has(part.data.tool_call_id);
    if (part.type !== 'finish') return true;
    const { reason, message, details } = part.data;
    if (message === 'Interrupted by steering' && !details && ['end_turn', 'tool_use'].includes(reason)) return false;
    return message || details || !['stop', 'end_turn', 'tool_use', 'tool-calls', 'tool_calls'].includes(reason);
  }));
  const copy = () => void navigator.clipboard.writeText(messageText(props.message)).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); });
  return <Show when={!notification()} fallback={<TaskNotice item={notification()!} id={props.message.id} highlighted={props.highlighted} openTask={props.openTask} />}>
    <Show when={visibleParts().length || props.message.is_summary_message}>
      <article class={`message ${props.message.role}`} classList={{ highlighted: props.highlighted, 'summary-message': props.message.is_summary_message }} style={props.tint ? { '--message-tint': props.tint } : undefined} data-message-id={props.message.id}>
        <Show when={props.message.is_summary_message} fallback={<Show when={props.message.role === 'user'} fallback={<div class="message-body"><Index each={visibleParts()}>{part => <PartView part={part()} streaming={streaming()} results={props.results} calls={props.calls} localResults={localResults()} openTask={props.openTask} />}</Index></div>}>
          <div class="bubble"><Index each={visibleParts()}>{part => <PartView part={part()} streaming={false} results={props.results} />}</Index></div>
        </Show>}>
          <details class="conversation-summary" open={Boolean(props.highlighted) || streaming()}>
            <summary>Conversation summary<Show when={streaming()}><span class="muted pulse"> · Summarizing…</span></Show></summary>
            <div class="message-body"><Index each={visibleParts().filter(part => part.type !== 'finish')}>{part => <PartView part={part()} streaming={streaming()} results={props.results} calls={props.calls} localResults={localResults()} openTask={props.openTask} />}</Index></div>
          </details>
          <Index each={visibleParts().filter(part => part.type === 'finish')}>{part => <PartView part={part()} streaming={false} results={props.results} />}</Index>
        </Show>
        <div class="message-actions">
          <Show when={props.message.role !== 'tool'}><button class="ghost small" onClick={copy} aria-label="Copy message"><Icon name={copied() ? 'check' : 'copy'} size={13} />{copied() ? 'Copied' : 'Copy'}</button></Show>
          <Show when={props.rewind}><button class="ghost small" onClick={props.rewind} title="Remove this message and everything after it"><Icon name="undo" size={13} />Rewind</button></Show>
          <Show when={props.message.role === 'assistant' && props.message.model}><span class="model-tag">{props.message.model}</span></Show>
        </div>
      </article>
    </Show>
  </Show>;
}

function TaskNotice(props: { item: NonNullable<ReturnType<typeof taskNotification>>; id: string; highlighted?: boolean; openTask?: (id: string) => void }) {
  const failed = () => ['failed', 'lost'].includes(props.item.status);
  return <article class="message notice" classList={{ highlighted: props.highlighted }} data-message-id={props.id} aria-label="Background task notification">
    <details class="task-notice" classList={{ bad: failed() }}>
      <summary><Icon name="bolt" size={13} /><strong>{props.item.title}</strong><span class="muted">{props.item.summary || props.item.id}</span><span class={`pill ${failed() ? 'bad' : props.item.status === 'completed' ? 'good' : ''}`}>{props.item.status}</span></summary>
      <div class="task-notice-body">
        <Show when={props.item.error}><p class="bad">{props.item.error}</p></Show>
        <Show when={props.item.lostReason && props.item.lostReason !== props.item.error}><p class="bad">{props.item.lostReason}</p></Show>
        <Show when={props.item.result}><Show when={props.item.type === 'shell'} fallback={<Markdown text={props.item.result!} />}><pre>{props.item.result}</pre></Show></Show>
        <Show when={props.openTask}><button class="ghost small" onClick={() => props.openTask!(props.item.id)}><Icon name="external" size={13} />Inspect task</button></Show>
      </div>
    </details>
  </article>;
}

export function pairResults(messages: Message[]) {
  const results = new Map<string, Part>();
  const calls = new Map<string, Part>();
  for (const message of messages) for (const part of message.parts ?? []) {
    if (part.type === 'tool_result') results.set(part.data.tool_call_id, part);
    if (part.type === 'tool_call') calls.set(part.data.id, part);
  }
  return { results, calls };
}
