import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'

import { ToastProvider } from './components/toaster'
import { useServerEvents } from './lib/events'
import { routeTree } from './routes'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // SSE は取りこぼしうるヒントなので、staleTime を置いて定期的な再取得でも
      // 収束させる。プッシュが来なくても最終的に正しい状態に追いつく。
      staleTime: 30_000,
      refetchOnWindowFocus: true,
    },
  },
})

const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

function App() {
  useServerEvents()
  return <RouterProvider router={router} />
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <App />
      </ToastProvider>
    </QueryClientProvider>
  </StrictMode>,
)
