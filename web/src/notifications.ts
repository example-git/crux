import type { Message } from './types.ts';

export function taskNotification(message: Message) {
  return parseTaskNotification(message.parts.filter(part => part.type === 'text').map(part => part.data.text ?? '').join('\n'));
}

export function parseTaskNotification(content: string) {
  const text = content.trim();
  if (!text.startsWith('<task-notification>') || !text.endsWith('</task-notification>')) return;
  const document = new DOMParser().parseFromString(text, 'application/xml');
  const root = document.documentElement;
  if (document.querySelector('parsererror') || root?.tagName !== 'task-notification') return;
  const field = (name: string) => Array.from(root.children).find(child => child.tagName === name)?.textContent?.trim() ?? '';
  const id = field('task-id');
  if (!id) return;
  const type = field('task-type');
  return { id, type, title: ({ agent: 'Background agent', shell: 'Background shell', image: 'Background image' } as Record<string, string>)[type] ?? 'Background task', status: field('status'), summary: field('summary'), result: field('result'), error: field('error-message'), lostReason: field('lost-reason'), exitCode: field('exit-code'), outputRef: field('output-file') };
}

export function messageText(message: Message) {
  const notification = taskNotification(message);
  return notification ? [notification.title, notification.status, notification.summary, notification.error, notification.lostReason, notification.result].filter(Boolean).join('\n') : message.parts.filter(part => part.type === 'text').map(part => part.data.text ?? '').join('\n');
}
