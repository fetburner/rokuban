import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Reservation, Rule } from '@/api/generated'
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
    serviceName: 'テスト局',
    startAt: dayStart.toISOString(),
    durationMs: 30 * 60_000,
    createdAt: dayStart.toISOString(),
    updatedAt: dayStart.toISOString(),
    skip: false,
    ...overrides,
  }
}

function sampleRule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 1,
    name: 'サンプルルール',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: dayStart.toISOString(),
    updatedAt: dayStart.toISOString(),
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
 * （`GET /api/sites/{site}/programs/{programId}/overlaps`）・ルール一覧
 * （`GET /api/rules`）・エンコードプロファイル一覧（`GET /api/encode-profiles`）
 * への問い合わせを振り分ける。`reservationOf` は `(site, programId)` から
 * 返す予約を引く関数で、再実体化（同じ `(site, programId)` でも呼び出しごとに
 * 違う `id` を返す）をシミュレートできるようにする。`sites`（既定
 * `['default']`）は `<SiteGate>` が返す `GET /api/sites` の応答 --- URL の
 * `$site` と異なる値を渡せるようにしている（下記「ゲート済み site と URL の
 * site が違う」テスト参照）。`rules`（既定 `[]`）はルール名の解決先
 * （issue #300、`pages/recordings.tsx` の `RuleSection` と同じ `useListRules`
 * キャッシュを引く）。
 */
function stubFetch(
  reservationOf: (site: string, programId: number) => Reservation | null,
  sites: string[] = ['default'],
  rules: Rule[] = [],
) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    // EncodeOverridesEditor（エンコードと保持セクション）が必ず引く。
    // 中身はこのファイルのテストの関心事ではないので既定は空配列。
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse([]))

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
  // `programId` はこの URL の宛先であって画面のフィールドではない（issue #300、
  // 「programId をフィールドとして出さない」テスト参照）ので、ここでは資源の
  // 同定がタイトルの表示で確認できれば足りる。
  it('/reservations/$site/$programId が (site, programId) だけで解決する', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
  })

  // issue #302: 予約詳細に局名を出す。同じタイトルが日付・局違いで並ぶと
  // 予約一覧・ホームでは区別できても、詳細画面単体では局名が無いと
  // どの局の予約かが分からない。
  it('局名（program_snapshots.service_name 由来）を出す', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000
        ? baseReservation({ serviceName: 'NHK総合' })
        : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
    // 局名は日時・尺と同じ <p> 内で中点区切りのテキストになる（`getByText` の
    // 完全一致はこの要素全体の文字列にしか当たらないため、部分一致で見る）。
    expect(screen.getByText(/NHK総合/)).toBeInTheDocument()
  })

  // 局名が空文字のときに裸の区切りが残らない。`serviceName` は openapi で
  // required だが空文字を禁じていないので、無条件連結（`{serviceName} · ...`）だと
  // 先頭に「· 」が出る。期待値はリテラルで書く（実装の式と比べても何も主張しない）。
  it('局名が空文字なら先頭に裸の中点を出さない', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation({ serviceName: '' }) : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
    expect(screen.getByText('7/25 00:00 · 30分')).toBeInTheDocument()
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

  // issue #300: 状態は一覧（`lib/reservation-labels.ts` の `stateLabels`）と同じ
  // 日本語ラベルで出る。生の `reservation.state`（'active' 等）をそのまま
  // 出すと、一覧では「有効」「ルール外」「EPG から消失」と読める状態がここでは
  // 読めなくなる。3 状態すべてを固定する --- `active` だけを見て通すテストは
  // `stateLabels.active` を書き換えても落ちないので何も保証しない。
  it.each([
    ['active', '有効'],
    ['detached', 'ルール外'],
    ['orphaned', 'EPG から消失'],
  ] as const)('状態 %s は一覧と同じ日本語ラベル「%s」で出る（生の state 値ではない）', async (state, label) => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation({ state }) : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText(label)).toBeInTheDocument()
    expect(screen.queryByText(state)).not.toBeInTheDocument()
  })

  // issue #300: ルールは名前で出す。ルール一覧（`useListRules`）に該当ルール
  // があれば名前をリンクテキストにし、リンク先はルールの実質的な編集画面
  // `/search?ruleId=N`（`pages/recordings.tsx` の `RuleSection` と同じ着地先）。
  it('ルールは名前で出て、名前は /search?ruleId= のルール編集画面へのリンクになる', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000
          ? baseReservation({ source: 'rule', ruleId: 7 })
          : null,
      ['default'],
      [sampleRule({ id: 7, name: 'ゆう6かがわ' })],
    )

    renderAt('/reservations/default/300000')

    const link = await screen.findByRole('link', { name: 'ゆう6かがわ' })
    expect(link).toHaveAttribute('href', '/search?ruleId=7')
    expect(screen.queryByText('#7')).not.toBeInTheDocument()
  })

  // issue #300: ルール一覧にまだ該当ルールが無い間（一覧が未解決・失敗、また
  // は返ってきた一覧にその id がまだ無い一時的な状態）だけ `#N` に落ちる。
  it('ルール一覧に該当ルールが無い間は #N に落ちる', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000
          ? baseReservation({ source: 'rule', ruleId: 42 })
          : null,
      ['default'],
      [],
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByRole('link', { name: '#42' })).toHaveAttribute(
      'href',
      '/search?ruleId=42',
    )
  })

  // issue #300: programId は URL の宛先であって利用者が読むフィールドでは
  // ない。フィールドとして出さない。
  it('programId をフィールドとして出さない', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    await screen.findByText('テスト番組')
    expect(screen.queryByText('programId')).not.toBeInTheDocument()
    expect(screen.queryByText('300000')).not.toBeInTheDocument()
  })

  // issue #300: 画面に issue 番号・設定キー名が出ない。実装の経緯・設定ファイル
  // のキー名は開発者向けの実装メモであって、利用者が読む画面には出さない。
  it('画面に issue 番号・設定キー名が出ない', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    await screen.findByText('テスト番組')
    // エンコードプロファイル一覧（既定で空配列に stub 済み）の解決を待って
    // から判定する。解決前に判定すると「まだ何も出ていない」空虚な成功になる。
    await screen.findByText(/エンコードプロファイルが設定されていません/)

    expect(screen.queryByText(/#19/)).not.toBeInTheDocument()
    expect(screen.queryByText(/config\.encode\.profiles/)).not.toBeInTheDocument()
  })
})
