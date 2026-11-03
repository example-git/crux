import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import path from 'node:path';

export function projectStore(directory) {
  const filename = path.join(directory, 'projects.json');
  let pending = Promise.resolve();
  function validate(value) {
    if (!value || value.version !== 1 || !Array.isArray(value.projects) || typeof value.pinned !== 'boolean' || typeof value.open !== 'boolean') throw new Error('Invalid desktop project list.');
    const seen = new Set();
    const projects = value.projects.map(project => {
      if (!project || typeof project.path !== 'string' || !(project.connection ? path.posix.isAbsolute(project.path) || path.win32.isAbsolute(project.path) : path.isAbsolute(project.path)) || typeof project.expanded !== 'boolean' || project.dataDir !== undefined && typeof project.dataDir !== 'string' || project.connection !== undefined && (typeof project.connection !== 'string' || !project.connection || project.connection.trim() !== project.connection)) throw new Error('Invalid project shortcut.');
      const key = JSON.stringify([project.connection ?? '', project.path, project.dataDir ?? '']);
      if (seen.has(key)) throw new Error('Duplicate project shortcut.');
      seen.add(key);
      return { path: project.path, ...(project.connection ? { connection: project.connection } : {}), ...(project.dataDir ? { dataDir: project.dataDir } : {}), expanded: project.expanded };
    });
    return { version: 1, pinned: value.pinned, open: value.open, projects };
  }
  return {
    async load() {
      await pending;
      try { return validate(JSON.parse(await readFile(filename, 'utf8'))); }
      catch (error) {
        if (error.code === 'ENOENT') return { version: 1, pinned: true, open: true, projects: [] };
        throw error;
      }
    },
    save(value) {
      const snapshot = validate(value);
      const saving = pending.then(async () => {
        await mkdir(directory, { recursive: true, mode: 0o700 });
        await writeFile(`${filename}.tmp`, `${JSON.stringify(snapshot, null, 2)}\n`, { mode: 0o600 });
        await rename(`${filename}.tmp`, filename);
        return snapshot;
      });
      pending = saving.catch(() => {});
      return saving;
    },
  };
}
