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
    // 実行環境の TZ に依存する落ち方をなくすため、テストプロセス全体の
    // ローカルタイムゾーンを固定する（issue #274）。`programs.test.tsx` の
    // 「今」を時刻境界に切り捨てるフィクスチャ（`windowOrigin` / 本番の
    // `dayOrigin(0)`）は、実行時の壁時計が現地 23 時台だと暦日をまたいで
    // しまい、TZ によって間欠的に落ちていた。固定先を JST にしたのは
    // 恣意的な選択ではなく、その回帰テスト（深夜 23:13 の実挙動）が
    // JST を前提に書かれているため。
    env: { TZ: 'Asia/Tokyo' },
  },
})
