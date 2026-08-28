import { defineConfig } from 'orval'

export default defineConfig({
  // URL クエリの検証は openapi.yaml から生成した zod スキーマに任せる
  // （`src/api/zod.ts`）。手書きのパーサと openapi.yaml の制約
  // （`minimum` / `pattern` / `enum`）を二重管理しないため。
  rokubanZod: {
    input: '../openapi.yaml',
    output: {
      target: './src/api/zod.ts',
      client: 'zod',
      mode: 'single',
    },
  },
  rokuban: {
    input: '../openapi.yaml',
    output: {
      target: './src/api/generated.ts',
      client: 'react-query',
      mode: 'single',
      override: {
        mutator: {
          path: './src/api/client.ts',
          name: 'customInstance',
        },
      },
    },
  },
})
