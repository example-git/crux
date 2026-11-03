import type { Attachment, Skill } from './types.ts';
import { readStored, writeStored } from './storage.ts';

export type Draft = { text: string; files: Attachment[]; skills: Skill[]; omittedFiles?: number };
type SavedDraft = Draft & { updated: number };

const draftsName = 'drafts';
const maxDrafts = 100;
const maxDraftAge = 30 * 24 * 60 * 60 * 1000;
const maxBytes = 2 << 20;

const empty = (draft: Draft) => !draft.text && !draft.files.length && !draft.skills.length;

export function createDraftStore(onSaveFailed: () => void) {
  const saved = new Map<string, SavedDraft>(Object.entries(readStored<Record<string, SavedDraft>>(draftsName, maxDraftAge) ?? {}));
  let timer: ReturnType<typeof setTimeout> | undefined;
  let failed = false;
  function encode() {
    const entries = [...saved.entries()].sort((a, b) => b[1].updated - a[1].updated).slice(0, maxDrafts);
    let value: Record<string, SavedDraft> = Object.fromEntries(entries);
    let size = JSON.stringify(value).length;
    for (const [key, draft] of [...entries].reverse()) {
      if (size <= maxBytes) break;
      if (!draft.files.length) continue;
      const stripped = { ...draft, files: [], omittedFiles: (draft.omittedFiles ?? 0) + draft.files.length };
      value = { ...value, [key]: stripped };
      size = JSON.stringify(value).length;
    }
    return size <= maxBytes ? value : Object.fromEntries(Object.entries(value).map(([key, draft]) => [key, { ...draft, files: [], omittedFiles: (draft.omittedFiles ?? 0) + draft.files.length }]));
  }
  function flush() {
    clearTimeout(timer);
    timer = undefined;
    if (writeStored(draftsName, saved.size ? encode() : undefined)) failed = false;
    else if (!failed) { failed = true; onSaveFailed(); }
  }
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(flush, 400);
  }
  if (typeof addEventListener === 'function') addEventListener('pagehide', () => { if (timer) flush(); });
  return {
    get(key: string): Draft | undefined {
      const draft = saved.get(key);
      return draft && { text: draft.text, files: draft.files, skills: draft.skills, omittedFiles: draft.omittedFiles };
    },
    set(key: string, draft: Draft) {
      const previous = saved.get(key);
      if (empty(draft)) { if (previous) { saved.delete(key); schedule(); } return; }
      if (previous && previous.text === draft.text && previous.files === draft.files && previous.skills === draft.skills) return;
      saved.set(key, { text: draft.text, files: draft.files, skills: draft.skills, updated: Date.now() });
      schedule();
    },
    delete(key: string) {
      if (saved.delete(key)) schedule();
    },
    flush,
  };
}
