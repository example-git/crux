import { createEffect, createMemo, createSignal, For, on, onCleanup, Show, untrack, type JSX } from 'solid-js';
import { isPlanMode, parseGoalCommand, type CruxState } from './state.ts';
import type { Attachment, FileMatch, InputPart, MCPPrompt, Skill } from './types.ts';
import { parseTaskNotification } from './notifications.ts';
import { ContextDialog } from './context.tsx';
import { ModelPopover } from './models.tsx';
import { compact, errorText, Icon, Menu, Modal, readFiles } from './ui.tsx';
import { createDraftStore } from './drafts.ts';
import type { IndexStatus } from './integrations.tsx';

type CustomCommand = { id: string; name: string; content: string; arguments: { id: string; title: string; required: boolean }[] };

type Suggestion = { kind: 'command' | 'skill' | 'mcp' | 'file'; id: string; label: string; detail?: string; icon: string; run: () => void };

export type ComposerHandle = { insert: (text: string) => void; focus: () => void };

const builtins = [
  { id: 'goal', label: '/goal', detail: 'Set a goal: /goal <objective>, or status, pause, resume, stop, complete, clear', insert: '/goal ' },
  { id: 'plan', label: '/plan', detail: 'Toggle plan mode for this session' },
  { id: 'model', label: '/model', detail: 'Switch the main model' },
  { id: 'new', label: '/new', detail: 'Start a new session' },
  { id: 'summarize', label: '/summarize', detail: 'Summarize this session to free context' },
  { id: 'init', label: '/init', detail: 'Initialize the project instructions file' },
  { id: 'mcp', label: '/mcp', detail: 'Browse MCP prompts and resources' },
  { id: 'errors', label: '/errors', detail: 'Inspect and copy recent errors' },
];

export function Composer(props: { client: CruxState; sent: () => void; openShell: () => void; openErrors: () => void; openIndex: () => void; actions?: JSX.Element; ref?: (handle: ComposerHandle) => void }) {
  const { client } = props;
  const { state, api } = client;
  const [text, setText] = createSignal('');
  const [files, setFiles] = createSignal<Attachment[]>([]);
  const [loadingAttachments, setLoadingAttachments] = createSignal(0);
  const [skills, setSkills] = createSignal<Skill[]>([]);
  const [suggestions, setSuggestions] = createSignal<Suggestion[]>([]);
  const [active, setActive] = createSignal(0);
  const [ghost, setGhost] = createSignal('');
  const [focused, setFocused] = createSignal(false);
  const [contextOpen, setContextOpen] = createSignal<MCPPrompt | true | undefined>();
  const [modelOpen, setModelOpen] = createSignal(false);
  const [effortOpen, setEffortOpen] = createSignal(false);
  const [dragging, setDragging] = createSignal(false);
  const [catalog, setCatalog] = createSignal<{ skills: Skill[]; prompts: MCPPrompt[]; commands: CustomCommand[] }>();
  const [customCommand, setCustomCommand] = createSignal<CustomCommand>();
  const [commandArgs, setCommandArgs] = createSignal<Record<string, string>>({});
  const drafts = createDraftStore(() => client.toast('Drafts could not be saved in browser storage. Unsent text will be lost if this page reloads.', 'error'));
  let input!: HTMLTextAreaElement;
  let upload!: HTMLInputElement;
  let key = '';
  let submission: { key: string } | undefined;
  let searchGeneration = 0;
  let catalogGeneration = 0;
  const pendingAttachments = new Set<{ key: string }>();
  let history: string[] | undefined;
  let historyIndex = -1;
  let historyDraft = '';
  let suggestionAbort: AbortController | undefined;

  const session = () => state.currentSession;
  const busy = () => Boolean(session()?.is_busy);
  const archived = () => Boolean(session()?.archived);
  const model = createMemo(() => {
    const selected = client.selectedModel('large');
    const provider = state.workspace?.provider_surfaces?.find(item => item.id === selected?.provider);
    return { selected, provider, info: provider?.models?.find(item => item.id === selected?.model) };
  });
  const localErrors = () => text().trim() === '/errors' && !files().length && !skills().length && !loadingAttachments();
  const canSend = () => localErrors() || state.connected && Boolean(state.agent?.is_ready) && !state.sending && !loadingAttachments() && !archived() && Boolean(text().trim() || files().length || skills().length);

  const resize = () => { input.style.height = 'auto'; input.style.height = `${Math.min(320, input.scrollHeight)}px`; };
  const setValue = (value: string) => { setText(value); input.value = value; requestAnimationFrame(resize); };
  props.ref?.({ insert: value => { setValue(`${text()}${text() && !text().endsWith('\n') ? '\n' : ''}${value}`); input.focus(); }, focus: () => input.focus() });

  createEffect(() => {
    const next = JSON.stringify([state.workspace?.connection_name ?? '', state.workspace?.path ?? '', state.workspace?.data_dir ?? '', state.sessionID]);
    if (next === key) return;
    untrack(() => {
      if (key) drafts.set(key, { text: text(), files: files(), skills: skills() });
      const emptySessionKey = JSON.stringify([state.workspace?.connection_name ?? '', state.workspace?.path ?? '', state.workspace?.data_dir ?? '', '']);
      const migrated = key === emptySessionKey && state.createdSessionID === state.sessionID;
      let saved = drafts.get(migrated ? key : next);
      if (!saved && !state.workspace?.connection_name && state.workspace?.path) {
        const legacyKey = `${state.workspace.path}:${state.sessionID}`;
        saved = drafts.get(legacyKey);
        if (saved) {
          drafts.set(next, saved);
          drafts.delete(legacyKey);
        }
      }
      if (migrated) {
        if (submission?.key === key) submission.key = next;
        for (const pending of pendingAttachments) if (pending.key === key) pending.key = next;
        drafts.delete(key);
      }
      searchGeneration++;
      setSuggestions([]);
      setCustomCommand(undefined);
      setValue(saved?.text ?? ''); setFiles(saved?.files ?? []); setSkills(saved?.skills ?? []);
      if (saved?.omittedFiles) client.toast(`Restored the draft without ${saved.omittedFiles} attachment${saved.omittedFiles === 1 ? '' : 's'} that did not fit in browser storage.`);
      history = undefined; historyIndex = -1; setGhost('');
      key = next;
    });
  });
  createEffect(on(() => [text(), files(), skills()] as const, ([value, attached, chosen]) => {
    if (key && state.workspace?.path) drafts.set(key, { text: value, files: attached, skills: chosen });
  }, { defer: true }));
  onCleanup(() => drafts.flush());

  async function loadCatalog() {
    if (catalog()) return catalog()!;
    const workspace = state.workspace?.id;
    const generation = catalogGeneration;
    const failures: string[] = [];
    const failed = (source: string, reason: unknown) => { failures.push(`${source}: ${errorText(reason)}`); return []; };
    const [skillList, prompts, commands] = await Promise.all([
      api.request<Skill[]>(api.route('/skills')).catch(reason => failed('Skills', reason)),
      api.request<MCPPrompt[]>(api.route('/mcp/prompts')).catch(reason => failed('MCP prompts', reason)),
      api.request<CustomCommand[]>(`/ui/workspaces/${workspace}/commands`).catch(reason => failed('Custom commands', reason)),
    ]);
    const value = { skills: (skillList ?? []).filter(skill => skill.user_invocable), prompts: prompts ?? [], commands: commands ?? [] };
    if (state.workspace?.id === workspace && generation === catalogGeneration) {
      if (failures.length) client.toast(failures.join('; '), 'error');
      else setCatalog(value);
    }
    return value;
  }
  createEffect(on(() => [state.workspace?.connection_name, state.workspace?.id, state.skillsRevision, state.servicesRevision], () => {
    catalogGeneration++;
    searchGeneration++;
    setCatalog(undefined);
    setSuggestions([]);
  }, { defer: true }));

  function token() {
    const position = input.selectionStart ?? text().length;
    const before = text().slice(0, position);
    const slash = /^\/([\w:.-]*)$/.exec(before);
    if (slash) return { kind: 'slash' as const, query: slash[1], start: 0, end: position };
    const mention = /(^|\s)@([^\s@]*)$/.exec(before);
    if (mention) return { kind: 'mention' as const, query: mention[2], start: position - mention[2].length - 1, end: position };
  }

  function replaceRange(start: number, end: number, value: string) {
    const next = text().slice(0, start) + value + text().slice(end);
    setValue(next);
    requestAnimationFrame(() => { input.focus(); const caret = start + value.length; input.setSelectionRange(caret, caret); });
  }

  async function updateSuggestions() {
    const current = token();
    const generation = ++searchGeneration;
    if (!current) { setSuggestions([]); return; }
    if (current.kind === 'slash') {
      const { skills: skillList, prompts, commands } = await loadCatalog();
      if (generation !== searchGeneration) return;
      const query = current.query.toLowerCase();
      const items: Suggestion[] = [
        ...builtins.map(item => ({ kind: 'command' as const, id: item.id, label: item.label, detail: item.detail, icon: 'command', run: () => runBuiltin(item.id, item.insert) })),
        ...commands.map(command => ({ kind: 'command' as const, id: command.id, label: `/${command.name}`, detail: 'Custom command', icon: 'file', run: () => {
          if (command.arguments.length) { setCommandArgs({}); setCustomCommand(command); }
          else setValue(command.content);
        } })),
        ...skillList.map(skill => ({ kind: 'skill' as const, id: skill.id, label: `/${skill.label || skill.name}`, detail: skill.description, icon: 'bolt', run: () => { addSkill(skill); setValue(''); } })),
        ...prompts.map(prompt => ({ kind: 'mcp' as const, id: prompt.id, label: `/${prompt.client_id}:${prompt.prompt_id}`, detail: prompt.title || prompt.description, icon: 'plug', run: () => { setValue(''); setContextOpen(prompt); } })),
      ].filter(item => `${item.label} ${item.detail ?? ''}`.toLowerCase().includes(query)).slice(0, 40);
      setSuggestions(items); setActive(0);
      return;
    }
    await new Promise(resolve => setTimeout(resolve, 120));
    if (generation !== searchGeneration || !state.workspace) return;
    try {
      const result = await api.request<{ files: FileMatch[] }>(api.route('/files/search'), 'POST', { query: current.query, roots: [state.workspace.path], limit: 30 });
      if (generation !== searchGeneration) return;
      setSuggestions((result.files ?? []).map(file => {
        const path = file.path.startsWith('/') ? file.path : `${file.root}/${file.path}`;
        return { kind: 'file' as const, id: path, label: file.file_name, detail: file.path, icon: 'file', run: () => { void attachWorkspaceFile(path); } };
      }));
      setActive(0);
    } catch (reason) { if (generation === searchGeneration) { setSuggestions([]); client.toast(errorText(reason), 'error'); } }
  }

  function runBuiltin(id: string, insert?: string) {
    setSuggestions([]);
    if (insert) { setValue(insert); input.focus(); return; }
    setValue('');
    if (id === 'plan') void client.attempt(() => client.togglePlan());
    if (id === 'model') setModelOpen(true);
    if (id === 'new') void client.attempt(() => client.selectSession(''));
    if (id === 'summarize') void client.attempt(async () => { await client.summarize(); client.toast('Session summarized.', 'success'); });
    if (id === 'init') void client.attempt(async () => { await client.initializeProject(); client.toast('Project initialization started.', 'success'); });
    if (id === 'mcp') setContextOpen(true);
    if (id === 'errors') props.openErrors();
  }

  async function attachWorkspaceFile(path: string) {
    const pending = { key };
    const workspace = state.workspace;
    const current = token();
    const draftText = text();
    if (!workspace || current?.kind !== 'mention') return;
    pendingAttachments.add(pending);
    setLoadingAttachments(count => count + 1);
    try {
      const attachment = await api.request<Attachment>(`/ui/workspaces/${workspace.id}/files/attachment`, 'POST', { path });
      if (key === pending.key) {
        if (text() === draftText) replaceRange(current.start, current.end, `@${path} `);
        setFiles(items => [...items.filter(item => item.file_path !== attachment.file_path), attachment]);
      } else {
        const saved = drafts.get(pending.key) ?? { text: draftText, files: [], skills: [] };
        drafts.set(pending.key, { ...saved, files: [...saved.files.filter(item => item.file_path !== attachment.file_path), attachment] });
      }
    } catch (reason) { client.toast(errorText(reason), 'error'); }
    finally { pendingAttachments.delete(pending); setLoadingAttachments(count => count - 1); }
  }

  function addSkill(skill: Skill) { setSkills(items => items.some(item => item.id === skill.id) ? items : [...items, skill]); }

  async function attach(list: FileList | File[] | null) {
    if (!list?.length) return;
    const pending = { key };
    pendingAttachments.add(pending);
    setLoadingAttachments(count => count + 1);
    try {
      const added = await readFiles(list);
      if (key === pending.key) setFiles(items => [...items, ...added]);
      else { const saved = drafts.get(pending.key) ?? { text: '', files: [], skills: [] }; drafts.set(pending.key, { ...saved, files: [...saved.files, ...added] }); }
    } catch (reason) { client.toast(errorText(reason), 'error'); }
    finally { pendingAttachments.delete(pending); setLoadingAttachments(count => count - 1); }
    if (upload) upload.value = '';
  }

  async function submit() {
    if (!canSend()) return;
    const value = text();
    const trimmed = value.trim();
    const attached = files();
    const chosen = skills();
    const pending = { key };
    submission = pending;
    const clear = () => {
      if (key !== pending.key) {
        const saved = drafts.get(pending.key);
        if (saved) drafts.set(pending.key, { text: saved.text === value ? '' : saved.text, files: saved.files.filter(file => !attached.includes(file)), skills: saved.skills.filter(skill => !chosen.includes(skill)) });
        return;
      }
      if (text() === value) setValue('');
      setFiles(items => items.filter(file => !attached.includes(file)));
      setSkills(items => items.filter(skill => !chosen.includes(skill)));
      history = undefined; historyIndex = -1;
    };
    try {
      const builtin = builtins.find(item => item.label === trimmed && !item.insert);
      if (builtin && !attached.length && !chosen.length) { runBuiltin(builtin.id); return; }
      if (parseGoalCommand(trimmed) && !attached.length && !chosen.length) {
        const notice = await client.runGoal(trimmed);
        clear(); client.toast(notice, 'success');
        return;
      }
      if (trimmed.startsWith('!') && trimmed.length > 1 && !attached.length && !chosen.length) {
        await runShell(trimmed.slice(1).trim());
        clear();
        return;
      }
      const parts: InputPart[] = chosen.map(skill => ({ type: 'skill', text: skill.name, path: skill.id }));
      await client.send(value, attached, parts);
      clear(); props.sent(); input.focus();
    } catch (reason) { client.fail(reason); }
    finally { if (submission === pending) submission = undefined; }
  }

  async function runShell(command: string) {
    const workspace = state.workspace!;
    const connection = api.connection;
    const root = api.route('');
    const current = () => state.workspace?.id === workspace.id && api.connection === connection;
    const id = state.sessionID || await client.newSession();
    if (!current()) throw new Error('The workspace changed before the command could start.');
    const result = await api.request<{ output: string; exit_code: number }>(`${root}/agent/sessions/${id}/shell`, 'POST', { command, term_width: 120 });
    if (!current()) return;
    await client.refreshSessions();
    if (current() && state.sessionID === id) await client.loadMessages(id);
    if (result.exit_code !== 0) client.toast(`Command exited with ${result.exit_code}`, 'error');
    props.sent();
  }

  async function recall(direction: -1 | 1) {
    if (!history) {
      const original = key;
      const draft = text();
      try {
        const prompts = await client.promptHistory();
        if (key !== original || text() !== draft) return;
        history = prompts;
      } catch (reason) { client.toast(errorText(reason), 'error'); return; }
      historyIndex = history.length;
      historyDraft = text();
    }
    if (!history.length) return;
    const next = Math.max(0, Math.min(history.length, historyIndex + direction));
    if (next === historyIndex) return;
    historyIndex = next;
    setValue(next === history.length ? historyDraft : history[next]);
    requestAnimationFrame(() => { const end = direction < 0 ? 0 : input.value.length; input.setSelectionRange(end, end); });
  }

  const suggestionReady = () => focused() && !text() && !suggestions().length && Boolean(state.sessionID) && !busy() && Boolean(state.agent?.is_ready) && !archived() && state.connected && !state.permission && !state.question && (session()?.message_count ?? 0) > 0;
  createEffect(on(() => [suggestionReady(), state.sessionID, state.workspace?.id, session()?.message_count, session()?.updated_at, state.workspace?.connection_name] as const, ([ready, id, workspaceID]) => {
    suggestionAbort?.abort();
    setGhost('');
    if (!ready || !id) return;
    const controller = new AbortController();
    suggestionAbort = controller;
    const timer = setTimeout(() => {
      void client.suggest(id, controller.signal).then(value => { if (!controller.signal.aborted && suggestionReady() && state.sessionID === id && state.workspace?.id === workspaceID) setGhost(value); }).catch(() => {});
    }, 700);
    onCleanup(() => { clearTimeout(timer); controller.abort(); });
  }));

  function keydown(event: KeyboardEvent) {
    const list = suggestions();
    if (list.length) {
      if (event.key === 'ArrowDown') { event.preventDefault(); setActive((active() + 1) % list.length); return; }
      if (event.key === 'ArrowUp') { event.preventDefault(); setActive((active() - 1 + list.length) % list.length); return; }
      if (event.key === 'Enter' || event.key === 'Tab') { event.preventDefault(); list[active()]?.run(); setSuggestions([]); return; }
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setSuggestions([]); return; }
    }
    if (event.key === 'Tab' && !event.shiftKey && ghost() && !text()) { event.preventDefault(); setValue(ghost()); setGhost(''); return; }
    if (event.key === 'Tab' && event.shiftKey && state.connected && !archived()) { event.preventDefault(); void client.attempt(() => client.togglePlan()); return; }
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) { event.preventDefault(); void submit(); return; }
    if (event.key === 'ArrowUp' && (input.selectionStart ?? 0) === 0 && (input.selectionEnd ?? 0) === 0) { event.preventDefault(); void recall(-1); return; }
    if (event.key === 'ArrowDown' && history && historyIndex < history.length && input.selectionStart === input.value.length) { event.preventDefault(); void recall(1); return; }
    if (event.key === 'Escape' && busy() && !text()) { event.preventDefault(); void client.attempt(client.cancelRun); }
  }

  const placeholder = () => {
    if (archived()) return 'Restore this session to continue';
    if (!state.agent?.is_ready) return 'Choose a model in Settings to start';
    if (ghost()) return ghost();
    if (busy()) return state.deliveryMode === 'steer' ? 'Redirect the current turn…' : 'Queue a follow-up…';
    return 'Message Crux…';
  };
  const reasoning = createMemo(() => {
    const info = model().info;
    if (!info?.can_reason) return undefined;
    if (info.reasoning_levels?.length) return { kind: 'levels' as const, levels: info.reasoning_levels, value: model().selected?.reasoning_effort || info.default_reasoning_effort || '' };
    return { kind: 'think' as const, value: Boolean(model().selected?.think) };
  });
  const contextUse = createMemo(() => {
    const capacity = model().info?.context_window ?? state.agent?.model?.context_window ?? 0;
    const used = (session()?.prompt_tokens ?? 0) + (session()?.completion_tokens ?? 0) + (session()?.unseen_local_tokens ?? 0);
    return { used, percent: capacity ? Math.min(100, Math.round(used / capacity * 100)) : undefined };
  });
  const [indexStatus, setIndexStatus] = createSignal<IndexStatus>();
  const [indexError, setIndexError] = createSignal('');
  createEffect(on(() => [state.workspace?.id, state.connected, state.workspace?.connection_name] as const, ([workspaceID, connected]) => {
    setIndexStatus(undefined);
    setIndexError('');
    if (!workspaceID || !connected) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const load = async () => {
      try {
        const value = await api.request<IndexStatus>(api.route('/codebase-index'), 'GET', undefined, controller.signal);
        if (controller.signal.aborted) return;
        setIndexStatus(value);
        setIndexError('');
      } catch (reason) {
        if (controller.signal.aborted) return;
        setIndexError(errorText(reason));
      }
      if (!controller.signal.aborted) timer = setTimeout(() => void load(), 1500);
    };
    void load();
    onCleanup(() => { controller.abort(); clearTimeout(timer); });
  }));
  const status = createMemo(() => {
    if (!state.connected) return 'index offline';
    if (indexError()) return 'index unavailable';
    const value = indexStatus();
    if (!value) return 'checking index…';
    if (!value.enabled || value.state === 'disabled') return 'index disabled';
    if (value.credential_status === 'missing' || value.credential_status === 'invalid') return 'index login required';
    switch (value.state) {
      case 'indexing': return `${value.serving ? 'refreshing index' : 'indexing'}${value.files_total ? ` ${value.files_processed ?? 0}/${value.files_total}` : value.stage ? ` · ${value.stage}` : '…'}`;
      case 'ready': return 'index ready';
      case 'stale': return value.serving ? 'index ready · refresh pending' : 'index stale';
      case 'failed': return value.serving ? 'index ready · refresh failed' : 'index failed';
      case 'missing': return 'index not built';
      default: return 'checking index…';
    }
  });

  return <div class="composer-wrap">
    <Show when={state.queued.length}><section class="queue" aria-label="Pending prompts">
      <header><Icon name="queue" size={14} /><span>{state.queued.length} pending</span><button class="ghost small" onClick={() => client.attempt(client.clearQueue)}>Clear</button></header>
      <For each={state.queued}>{prompt => {
        const notice = createMemo(() => parseTaskNotification(prompt.prompt));
        return <div class="queued"><span class={`pill ${prompt.delivery_mode}`}>{notice() ? 'Task' : prompt.delivery_mode === 'steer' ? 'Steer' : 'Queue'}</span><p>{notice() ? `${notice()!.title} · ${notice()!.status}${notice()!.summary ? ` · ${notice()!.summary}` : ''}` : prompt.prompt}</p></div>;
      }}</For>
    </section></Show>
    <Show when={loadingAttachments()}><p class="muted small-text" role="status">Loading attachments…</p></Show>
    <form class="composer" classList={{ busy: busy(), steer: busy() && state.deliveryMode === 'steer', plan: isPlanMode(session()?.mode), dragging: dragging(), offline: !state.connected }}
      onSubmit={event => { event.preventDefault(); void submit(); }}
      onDragOver={event => { event.preventDefault(); setDragging(true); }} onDragLeave={() => setDragging(false)}
      onDrop={event => { event.preventDefault(); setDragging(false); void attach(event.dataTransfer?.files ?? null); }}>
      <button type="button" class="composer-status" aria-label={`Codebase indexing: ${status()}`} title={indexError() || indexStatus()?.error || 'Open codebase indexing settings'} onClick={props.openIndex}><span role="status" aria-live="polite">{status()}</span></button>
      <span class="composer-progress" aria-hidden="true" />
      <Show when={suggestions().length}><div class="suggestions" role="listbox" aria-label="Suggestions"><For each={suggestions()}>{(item, index) => <button type="button" role="option" aria-selected={index() === active()} classList={{ active: index() === active() }} onMouseEnter={() => setActive(index())} onMouseDown={event => event.preventDefault()} onClick={() => { item.run(); setSuggestions([]); }}><Icon name={item.icon} size={14} /><span class="label">{item.label}</span><span class="detail">{item.detail}</span></button>}</For></div></Show>
      <Show when={files().length || skills().length}><div class="chips">
        <For each={files()}>{(file, index) => <span class="chip"><Icon name={file.mime_type.startsWith('image/') ? 'eye' : 'file'} size={13} />{file.file_name}<button type="button" class="ghost icon" aria-label={`Remove ${file.file_name}`} onClick={() => setFiles(items => items.filter((_, i) => i !== index()))}><Icon name="close" size={11} /></button></span>}</For>
        <For each={skills()}>{skill => <span class="chip skill"><Icon name="bolt" size={13} />{skill.name}<button type="button" class="ghost icon" aria-label={`Remove ${skill.name}`} onClick={() => setSkills(items => items.filter(item => item.id !== skill.id))}><Icon name="close" size={11} /></button></span>}</For>
      </div></Show>
      <textarea ref={input} rows={1} aria-label="Message Crux" placeholder={placeholder()} classList={{ ghosted: Boolean(ghost()) && !text() }} disabled={!state.connected || archived()}
        onInput={event => { setText(event.currentTarget.value); setGhost(''); historyIndex = history?.length ?? -1; resize(); void updateSuggestions(); }}
        onKeyDown={keydown} onFocus={() => setFocused(true)} onBlur={() => { setFocused(false); setTimeout(() => setSuggestions([]), 120); }}
        onClick={() => void updateSuggestions()}
        onPaste={event => { if (event.clipboardData?.files.length) { event.preventDefault(); void attach(event.clipboardData.files); } }} />
      <Show when={ghost() && !text() && focused()}><span class="ghost-hint">Tab to accept</span></Show>
      <div class="composer-bar">
        <input ref={upload} hidden type="file" multiple onChange={event => void attach(event.currentTarget.files)} />
        <button type="button" class="ghost icon" title="Attach files" aria-label="Attach files" disabled={!state.connected} onClick={() => upload.click()}><Icon name="attach" /></button>
        <button type="button" class="ghost icon" title="MCP prompts and resources" aria-label="MCP prompts and resources" disabled={!state.connected} onClick={() => setContextOpen(true)}><Icon name="plug" /></button>
        <select class="mode-select" aria-label="Interaction mode" title="Plan applies to this session. YOLO auto-approves tools across this running workspace." classList={{ plan: isPlanMode(session()?.mode), yolo: state.autoApprove }} value={isPlanMode(session()?.mode) ? state.autoApprove ? 'plan-yolo' : 'plan' : state.autoApprove ? 'yolo' : 'normal'} disabled={!state.connected || archived() || state.modeSaving} onChange={event => { const select = event.currentTarget; const value = select.value; void client.attempt(() => client.setInteractionMode(value)).finally(() => { select.value = isPlanMode(session()?.mode) ? state.autoApprove ? 'plan-yolo' : 'plan' : state.autoApprove ? 'yolo' : 'normal'; }); }}><option value="normal">Normal</option><option value="plan">Plan</option><option value="yolo">YOLO</option><option value="plan-yolo">Plan + YOLO</option></select>
        <div class="anchor">
          <button type="button" class="chip-button" aria-label="Choose model" aria-expanded={modelOpen()} onClick={() => setModelOpen(!modelOpen())}><Icon name="layers" size={14} /><span class="truncate">{model().info?.name || model().selected?.model || 'Choose model'}</span><Icon name="down" size={12} /></button>
          <Show when={modelOpen()}><ModelPopover client={client} close={() => setModelOpen(false)} /></Show>
        </div>
        <Show when={reasoning()}>{value => <div class="anchor">
          <Show when={value().kind === 'levels'} fallback={<button type="button" class="chip-button" classList={{ on: value().value === true }} aria-pressed={value().value === true} disabled={busy()} onClick={() => client.attempt(async () => { await client.setModelControl({ think: !value().value }); client.toast(`Thinking ${value().value ? 'disabled' : 'enabled'}.`, 'success'); })}><Icon name="spark" size={14} />Think</button>}>
            <button type="button" class="chip-button" aria-expanded={effortOpen()} disabled={busy()} onClick={() => setEffortOpen(!effortOpen())}><Icon name="spark" size={14} />{String(value().value || 'effort')}<Icon name="down" size={12} /></button>
            <Menu open={effortOpen()} close={() => setEffortOpen(false)} align="left"><span class="menu-label">Reasoning effort</span><For each={(value() as { levels: string[] }).levels}>{level => <button type="button" role="menuitemradio" aria-checked={value().value === level} onClick={() => client.attempt(async () => { await client.setModelControl({ reasoning_effort: level }); client.toast(`Reasoning effort set to ${level}.`, 'success'); })}><Icon name={value().value === level ? 'check' : 'spark'} size={14} />{level}</button>}</For></Menu>
          </Show>
        </div>}</Show>
        <Show when={state.sessionID && contextUse().used}><span class="chip-button context-chip" title={`${contextUse().used.toLocaleString()} tokens in context`} classList={{ warn: (contextUse().percent ?? 0) > 70, bad: (contextUse().percent ?? 0) > 90 }}><Icon name="database" size={13} />{contextUse().percent !== undefined ? `${contextUse().percent}%` : compact(contextUse().used)}</span></Show>
        <span class="spacer" />
        <Show when={busy()}><div class="segmented compact" role="radiogroup" aria-label="Delivery mode">
          <button type="button" role="radio" aria-checked={state.deliveryMode === 'queue'} title="Run after the current turn" disabled={state.deliverySaving} onClick={() => client.attempt(() => client.setDeliveryMode('queue'))}>Queue</button>
          <button type="button" role="radio" aria-checked={state.deliveryMode === 'steer'} title="Interrupt and redirect the current turn" disabled={state.deliverySaving} onClick={() => client.attempt(() => client.setDeliveryMode('steer'))}>Steer</button>
        </div>
        <button type="button" class="stop" aria-label="Stop the current turn" title="Stop (Esc)" onClick={() => client.attempt(client.cancelRun)}><Icon name="stop" size={14} /></button></Show>
        <button type="submit" class="send" disabled={!canSend()} aria-label={busy() ? state.deliveryMode === 'steer' ? 'Steer the current turn' : 'Queue message' : 'Send message'}><Icon name={busy() ? state.deliveryMode : 'send'} size={16} /></button>
        <details class="composer-tools">
          <summary aria-label="Workspace actions" title="Workspace actions"><Icon name="settings" size={14} /></summary>
          <div class="composer-tools-menu">
            <Show when={state.autoApprove}><span class="warn"><Icon name="shield" size={12} />Auto-approve is on</span></Show>
            {props.actions}
          </div>
        </details>
      </div>
    </form>
    <Show when={customCommand()}>{command => <Modal title={command().name} close={() => setCustomCommand(undefined)} size="small"><form class="modal-body stack" onSubmit={event => {
      event.preventDefault();
      const values = commandArgs();
      setValue(command().content.replace(/\$([A-Z][A-Z0-9_]*)/g, (placeholder, name: string) => values[name] ?? placeholder));
      setCustomCommand(undefined);
      input.focus();
    }}><For each={command().arguments}>{argument => <label class="field"><span>{argument.title || argument.id}</span><input aria-label={`Command argument ${argument.id}`} required={argument.required} value={commandArgs()[argument.id] ?? ''} onInput={event => setCommandArgs(values => ({ ...values, [argument.id]: event.currentTarget.value }))} /></label>}</For><button class="primary">Insert command</button></form></Modal>}</Show>
    <Show when={contextOpen()}>{value => <ContextDialog client={client} prompt={value() === true ? undefined : value() as MCPPrompt} close={() => setContextOpen(undefined)} insert={(content, added) => { setValue(`${text()}${text() && content ? '\n\n' : ''}${content}`); setFiles(items => [...items, ...added]); input.focus(); }} />}</Show>
  </div>;
}
