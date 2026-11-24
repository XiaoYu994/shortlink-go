import path from 'node:path';
import { fileURLToPath } from 'node:url';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

const rootDir = path.dirname(fileURLToPath(import.meta.url));
const consoleOrigin = process.env.VITE_CONSOLE_ORIGIN || 'http://127.0.0.1:8081';
const redirectorOrigin = process.env.VITE_REDIRECTOR_ORIGIN || 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(rootDir, './src'),
    },
  },
  build: {
    rollupOptions: {
      output: {
        // 将稳定框架依赖拆为独立 chunk，配合后端 /assets/ 的 immutable 强缓存，业务迭代不影响框架缓存命中。
        manualChunks(id) {
          if (!id.includes('node_modules')) return undefined;
          if (id.includes('react-router')) return 'vendor-router';
          if (id.includes('@tanstack/react-query')) return 'vendor-query';
          if (
            id.includes('@radix-ui') ||
            id.includes('lucide-react') ||
            id.includes('sonner') ||
            id.includes('qrcode')
          ) {
            return 'vendor-ui';
          }
          if (id.includes('/react/') || id.includes('/react-dom/') || id.includes('/scheduler/')) {
            return 'vendor-react';
          }
          return undefined;
        },
      },
    },
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': {
        target: consoleOrigin,
        // 保留浏览器 Host，避免后端 CrossOriginProtection 判定跨站或 Cookie 域名不一致。
        changeOrigin: false,
      },
      '/r': {
        target: redirectorOrigin,
        changeOrigin: false,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
});
