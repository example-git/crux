const binary = process.platform === 'win32' ? 'crux.exe' : 'crux';

module.exports = {
  appId: 'dev.crux.desktop',
  productName: 'Crux',
  directories: { output: '../dist/desktop', buildResources: '.' },
  asar: true,
  files: ['electron/**/*.mjs', 'electron/preload.cjs', '!electron/**/*.test.mjs', 'package.json'],
  extraResources: [
    { from: '../crux', to: binary },
    { from: 'icon.png', to: 'icon.png' },
  ],
  icon: 'icon.png',
  npmRebuild: false,
  mac: { target: ['dmg', 'zip'], category: 'public.app-category.developer-tools' },
  win: { target: ['nsis'] },
  linux: { target: ['AppImage'], category: 'Development' },
};
