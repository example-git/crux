import type { Event, Message } from './types.ts';

export function coalesceEvents(events: Event[]): Event[] {
  const result: Event[] = [];
  const positions = new Map<string, number>();
  for (const event of events) {
    if (event.type !== 'message') {
      result.push(event);
      continue;
    }
    const message = event.payload.payload as Message;
    const key = `${message.session_id}/${message.id}`;
    const position = positions.get(key);
    if (position !== undefined) result[position] = event;
    else { positions.set(key, result.length); result.push(event); }
  }
  return result;
}

export function resolveSession(sessions: { id: string }[], id: string): string {
  const exact = sessions.find(session => session.id === id);
  if (exact) return exact.id;
  const matches = sessions.filter(session => session.id.startsWith(id));
  if (matches.length !== 1) throw new Error(matches.length ? `Session ID ${id} is ambiguous.` : `Session ${id} was not found in this workspace.`);
  return matches[0].id;
}
