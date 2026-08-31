import { StrictMode, useEffect } from 'react'
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
      // staleTime は「stale と判定する期限」であって再取得を起こすタイマーでは
      // ない。mount / focus のたびに取り直す幅を抑えるだけの値で、SSE の
      // 取りこぼしを回復するのは lib/events.ts の定期 invalidate の方。
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
  useEffect(() => {
    const colorScheme = window.matchMedia('(prefers-color-scheme: dark)')
    const syncColorScheme = () => document.documentElement.classList.toggle('dark', colorScheme.matches)
    syncColorScheme()
    colorScheme.addEventListener('change', syncColorScheme)
    return () => colorScheme.removeEventListener('change', syncColorScheme)
  }, [])

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
