import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Reservation } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

function baseReservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    id: 111,
    site: 'default',
    programId: 300000,
    source: 'manual',
    state: 'active',
    title: 'テスト番組',
    startAt: dayStart.toISOString(),
    durationMs: 30 * 60_000,
    createdAt: dayStart.toISOString(),
    updatedAt: dayStart.toISOString(),
    skip: false,
    ...overrides,
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * stubFetch は AppShell（`/api/breakers`）・詳細画面本体・重なり警告
 * （`GET /api/sites/{site}/programs/{programId}/overlaps`）への問い合わせを
 * 振り分ける。`reservationOf` は `(site, programId)` から返す予約を引く関数で、
 * 再実体化（同じ `(site, programId)` でも呼び出しごとに違う `id` を返す）を
 * シミュレートできるようにする。`sites`（既定 `['default']`）は `<SiteGate>`
 * が返す `GET /api/sites` の応答 --- URL の `$site` と異なる値を渡せるように
 * している（下記「ゲート済み site と URL の site が違う」テスト参照）。
 */
function stubFetch(
  reservationOf: (site: string, programId: number) => Reservation | null,
  sites: string[] = ['default'],
) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))

    const reservationMatch = /^\/api\/sites\/([^/]+)\/programs\/(\d+)\/reservation$/.exec(
      url.pathname,
    )
    if (reservationMatch) {
      const [, site, programId] = reservationMatch
      const reservation = reservationOf(site, Number(programId))
      if (!reservation) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      return Promise.resolve(jsonResponse(reservation))
    }

    if (/^\/api\/sites\/[^/]+\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }

    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

function renderAt(path: string) {
  window.scrollTo = vi.fn()
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { queryClient, router }
}

describe('ReservationDetailPage', () => {
  // この issue (#99) の本体: ディープリンクは (site, programId) を宛先にする。
  // 予約が ruler の導出削除・再実体化で id を変えても、同じ URL がそのまま解決する
  // ことを、生成された TanStack Query フックの実際のクエリキー
  // （getGetProgramReservationQueryKey、id を含まない）を通して確認する。
  it('/reservations/$site/$programId が (site, programId) だけで解決する', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
    expect(screen.getByText('300000')).toBeInTheDocument()
  })

  // 核心: 予約行が再実体化されて id が変わっても、同じ URL のまま
  // （ナビゲーションもクエリキーの変更も無く）新しい内容に更新される。
  // reservations.id をクエリキーやルートパラメータに使っていれば、この経路は
  // 「別のキャッシュエントリ」または「別の URL」を要求するはずで、この
  // テストは id だけを変えた再取得が同じ画面にそのまま反映されることを見る。
  it('予約の再実体化（id の変化）を挟んでも同じ URL のまま新しい内容に更新される', async () => {
    let currentId = 111
    const fetchMock = stubFetch((site, programId) =>
      site === 'default' && programId === 300000
        ? baseReservation({ id: currentId, title: `番組 (id=${currentId})` })
        : null,
    )

    const { queryClient } = renderAt('/reservations/default/300000')

    expect(await screen.findByText('番組 (id=111)')).toBeInTheDocument()

    // ruler の導出削除・再実体化を模す: 同じ (site, programId) だが id が変わる。
    currentId = 222
    await queryClient.invalidateQueries({
      queryKey: ['/api/reservations', 'detail', 'default', 300000],
    })

    await waitFor(() => expect(screen.getByText('番組 (id=222)')).toBeInTheDocument())
    // URL 自体は変わっていない（再取得だけで済んでいる = ナビゲーション不要）。
    expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('/reservation'))).toBe(true)
  })

  // このページのクエリキーの**先頭要素**が一覧と同じ '/api/reservations' で
  // あることを、実際に使われる経路（前方一致の invalidate → 再取得 → 表示の
  // 更新）で固定する。orval の生成キー
  // （['/api/sites/{site}/programs/{programId}/reservation'] の 1 要素）に戻すと、
  // TanStack Query の前方一致は先頭要素の比較なのでこの invalidate が届かず、
  // SSE の `reservations` トピックも `lib/events.ts` の 60 秒の定期 invalidate も
  // このページを素通りする（代わりに '/api/sites/' に掛かって EPG の 10 分側で
  // しか収束しなくなる）。
  it('予約一覧の invalidate（[\'/api/reservations\']）が詳細ページにも届く', async () => {
    let title = '更新前のタイトル'
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation({ title }) : null,
    )

    const { queryClient } = renderAt('/reservations/default/300000')

    // 初回の表示を観測してから始める（「何も起きないまま成功」を避ける）
    expect(await screen.findByText('更新前のタイトル')).toBeInTheDocument()

    // SSE の reservations トピック・定期 invalidate・一覧側の mutater が
    // 使うのと同じフィルタ
    title = '更新後のタイトル'
    await queryClient.invalidateQueries({ queryKey: ['/api/reservations'] })

    await waitFor(() => expect(screen.getByText('更新後のタイトル')).toBeInTheDocument())
  })

  it('存在しない (site, programId) は「見つかりません」を表示する', async () => {
    stubFetch(() => null)

    renderAt('/reservations/default/999999')

    expect(await screen.findByText('予約が見つかりません')).toBeInTheDocument()
  })

  // `ProgramOverlapWarning` に `useCurrentSite()`（<SiteGate> が配る「現在の
  // site」）ではなく URL の `$site` を明示的に渡していることを固定する。
  //
  // このページの route は `/reservations/$site/$programId` で、`$site` は
  // ディープリンクが指す資源そのものの一部（issue #99）。一方 `<SiteGate>` が
  // 配る「現在の site」はレジストリの先頭サイトに過ぎない（issue #184
  // M4-12、サイト切り替え UI を持たない決定）。この 2 つはレジストリが 2 サイト
  // 以上のとき一致するとは限らないので、`ReservationDetailPage` が
  // `ProgramOverlapWarning` に `useCurrentSite()` を渡す実装に戻すと、
  // 対象と異なる site の重なりを問い合わせてしまう --- ここでは `<SiteGate>`
  // が返す「現在の site」（tokyo）と URL の `$site`（osaka）を意図的に
  // 違えて、実際に叩かれる overlaps の URL が osaka であることを見る。
  it('重なり警告は URL の $site を使う（<SiteGate> の現在の site とは独立）', async () => {
    const fetchMock = stubFetch(
      (site, programId) =>
        site === 'osaka' && programId === 300000 ? baseReservation({ site: 'osaka' }) : null,
      ['tokyo', 'osaka'],
    )

    renderAt('/reservations/osaka/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()

    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some((c) => String(c[0]).includes('/programs/300000/overlaps')),
      ).toBe(true),
    )
    const overlapsCall = fetchMock.mock.calls.find((c) =>
      String(c[0]).includes('/programs/300000/overlaps'),
    )
    expect(String(overlapsCall?.[0])).toBe('/api/sites/osaka/programs/300000/overlaps')
  })
})
