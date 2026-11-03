import assert from 'node:assert/strict';
import test from 'node:test';
import { installPermissions } from './permissions.mjs';

function setup(response = 0) {
  const origin = 'http://127.0.0.1:41001';
  let request, check;
  let url = origin;
  let destroyed = false;
  const dialogs = [];
  const contents = { isDestroyed: () => destroyed, getURL: () => url, session: {
    setPermissionCheckHandler(handler) { check = handler; },
    setPermissionRequestHandler(handler) { request = handler; },
  } };
  const window = { webContents: contents, isDestroyed: () => destroyed };
  const dialog = { async showMessageBox(owner, options) { dialogs.push({ owner, options }); return { response }; } };
  installPermissions(window, origin, dialog);
  const details = { requestingUrl: origin + '/', isMainFrame: true };
  return { origin, contents, window, dialog, dialogs, details, check: (...args) => check(...args), request: (...args) => request(...args), navigate: next => { url = next; }, destroy: () => { destroyed = true; } };
}

test('desktop notifications require explicit approval and retain the grant only for the Crux main frame', async () => {
  const f = setup();
  assert.equal(f.check(f.contents, 'notifications', f.origin, f.details), false);
  const granted = await new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details));
  assert.equal(granted, true);
  assert.equal(f.dialogs.length, 1);
  assert.equal(f.dialogs[0].owner, f.window);
  assert.equal(f.dialogs[0].options.defaultId, 1);
  assert.equal(f.check(f.contents, 'notifications', f.origin, f.details), true);
  assert.equal(f.check(f.contents, 'notifications', 'https://example.test', f.details), false);
  assert.equal(f.check(f.contents, 'notifications', f.origin, { ...f.details, isMainFrame: false }), false);
  assert.equal(f.check(null, 'notifications', f.origin, f.details), false);
  assert.equal(await new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details)), true);
  assert.equal(f.dialogs.length, 1);
});

test('desktop rejects denied notification requests and unrelated permissions', async () => {
  const f = setup(1);
  assert.equal(await new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details)), false);
  assert.equal(f.check(f.contents, 'notifications', f.origin, f.details), false);
  for (const permission of ['media', 'display-capture', 'clipboard-read', 'geolocation', 'unknown']) {
    assert.equal(await new Promise(resolve => f.request(f.contents, permission, resolve, f.details)), false);
    assert.equal(f.check(f.contents, permission, f.origin, f.details), false);
  }
  assert.equal(f.check(f.contents, 'clipboard-sanitized-write', f.origin, f.details), true);
  assert.equal(await new Promise(resolve => f.request(f.contents, 'clipboard-sanitized-write', resolve, f.details)), true);
  for (const details of [{ ...f.details, requestingUrl: 'https://example.test' }, { ...f.details, isMainFrame: false }, { ...f.details, requestingUrl: 'not a URL' }]) {
    assert.equal(await new Promise(resolve => f.request(f.contents, 'notifications', resolve, details)), false);
  }
  assert.equal(f.dialogs.length, 1);
});

test('desktop coalesces permission prompts and rejects approval after navigation or destruction', async () => {
  for (const action of ['navigate', 'destroy']) {
    const f = setup();
    let finish;
    f.dialog.showMessageBox = async () => { f.dialogs.push({}); return new Promise(resolve => { finish = resolve; }); };
    const first = new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details));
    const second = new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details));
    assert.equal(f.dialogs.length, 1);
    if (action === 'navigate') f.navigate('https://example.test');
    else f.destroy();
    finish({ response: 0 });
    assert.deepEqual(await Promise.all([first, second]), [false, false]);
    assert.equal(f.check(f.contents, 'notifications', f.origin, f.details), false);
  }
});

test('desktop notification dialog failures deny permission', async () => {
  const f = setup();
  f.dialog.showMessageBox = async () => { throw new Error('Dialog unavailable'); };
  assert.equal(await new Promise(resolve => f.request(f.contents, 'notifications', resolve, f.details)), false);
});
