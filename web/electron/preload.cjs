const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('cruxDesktop', Object.freeze({
  windowControl: action => ipcRenderer.invoke('crux:window-control', action),
  selectDirectory: () => ipcRenderer.invoke('crux:select-directory'),
  loadProjects: () => ipcRenderer.invoke('crux:projects-load'),
  saveProjects: value => ipcRenderer.invoke('crux:projects-save', value),
}));
