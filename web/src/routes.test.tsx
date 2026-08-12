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
      '/rules',
      '/reservations',
      '/reservations/$site/$programId',
      '/recordings',
      '/recordings/$id',
      '/live',
    ])
  })

  it('検索は番組表とは別のルートに置く', () => {
    // 番組表（/）は EPG を時間軸で眺める画面、検索は ruler と同じ条件
    // コンパイラを叩く「ルールの条件を試す」画面なので関心事が違う
    const search = children.find((route) => route.options.path === '/search')
    expect(search?.options.component).toBe(SearchPage)
  })

  it('/search の ruleId は不正な値だと undefined に落ちる', () => {
    // ルール画面 → 検索画面のプレビュー導線（`<Link to="/search" search={{ ruleId }}>`）
    // の契約。消費側（プレビュー実装）はまだ無いが、双方が互いを見ずに実装
    // できるようこの検証だけ先に固定する
    const search = routeTree.children as unknown as {
      options: {
        path?: string
        validateSearch?: (search: Record<string, unknown>) => { ruleId?: number }
      }
    }[]
    const validateSearch = search.find((route) => route.options.path === '/search')?.options
      .validateSearch
    if (!validateSearch) throw new Error('/search route に validateSearch が無い')

    expect(validateSearch({ ruleId: 42 })).toEqual({ ruleId: 42 })
    // URL の生値は文字列で来る（JSON.parse できる数字文字列は router 側で
    // 既に数値化されるが、それに頼らず文字列でも受けられることを確かめる）
    expect(validateSearch({ ruleId: '42' })).toEqual({ ruleId: 42 })
    expect(validateSearch({ ruleId: 'abc' })).toEqual({})
    expect(validateSearch({ ruleId: Number.NaN })).toEqual({})
    expect(validateSearch({ ruleId: Number.POSITIVE_INFINITY })).toEqual({})
    expect(validateSearch({})).toEqual({})
  })

  it('/live?serviceId=abc は useSearch の戻り値に文字列を残さない', async () => {
    // **`validateSearch` を直接呼ぶだけでは検出できない。** TanStack Router は
    // 非 strict モードで `{ ...生の location.search, ...validateSearch の戻り値 }`
    // の順に合成するので、`validateSearch` が**キーを省略**すると生の値
    // （文字列 "abc"）がそのまま残る。`LivePageSearch` は `serviceId?: number` と
    // 宣言しているので、これは型が実行時に嘘をついている状態になる（issue #194）。
    // 落とす次元にも undefined を明示代入して初めて消える
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/live?serviceId=abc'] }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { serviceId?: unknown }
    expect(search.serviceId).toBeUndefined()
    // 正しい値は通る（両方向を見る）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/live?serviceId=1024'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { serviceId?: unknown }).serviceId).toBe(1024)
  })

  it('/live の serviceId は非整数・0 以下も落とす', async () => {
    for (const raw of ['1.5', '0', '-1', 'Infinity']) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/live?serviceId=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { serviceId?: unknown }).serviceId).toBe(
        undefined,
      )
    }
  })

  it('/?serviceId=abc は useSearch の戻り値に文字列を残さない', async () => {
    // `/live` と同じ罠（issue #194 型。docs/frontend.md「TanStack Router の
    // validateSearch は無効な値を『省略』しても消えない」）。validateSearch を
    // 直接呼ぶだけでは検出できない --- 非 strict モードは生の
    // location.search の上に戻り値を重ねるので、キーを省略すると生の値が残る。
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/?serviceId=abc'] }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { serviceId?: unknown }
    expect(search.serviceId).toBeUndefined()
    // 正しい値は通る（両方向を見る）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/?serviceId=1024'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { serviceId?: unknown }).serviceId).toEqual([1024])
  })

  it('/ の serviceId は複数指定・混在配列を検証済みの配列に正規化する', async () => {
    // ?serviceId=1024&serviceId=abc&serviceId=0 のような、一部だけ不正な値が
    // 混ざった URL（手入力・古いブックマーク）でも、不正な要素だけを落として
    // 開ける
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({
        initialEntries: ['/?serviceId=1024&serviceId=abc&serviceId=0&serviceId=1032'],
      }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { serviceId?: unknown }
    expect(search.serviceId).toEqual([1024, 1032])
  })

  it('/search を開くと検索画面が出て、主ナビゲーションから辿れる', async () => {
    // jsdom は window.scrollTo を実装していない。ルーターのスクロール復元が
    // 呼ぶため、置いておかないと関係のない例外がログを埋める
    window.scrollTo = vi.fn()

    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      // SiteGate（routes.tsx）が全ルートの手前で GET /api/sites を待つ
      // （issue #184 M4-12）。空配列を返すと「利用可能なサイトがありません」に
      // 落ちて検索フォームまで辿り着けないため、他のパスとは別に応答する。
      const body = url.pathname === '/api/sites' ? ['default'] : []
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
