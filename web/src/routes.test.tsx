import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Recording, Reservation } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { SearchPage } from '@/pages/search'
import { routeTree } from '@/routes'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * stubSitesFetch は `GET /api/sites` にだけ応答し、他のパスは空配列で返す
 * `globalThis.fetch` のスタブ。`SiteGate`（routes.tsx）が全ルートの手前で
 * サイト解決を待つため、これが無いとどのルートも開けない。
 */
function stubSitesFetch() {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    const body = url.pathname === '/api/sites' ? ['default'] : []
    return Promise.resolve(jsonResponse(body))
  }) as unknown as typeof fetch
}

/**
 * stubDetailFetch は `stubSitesFetch` に加えて、詳細ルート
 * （`/recordings/$id` ・ `/reservations/$site/$programId`）が実際に描画するのに
 * 必要な単体取得エンドポイントをスキーマに沿った形で返す。
 *
 * **`[]` を返すだけでは足りない。** `GET /api/recordings/{id}` /
 * `GET /api/sites/{site}/programs/{programId}/reservation` は配列ではなく単一
 * オブジェクトを返す契約なので、`[]` を渡すと `recording.startAt` /
 * `reservation.durationMs` が `undefined` になり、`formatDateTime` /
 * `formatDuration`（`lib/format.ts`）が `new Date(undefined)` を
 * `Intl.DateTimeFormat.format` に渡して `RangeError: Invalid time value` を
 * 投げる。このエラーはどのルートも `errorComponent` を持たないため
 * `<HeadContent />` を含む rootRoute の component ごと汎用のフォールバック
 * 画面に置き換わり、`document.title` は空文字になる（`routes.tsx` の
 * `<HeadContent />` 直前のコメント参照）。`[]` スタブのままだと、詳細ルートの
 * title アサーションはこのクラッシュが起こる前の 28ms ほどの過渡状態を
 * 掴んで通ってしまう「非同期の空虚な成功」になる（CLAUDE.md テスト規律）。
 * `GET /api/sites/{site}/programs/{programId}/overlaps` も同じ理由で
 * `{ count: 0, reservations: [] }` の形が要る（`ProgramOverlapWarning` が
 * `overlaps.count` / `overlaps.reservations.map` を読むため）。
 */
function stubDetailFetch() {
  const recording: Recording = {
    id: 1,
    site: 'default',
    source: 'manual',
    serviceName: 'テスト放送局',
    channelType: 'GR',
    channel: '1',
    networkId: 1,
    serviceId: 1,
    eventId: 1,
    title: '録画詳細テスト番組',
    startAt: '2026-01-01T00:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-01T00:30:00Z',
  }
  const reservation: Reservation = {
    id: 1,
    site: 'default',
    programId: 1,
    source: 'manual',
    state: 'active',
    title: '予約詳細テスト番組',
    serviceName: 'テスト放送局',
    channelType: 'GR',
    startAt: '2026-01-01T00:00:00Z',
    durationMs: 1_800_000,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    skip: false,
  }

  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (/^\/api\/recordings\/\d+$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse(recording))
    }
    if (/^\/api\/sites\/[^/]+\/programs\/\d+\/reservation$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse(reservation))
    }
    if (/^\/api\/sites\/[^/]+\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }
    return Promise.resolve(jsonResponse([]))
  }) as unknown as typeof fetch
}

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
      '/programs',
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

  it('/search?ruleId=abc は useSearch の戻り値に文字列を残さない', async () => {
    // **`validateSearch` を直接呼ぶだけでは検出できない。** TanStack Router は
    // 非 strict モードで `{ ...生の location.search, ...validateSearch の戻り値 }`
    // の順に合成するので、`validateSearch` が**キーを省略**すると生の値
    // （文字列 "abc"）がそのまま残る（`/live` の service と同じ罠。issue #194）。
    // 落とす次元にも undefined を明示代入して初めて消える
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/search?ruleId=abc'] }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { ruleId?: unknown }
    expect(search.ruleId).toBeUndefined()
    // 正しい値は通る（両方向を見る）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/search?ruleId=42'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(42)
  })

  it('/search の ruleId は非整数・NaN・Infinity も落とす', async () => {
    for (const raw of ['1.5', 'NaN', 'Infinity', '-Infinity']) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/search?ruleId=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(undefined)
    }
  })

  // issue #275: `parseRuleId` が空文字を 0 に、安全整数の外を黙って丸めていた。
  // `validateSearch` 経由（`parseRuleId` の単体テストだけでは呼び出し側で本当に
  // 守られているか分からない）で固定する。
  it('/search の ruleId は空文字・0 以下・安全整数の外も落とす（issue #275）', async () => {
    for (const raw of ['', '0', '-1', '1e30', '9007199254740993']) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/search?ruleId=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(undefined)
    }
    // 指数表記は数値として一意なので通す（parseAt と同じ流儀）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/search?ruleId=1e3'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(1000)
  })

  it('/search を ruleId 無しで開くと undefined のまま', async () => {
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/search'] }),
    })
    await router.load()
    expect((router.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBeUndefined()
  })

  // issue #275: `/recordings` の `ruleId` も `parseRuleId`（`lib/recording-search.ts`
  // の `parseRecordingsSearch`）を経由するので、`/search` と同じ経路の穴が
  // `validateSearch` レベルで塞がっていることを固定する。
  it('/recordings の ruleId は空文字・0 以下・安全整数の外も落とす（issue #275）', async () => {
    for (const raw of ['', '0', '-1', '1e30', '9007199254740993']) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/recordings?ruleId=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(undefined)
    }
    // 正しい値・指数表記は通る（両方向を見る）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/recordings?ruleId=1e3'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { ruleId?: unknown }).ruleId).toBe(1000)
  })

  it('/live?service=abc は useSearch の戻り値に文字列を残さない', async () => {
    // **`validateSearch` を直接呼ぶだけでは検出できない。** TanStack Router は
    // 非 strict モードで `{ ...生の location.search, ...validateSearch の戻り値 }`
    // の順に合成するので、`validateSearch` が**キーを省略**すると生の値
    // （文字列 "abc"）がそのまま残る。`LivePageSearch` は `service?: number` と
    // 宣言しているので、これは型が実行時に嘘をついている状態になる（issue #194）。
    // 落とす次元にも undefined を明示代入して初めて消える
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/live?service=abc'] }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { service?: unknown }
    expect(search.service).toBeUndefined()
    // 正しい値は通る（両方向を見る）
    const ok = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/live?service=100020'] }),
    })
    await ok.load()
    expect((ok.state.matches.at(-1)!.search as { service?: unknown }).service).toBe(100020)
  })

  it('/live の service は非整数・0 以下・安全整数の外も落とす', async () => {
    // issue #275: `/live` の service パーサを `parsePositiveIntId`
    // （`lib/positive-id.ts`）へ寄せた。以前は `Number.isInteger(n) && n > 0` だけを
    // 見ており、`Number.MAX_SAFE_INTEGER` を超える値が黙って別の値に丸まる経路
    // （`9007199254740993` → `9007199254740992`）を塞いでいなかった。
    for (const raw of ['1.5', '0', '-1', 'Infinity', '1e30', '9007199254740993']) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/live?service=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { service?: unknown }).service).toBe(
        undefined,
      )
    }
  })

  /**
   * `/live` の `?service=` は `/programs` の同名パラメータと同じ id 空間なので、
   * 上限も同じ生成スキーマ（openapi の `maximum`）で見る --- 同じ名前の
   * パラメータが画面ごとに違う値域を持つと、片方だけが通る id ができる。
   * 期待値は openapi の `maximum`（`networkId` / `serviceId` とも 65535 の
   * ときの合成値）をリテラルで書く --- 生成定数を参照すると上限を動かしても
   * 通り続ける。
   */
  it('/live の service は Service.id の上限（openapi の maximum）を超えると落ちる', async () => {
    for (const [raw, expected] of [
      ['6553565536', undefined],
      ['6553565535', 6553565535],
    ] as const) {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: [`/live?service=${raw}`] }),
      })
      await router.load()
      expect((router.state.matches.at(-1)!.search as { service?: unknown }).service).toBe(expected)
    }
  })

  /**
   * issue #438: `/live` の URL は他画面と同じ `?service=<Service.id>` に統一し、
   * SI の 2 値をそのまま運ぶ旧クエリ形式の後方互換は持たない（`5ab06f8` と同じ
   * 判断）。`LivePageSearch` はその 2 値のキーを持たないので、旧形式のリンクを
   * 踏んでも `service` は常に undefined になる。
   */
  it('SI の 2 値をそのまま運ぶ旧クエリ形式は無視され、service は undefined になる', async () => {
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/live?networkId=32736&serviceId=1024'] }),
    })
    await router.load()

    const search = router.state.matches.at(-1)!.search as { service?: unknown }
    expect(search.service).toBeUndefined()
  })

  describe('ホーム新設（M8-3, issue #242）', () => {
    it('裸の / はホーム', async () => {
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: ['/'] }),
      })
      await router.load()

      expect(router.state.location.pathname).toBe('/')
    })

    it('/programs?service= は不正な要素だけを落として開く', async () => {
      // 一部だけ不正な組が混ざった URL（手入力・壊れた共有リンク）でも、
      // 不正な要素だけを落として画面は開く。
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({
          initialEntries: ['/programs?service=400101&service=bad&service=0&service=600101'],
        }),
      })
      await router.load()

      const search = router.state.matches.at(-1)!.search as { service?: unknown }
      expect(search.service).toEqual([400101, 600101])
    })

    it('「ホーム」ナビ（検索パラメータ無しの / への遷移）は search を持ち越さない', async () => {
      // `navigate({ to: '/' })` は search を明示しない限り引き継がない
      // （TanStack Router の既定）ので、番組表の絞り込みがホームに漏れない。
      const router = createRouter({
        routeTree,
        history: createMemoryHistory({ initialEntries: ['/programs?service=400101'] }),
      })
      await router.load()
      await router.navigate({ to: '/' })

      expect(router.state.location.pathname).toBe('/')
      expect(router.state.location.search).toEqual({})
    })
  })

  it('/search を開くと検索画面が出て、主ナビゲーションから辿れる', async () => {
    // jsdom は window.scrollTo を実装していない。ルーターのスクロール復元が
    // 呼ぶため、置いておかないと関係のない例外がログを埋める
    window.scrollTo = vi.fn()
    // SiteGate（routes.tsx）が全ルートの手前で GET /api/sites を待つ
    // （issue #184 M4-12）。空配列を返すと「利用可能なサイトがありません」に
    // 落ちて検索フォームまで辿り着けない。
    stubSitesFetch()

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

  /**
   * `waitFor` は最初にポーリングした時点で偽陽性になりうる ---
   * 詳細ルート（`/recordings/$id` ・ `/reservations/$site/$programId`）は
   * react-query の取得が解決した直後の 1 レンダーだけ正しい title を出し、
   * その次のレンダーでクラッシュして title が空文字に落ちることがある
   * （`stubDetailFetch` のコメント参照。`[]` のような形の合わないスタブを
   * 渡すとこれが起こる）。`waitFor` の 1 回目のポーリングがその一瞬を
   * 掴むと、画面が実際にはクラッシュしているのにアサーションだけ通る
   * （CLAUDE.md テスト規律「非同期の空虚な成功に注意する」）。ここでは
   * 期待値に届いたことを確認した後、少し待って同じ値のままであることも
   * 確認することで、一過性の値ではなく定常状態であることを固定する。
   */
  async function expectSteadyTitle(expected: string) {
    await waitFor(() => expect(document.title).toBe(expected))
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(document.title).toBe(expected)
  }

  it('ルートを変えると document.title が画面ごとに変わる（issue #304）', async () => {
    window.scrollTo = vi.fn()
    stubDetailFetch()

    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ['/'] }),
    })
    render(
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <ToastProvider>
          <RouterProvider router={router as never} />
        </ToastProvider>
      </QueryClientProvider>,
    )

    // どのルートも「画面名 · 録番」の形になる（index.html の固定 "Rokuban" の
    // まま止まらない）。主要ルート（issue #304 が Playwright で確認した 6 つ）
    // に加え、issue の一覧には無いが同じ理由（`document.title` はナビゲーション
    // だけでは前の値に戻らない）で `/live` と詳細 2 ルートも見る。
    await router.navigate({ to: '/' })
    await expectSteadyTitle('ホーム · 録番')

    await router.navigate({ to: '/programs' })
    await expectSteadyTitle('番組 · 録番')

    await router.navigate({ to: '/recordings' })
    await expectSteadyTitle('録画 · 録番')

    await router.navigate({ to: '/reservations' })
    await expectSteadyTitle('予約 · 録番')

    await router.navigate({ to: '/search' })
    await expectSteadyTitle('検索 · 録番')

    await router.navigate({ to: '/rules' })
    await expectSteadyTitle('ルール · 録番')

    await router.navigate({ to: '/live' })
    await expectSteadyTitle('ライブ · 録番')

    // 録画名を積んでいないので、単体取得のスタブが実在の録画を返す限り
    // `id` の値そのものは何でもよい。ただし `[]` のような形の合わない
    // レスポンスだとページがクラッシュして title が空文字に落ちる
    // （`stubDetailFetch` 参照）。それを検知するのが `expectSteadyTitle`。
    await router.navigate({ to: '/recordings/$id', params: { id: '1' } })
    await expectSteadyTitle('録画の詳細 · 録番')
    // ページ本体が実際に描画されている（クラッシュ後の汎用フォールバック
    // 画面ではない）ことも確認する --- title が定常でも、たまたま別の理由で
    // 空でない title を返す壊れ方だと `expectSteadyTitle` だけでは拾えない。
    expect(await screen.findByText('録画詳細テスト番組')).toBeInTheDocument()

    await router.navigate({
      to: '/reservations/$site/$programId',
      params: { site: 'default', programId: '1' },
    })
    await expectSteadyTitle('予約の詳細 · 録番')
    expect(await screen.findByText('予約詳細テスト番組')).toBeInTheDocument()
  })
})
