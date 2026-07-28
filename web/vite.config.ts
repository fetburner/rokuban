import path from 'path'
// vite ではなく vitest/config の defineConfig を使う。vite 単体の型には
// `test` フィールドが無いため、vitest の設定をここに同居させるにはこちらが必要。
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    proxy: {
      '/api': 'http://localhost:40773',
      '/healthz': 'http://localhost:40773',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
