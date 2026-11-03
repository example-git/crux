export function installPermissions(window, origin, dialog) {
  let notificationsGranted = false;
  let pending;
  const trusted = (contents, url, mainFrame) => {
    if (window.isDestroyed() || contents !== window.webContents || contents.isDestroyed() || !mainFrame) return false;
    try { return new URL(url).origin === origin && new URL(contents.getURL()).origin === origin; }
    catch { return false; }
  };
  window.webContents.session.setPermissionCheckHandler((contents, permission, requestingOrigin, details) => {
    if (!trusted(contents, details.requestingUrl, details.isMainFrame) || requestingOrigin !== origin) return false;
    return permission === 'notifications' ? notificationsGranted : permission === 'clipboard-sanitized-write';
  });
  window.webContents.session.setPermissionRequestHandler((contents, permission, callback, details) => {
    if (!trusted(contents, details.requestingUrl, details.isMainFrame)) { callback(false); return; }
    if (permission === 'clipboard-sanitized-write') { callback(true); return; }
    if (permission !== 'notifications') { callback(false); return; }
    if (notificationsGranted) { callback(true); return; }
    pending ??= dialog.showMessageBox(window, {
      type: 'question', title: 'Crux notifications', message: 'Allow Crux to show desktop notifications?',
      detail: 'Notifications can include session titles, task summaries, and errors. Enable or disable alerts for this tab in Settings → Behavior.',
      buttons: ['Allow notifications', 'Not now'], defaultId: 1, cancelId: 1, noLink: true,
    }).then(result => result.response === 0).catch(() => false).finally(() => { pending = undefined; });
    void pending.then(allowed => {
      const granted = allowed && trusted(contents, details.requestingUrl, details.isMainFrame);
      if (granted) notificationsGranted = true;
      callback(granted);
    });
  });
}
