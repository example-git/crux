import { defineConfig } from 'vite';
import solid from 'vite-plugin-solid';

const proxy = {
  target: 'http://127.0.0.1:8080',
  changeOrigin: true,
  configure(server: import('vite').HttpProxy.ProxyServer) {
    server.on('proxyReq', request => request.setHeader('Origin', 'http://127.0.0.1:8080'));
  },
};

export default defineConfig({
  plugins: [solid()],
  build: {
    target: 'es2022',
    outDir: '../internal/webclient/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/v1': { ...proxy, ws: true, rewriteWsOrigin: true },
      '/ui': proxy,
    },
  },
});
