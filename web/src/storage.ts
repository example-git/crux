const prefix = 'crux.web.';
const version = 1;

type Stored<T> = { version: number; saved: number; value: T };

function storage(): Storage | undefined {
  try { return typeof localStorage === 'undefined' ? undefined : localStorage; }
  catch { return undefined; }
}

export function readStored<T>(name: string, maxAge = Infinity): T | undefined {
  const store = storage();
  if (!store) return undefined;
  try {
    const raw = store.getItem(prefix + name);
    if (!raw) return undefined;
    const parsed = JSON.parse(raw) as Stored<T>;
    if (parsed?.version !== version || Date.now() - parsed.saved > maxAge) { store.removeItem(prefix + name); return undefined; }
    return parsed.value;
  } catch { return undefined; }
}

export function writeStored<T>(name: string, value: T | undefined): boolean {
  const store = storage();
  if (!store) return false;
  try {
    if (value === undefined) store.removeItem(prefix + name);
    else store.setItem(prefix + name, JSON.stringify({ version, saved: Date.now(), value } satisfies Stored<T>));
    return true;
  } catch { return false; }
}

const timers = new Map<string, ReturnType<typeof setTimeout>>();
const pending = new Map<string, () => void>();

export function writeStoredSoon<T>(name: string, value: () => T | undefined, delay = 400) {
  clearTimeout(timers.get(name));
  const write = () => { clearTimeout(timers.get(name)); timers.delete(name); pending.delete(name); writeStored(name, value()); };
  pending.set(name, write);
  timers.set(name, setTimeout(write, delay));
}

export function flushStored() {
  for (const write of [...pending.values()]) write();
}

if (typeof addEventListener === 'function') addEventListener('pagehide', flushStored);
