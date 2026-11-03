import { app, BrowserWindow, dialog, ipcMain, Menu, shell } from 'electron';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { startHost } from './host.mjs';
import { installPermissions } from './permissions.mjs';
import { projectStore } from './projects.mjs';

const directory = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(directory, '../..');
const binary = process.env.CRUX_BINARY ?? path.join(app.isPackaged ? process.resourcesPath : root, process.platform === 'win32' ? 'crux.exe' : 'crux');
const icon = path.join(app.isPackaged ? process.resourcesPath : path.resolve(directory, '..'), 'icon.png');
const args = process.argv.slice(app.isPackaged ? 1 : 2);
let host;
let window;
let quitting = false;

app.setName('Crux');
Menu.setApplicationMenu(Menu.buildFromTemplate([
  ...(process.platform === 'darwin' ? [{ role: 'appMenu' }] : []),
  { role: 'editMenu' }, { role: 'viewMenu' }, { role: 'windowMenu' },
]));

function openExternal(url) {
  try {
    const parsed = new URL(url);
    if (['https:', 'http:'].includes(parsed.protocol)) void shell.openExternal(parsed.href);
  } catch {}
}

app.on('before-quit', event => {
  if (!host || quitting) return;
  event.preventDefault();
  quitting = true;
  void host.stop().finally(() => app.quit());
});
app.on('window-all-closed', () => app.quit());

void app.whenReady().then(async () => {
  host = startHost(binary, args, { cwd: process.env.INIT_CWD ?? (app.isPackaged ? app.getPath('home') : root) });
  const origin = await host.ready;
  host.child.on('exit', (code, signal) => {
    if (!quitting) {
      dialog.showErrorBox('Crux host stopped', `The local execution host exited (${signal ?? code}). Restart Crux to reconnect.`);
      app.quit();
    }
  });
  const translucency = process.platform === 'darwin'
    ? { vibrancy: 'under-window', visualEffectState: 'active' }
    : process.platform === 'win32' ? { backgroundMaterial: 'acrylic' } : { transparent: true };
  window = new BrowserWindow({
    title: 'Crux', icon, width: 1320, height: 900, minWidth: 640, minHeight: 540,
    backgroundColor: '#00000000', show: false, frame: false, ...translucency,
    webPreferences: { nodeIntegration: false, contextIsolation: true, sandbox: true, spellcheck: false, preload: path.join(directory, 'preload.cjs') },
  });
  const projects = projectStore(app.getPath('userData'));
  const requireWindow = event => {
    if (event.sender !== window.webContents || event.senderFrame !== window.webContents.mainFrame || new URL(event.senderFrame.url).origin !== origin) throw new Error('Project access requires the Crux window.');
  };
  ipcMain.handle('crux:window-control', (event, action) => {
    requireWindow(event);
    switch (action) {
      case 'minimize': window.minimize(); break;
      case 'maximize': if (window.isMaximized()) window.unmaximize(); else window.maximize(); break;
      case 'close': window.close(); break;
      default: throw new Error('Unknown window control.');
    }
  });
  ipcMain.handle('crux:projects-load', event => { requireWindow(event); return projects.load(); });
  ipcMain.handle('crux:projects-save', (event, value) => { requireWindow(event); return projects.save(value); });
  ipcMain.handle('crux:select-directory', async event => {
    if (event.sender !== window.webContents || event.senderFrame !== window.webContents.mainFrame || new URL(event.senderFrame.url).origin !== origin) throw new Error('Directory selection requires the Crux window.');
    const result = await dialog.showOpenDialog(window, { title: 'Open Crux workspace', properties: ['openDirectory'] });
    return result.canceled ? null : result.filePaths[0];
  });
  window.webContents.setWindowOpenHandler(({ url, frameName }) => {
    if (url === 'about:blank' && frameName === 'crux-task-panel') return { action: 'allow', overrideBrowserWindowOptions: { parent: window, title: 'Crux tasks and terminal', width: 900, height: 600, minWidth: 480, minHeight: 260, frame: true, backgroundColor: '#201f26', webPreferences: { nodeIntegration: false, contextIsolation: true, sandbox: true } } };
    openExternal(url);
    return { action: 'deny' };
  });
  window.webContents.on('did-create-window', child => {
    child.webContents.on('will-navigate', event => event.preventDefault());
    child.webContents.setWindowOpenHandler(({ url }) => { openExternal(url); return { action: 'deny' }; });
  });
  window.webContents.on('will-navigate', (event, url) => {
    if (new URL(url).origin !== origin) { event.preventDefault(); openExternal(url); }
  });
  installPermissions(window, origin, dialog);
  window.once('ready-to-show', () => window.show());
  await window.loadURL(origin);
}).catch(error => {
  dialog.showErrorBox('Could not start Crux', String(error));
  app.quit();
});
