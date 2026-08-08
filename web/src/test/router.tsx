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
import { SiteContext } from '@/lib/site'

/**
 * testSite は renderInRouter が既定で `<SiteContext>` に注入する値。実アプリの
 * `<SiteGate>`（`GET /api/sites` を解決してから注入する）は経由しない ---
 * ページ・コンポーネントのテストがサイト一覧のフェッチ完了を待つ必要を
 * 無くすため（CLAUDE.md テスト規律「非同期の空虚な成功に注意する」。
 * `lib/site.ts` の `SiteContext` のコメントも参照）。
 */
export const testSite = 'default'

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
  options?: { path?: string; initialEntries?: string[]; queryClient?: QueryClient; site?: string },
) {
  const path = options?.path ?? '/'
  const queryClient =
    options?.queryClient ??
    new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })
  const site = options?.site ?? testSite

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
        <SiteContext value={site}>
          {/* 型はアプリ本体の routeTree で登録されるため、ここでは構造だけ見る */}
          <RouterProvider router={router as never} />
        </SiteContext>
      </ToastProvider>
    </QueryClientProvider>,
  )

  return { ...view, router, queryClient }
}
