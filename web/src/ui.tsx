import { For, onCleanup, onMount, Show, type JSX } from 'solid-js';
import type { Toast } from './state.ts';

const paths: Record<string, string> = {
  plus: 'M12 5v14M5 12h14', close: 'M6 6l12 12M18 6 6 18', search: 'M21 21l-4.3-4.3M11 18a7 7 0 1 1 0-14 7 7 0 0 1 0 14Z',
  folder: 'M3 6.5A1.5 1.5 0 0 1 4.5 5H9l2 2h8.5A1.5 1.5 0 0 1 21 8.5v9a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5Z',
  chat: 'M4 5h16v11H9l-5 4Z', settings: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1Z',
  sidebar: 'M4 4h16v16H4ZM9 4v16', panel: 'M4 4h16v16H4ZM15 4v16', terminal: 'M4 17l5-5-5-5M12 19h8',
  branch: 'M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM18 9a9 9 0 0 1-9 9',
  send: 'M5 12h14M13 6l6 6-6 6', down: 'M6 9l6 6 6-6', right: 'M9 6l6 6-6 6', menu: 'M4 6h16M4 12h16M4 18h16',
  file: 'M14 3H6v18h12V7ZM14 3v4h4', check: 'M5 12l5 5L20 7', clock: 'M12 7v5l3 2M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
  queue: 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01', steer: 'M5 19 19 5M19 5h-7M19 5v7',
  refresh: 'M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7', copy: 'M9 9h11v11H9ZM5 15V4h11', bolt: 'M13 2 4 14h7l-1 8 9-12h-7Z',
  archive: 'M3 4h18v4H3ZM5 8v12h14V8M10 12h4', more: 'M5 12h.01M12 12h.01M19 12h.01', stop: 'M7 7h10v10H7Z',
  target: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8ZM12 12h.01', plan: 'M9 6h11M9 12h11M9 18h11M4 6l1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2',
  key: 'M15 7a4 4 0 1 1-3.9 5H3v3h3v3h3v-3h2.1A4 4 0 0 1 15 7Z', plug: 'M9 2v6M15 2v6M6 8h12v4a6 6 0 0 1-12 0ZM12 18v4',
  database: 'M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3ZM4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3',
  shield: 'M12 3 4 6v6c0 5 3.5 8 8 9 4.5-1 8-4 8-9V6Z', eye: 'M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12ZM12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z',
  spark: 'M12 3l1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8Z', user: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8ZM4 21a8 8 0 0 1 16 0',
  undo: 'M9 14 4 9l5-5M4 9h11a5 5 0 0 1 0 10h-3', trash: 'M4 7h16M10 11v6M14 11v6M5 7l1 13h12l1-13M9 7V4h6v3', edit: 'M4 20h4L19 9l-4-4L4 16ZM14 6l4 4',
  external: 'M14 4h6v6M20 4l-9 9M18 14v6H4V6h6', attach: 'M21 11l-8.5 8.5a5 5 0 0 1-7-7L14 4a3.5 3.5 0 0 1 5 5l-8.5 8.5a2 2 0 0 1-3-3L15 7',
  command: 'M9 6a3 3 0 1 0-3 3h12a3 3 0 1 0-3-3v12a3 3 0 1 0 3-3H6a3 3 0 1 0 3 3Z', layers: 'M12 3 2 8l10 5 10-5ZM2 13l10 5 10-5',
  info: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 11v5M12 8h.01', warning: 'M12 3 2 20h20ZM12 10v4M12 17h.01',
};

export const pageHidden = () => typeof document !== 'undefined' && document.visibilityState === 'hidden';

export function Icon(props: { name: string; size?: number; class?: string }) {
  return <svg class={props.class} width={props.size ?? 16} height={props.size ?? 16} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d={paths[props.name] ?? paths.file} /></svg>;
}

export function Modal(props: { title: string; close: () => void; children: JSX.Element; size?: 'small' | 'medium' | 'large'; class?: string; subtitle?: string }) {
  let panel!: HTMLElement;
  let previous: HTMLElement | null;
  onMount(() => {
    previous = document.activeElement as HTMLElement;
    (panel.querySelector('[autofocus], input, textarea, select, button:not(.close)') as HTMLElement)?.focus();
  });
  onCleanup(() => previous?.focus?.());
  return <div class="overlay" onMouseDown={event => { if (event.target === event.currentTarget) props.close(); }} onKeyDown={event => {
    if (event.key === 'Escape') { event.stopPropagation(); props.close(); }
    if (event.key !== 'Tab') return;
    const items = Array.from(panel.querySelectorAll<HTMLElement>('a[href], button:not(:disabled), input:not(:disabled), textarea:not(:disabled), select:not(:disabled), [tabindex="0"]')).filter(item => !item.closest('[hidden]'));
    if (!items.length) return;
    const edge = event.shiftKey ? items[0] : items.at(-1);
    if (document.activeElement === edge) { event.preventDefault(); (event.shiftKey ? items.at(-1) : items[0])?.focus(); }
  }}>
    <section ref={panel} class={`modal ${props.size ?? 'medium'} ${props.class ?? ''}`} role="dialog" aria-modal="true" aria-label={props.title}>
      <header class="modal-head"><div><h2>{props.title}</h2><Show when={props.subtitle}><p>{props.subtitle}</p></Show></div><button class="ghost icon close" aria-label={`Close ${props.title.toLowerCase()}`} onClick={props.close}><Icon name="close" /></button></header>
      {props.children}
    </section>
  </div>;
}

export function Switch(props: { checked: boolean; onChange: (value: boolean) => void; label: string; disabled?: boolean }) {
  return <button type="button" role="switch" class="switch" aria-checked={props.checked} aria-label={props.label} disabled={props.disabled} onClick={() => props.onChange(!props.checked)}><span /></button>;
}

export function Segmented<T extends string>(props: { value: T; options: { value: T; label: string; icon?: string; title?: string }[]; onChange: (value: T) => void; label: string; disabled?: boolean }) {
  return <div class="segmented" role="radiogroup" aria-label={props.label}><For each={props.options}>{option => <button type="button" role="radio" aria-checked={props.value === option.value} title={option.title} disabled={props.disabled} onClick={() => props.onChange(option.value)}><Show when={option.icon}><Icon name={option.icon!} size={14} /></Show>{option.label}</button>}</For></div>;
}

export function Row(props: { title: string; detail?: JSX.Element; children?: JSX.Element; class?: string }) {
  return <div class={`row ${props.class ?? ''}`}><div class="row-text"><strong>{props.title}</strong><Show when={props.detail}><small>{props.detail}</small></Show></div><Show when={props.children}><div class="row-control">{props.children}</div></Show></div>;
}

export function Empty(props: { icon?: string; title: string; children?: JSX.Element }) {
  return <div class="empty"><Show when={props.icon}><Icon name={props.icon!} size={22} /></Show><strong>{props.title}</strong><Show when={props.children}><p>{props.children}</p></Show></div>;
}

export function Spinner(props: { label?: string }) {
  return <span class="spinner" role="status" aria-label={props.label ?? 'Loading'} />;
}

export function Badge(props: { tone?: 'neutral' | 'good' | 'warn' | 'bad' | 'accent'; children: JSX.Element; title?: string }) {
  return <span class={`badge ${props.tone ?? 'neutral'}`} title={props.title}>{props.children}</span>;
}

export function Menu(props: { open: boolean; close: () => void; children: JSX.Element; align?: 'left' | 'right' }) {
  let element!: HTMLDivElement;
  onMount(() => {
    const outside = (event: MouseEvent) => { if (props.open && !element.parentElement?.contains(event.target as Node)) props.close(); };
    document.addEventListener('mousedown', outside);
    onCleanup(() => document.removeEventListener('mousedown', outside));
  });
  return <div ref={element} class={`menu ${props.align ?? 'right'}`} hidden={!props.open} role="menu" onClick={props.close}>{props.children}</div>;
}

export function Toasts(props: { toasts: Toast[]; dismiss: (id: number) => void }) {
  return <div class="toasts" aria-live="polite"><For each={props.toasts}>{toast => <div class={`toast ${toast.kind}`} role={toast.kind === 'error' ? 'alert' : 'status'}><Icon name={toast.kind === 'error' ? 'warning' : toast.kind === 'success' ? 'check' : 'info'} size={15} /><span>{toast.text}</span><button class="ghost icon" aria-label="Dismiss" onClick={() => props.dismiss(toast.id)}><Icon name="close" size={13} /></button></div>}</For></div>;
}

export function Kbd(props: { children: string }) { return <kbd>{props.children}</kbd>; }

export const errorText = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);

export function relativeTime(seconds: number) {
  const delta = Date.now() / 1000 - seconds;
  if (delta < 60) return 'now';
  if (delta < 3600) return `${Math.floor(delta / 60)}m`;
  if (delta < 86400) return `${Math.floor(delta / 3600)}h`;
  if (delta < 604800) return `${Math.floor(delta / 86400)}d`;
  return new Date(seconds * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

export function compact(value: number) {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1000) return `${(value / 1000).toFixed(value >= 10000 ? 0 : 1)}k`;
  return String(value);
}

export async function readFiles(files: FileList | File[]) {
  return Promise.all(Array.from(files).map(file => new Promise<{ file_name: string; mime_type: string; content: string }>((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error(`Could not read ${file.name}`));
    reader.onload = () => resolve({ file_name: file.name, mime_type: file.type || 'application/octet-stream', content: String(reader.result).split(',')[1] ?? '' });
    reader.readAsDataURL(file);
  })));
}

export function busyRunner(setBusy: (value: boolean) => void, setError: (value: string) => void) {
  return async (action: () => Promise<unknown>) => {
    setBusy(true); setError('');
    try { await action(); return true; } catch (reason) { setError(errorText(reason)); return false; } finally { setBusy(false); }
  };
}
