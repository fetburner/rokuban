import { defineConfig } from 'orval'

export default defineConfig({
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
