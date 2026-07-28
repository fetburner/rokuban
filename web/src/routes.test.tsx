import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ToastProvider } from '@/components/toaster'
import { SearchPage } from '@/pages/search'
import { routeTree } from '@/routes'

/**
 * 画面を作ってもルートとナビゲーションに繋がなければどこからも開けない。
 * ページ側のテストはコンポーネントを直接描くので、この配線は別に見る必要がある。
 */
describe('routeTree', () => {
  const children = routeTree.children as unknown as {
    options: { path?: string; component?: unknown }
  }[]

  it('全ルートが登録されている', () => {
    expect(children.map((route) => route.options.path)).toEqual([
      '/',
      '/search',
      '/reservations',
      '/reservations/$reservationId',
      '/recordings',
    ])
  })

  it('検索は番組表とは別のルートに置く', () => {
    // 番組表（/）は EPG を時間軸で眺める画面、検索は ruler と同じ条件
    // コンパイラを叩く「ルールの条件を試す」画面なので関心事が違う
    const search = children.find((route) => route.options.path === '/search')
    expect(search?.options.component).toBe(SearchPage)
  })

  it('/search を開くと検索画面が出て、主ナビゲーションから辿れる', async () => {
    // jsdom は window.scrollTo を実装していない。ルーターのスクロール復元が
    // 呼ぶため、置いておかないと関係のない例外がログを埋める
    window.scrollTo = vi.fn()

    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      const body = url.pathname === '/api/breakers' ? [] : []
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }) as unknown as typeof fetch

    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/search'] }),
    })
    render(
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <ToastProvider>
          {/* 型は main.tsx の router で登録されるため、ここでは構造だけ見る */}
          <RouterProvider router={router as never} />
        </ToastProvider>
      </QueryClientProvider>,
    )

    // 検索フォームが描かれる（ルートがページに繋がっている）
    expect(await screen.findByRole('form', { name: '検索条件' })).toBeInTheDocument()
    // 主ナビゲーション（モバイルのボトムタブとデスクトップのサイドバーで
    // 同じ定義を使うので 2 つ出る）に検索への導線がある
    const links = screen.getAllByRole('link', { name: '検索' })
    expect(links.length).toBeGreaterThan(0)
    for (const link of links) expect(link).toHaveAttribute('href', '/search')
    // 現在地として示される
    expect(links[0]).toHaveAttribute('aria-current', 'page')
  })
})
