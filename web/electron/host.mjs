import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';

export function startHost(binary, args, options = {}) {
  const child = spawn(binary, ['web', '--json', ...args], {
    cwd: options.cwd ?? process.cwd(), env: options.env ?? process.env,
    stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true,
  });
  const lines = createInterface({ input: child.stdout });
  child.stderr.pipe(options.stderr ?? process.stderr);
  let stopped = false;
  const ready = new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', (code, signal) => reject(new Error(`Crux host exited before readiness (${signal ?? code}).`)));
    lines.on('line', line => {
      let value;
      try { value = JSON.parse(line); } catch { return; }
      if (typeof value.url !== 'string') return;
      let url;
      try { url = new URL(value.url); }
      catch { reject(new Error('Crux returned an invalid local host URL.')); child.kill('SIGTERM'); return; }
      if (url.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(url.hostname) || !url.port || url.username || url.password || url.search || url.hash || url.pathname !== '/') {
        reject(new Error('Crux returned an invalid local host URL.'));
        child.kill('SIGTERM');
        return;
      }
      resolve(url.origin);
    });
  });
  const exited = new Promise(resolve => { child.once('exit', resolve); child.once('error', resolve); });
  return {
    child, ready, exited,
    async stop() {
      if (stopped || child.exitCode !== null || child.signalCode !== null) return;
      stopped = true;
      child.kill('SIGTERM');
      const timer = setTimeout(() => child.kill('SIGKILL'), 8000);
      timer.unref();
      await exited;
      clearTimeout(timer);
      lines.close();
    },
  };
}
