import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import { render } from '@testing-library/react'

import { ToastProvider } from '@/components/toaster'

/**
 * renderInRouter は任意の要素をルーター（+ QueryClient + ToastProvider）の
 * 中で描く。
 *
 * `Link` / `useNavigate` / `useSearch` を使うコンポーネントは、ルーターの外では
 * 描けない（`pages/reservations.test.tsx` が組んでいたのと同じ理由）。
 * 呼び出し側ごとに毎回ルーターを組み立てさせると、同じ配線をテストファイル
 * 間で重複させることになるのでここに集約する。
 *
 * 実際のアプリの `routeTree`（`@/routes`）は使わない。`AppShell` を経由すると
 * `/api/breakers` 等のこの画面に無関係な問い合わせまで発生し、それをテストごとに
 * スタブしなければならなくなる。ここで作るのは `ui` だけを描く最小限の
 * ルートツリー（root + `path` の 1 ルート）。`Link to="/other/route"` は
 * 宛先ルートが未登録でも、パステンプレートの補間だけで href を組めるので
 * 問題なく機能する（宛先ルートへの実際のナビゲーション・データ読み込みを
 * 検証したいテストは、このヘルパではなく本物の `routeTree` を使うこと）。
 */
export function renderInRouter(
  ui: React.ReactElement,
  options?: { path?: string; initialEntries?: string[]; queryClient?: QueryClient },
) {
  const path = options?.path ?? '/'
  const queryClient =
    options?.queryClient ??
    new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })

  const rootRoute = createRootRoute()
  const route = createRoute({
    getParentRoute: () => rootRoute,
    path,
    component: () => ui,
  })
  const routeTree = rootRoute.addChildren([route])

  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: options?.initialEntries ?? [path] }),
  })

  const view = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        {/* 型はアプリ本体の routeTree で登録されるため、ここでは構造だけ見る */}
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )

  return { ...view, router, queryClient }
}
