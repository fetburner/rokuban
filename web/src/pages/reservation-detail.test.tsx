import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, Reservation, Rule } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

function baseReservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    site: 'default',
    programId: 300000,
    source: 'manual',
    state: 'active',
    title: 'テスト番組',
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: dayStart.toISOString(),
    durationMs: 30 * 60_000,
    createdAt: dayStart.toISOString(),
    updatedAt: dayStart.toISOString(),
    skip: false,
    series: null,
    ...overrides,
  }
}

function baseProgram(overrides: Record<string, unknown> = {}) {
  return {
    programId: 300000,
    networkId: 1,
    serviceId: 1,
    eventId: 1,
    startAt: dayStart.toISOString(),
    endAt: new Date(dayStart.getTime() + 30 * 60_000).toISOString(),
    durationMs: 30 * 60_000,
    name: 'テスト番組',
    description: '',
    genres: [],
    isFree: true,
    extended: {},
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

function errorResponse(status: number, message: string): Response {
  return jsonResponse({ error: message }, status)
}

/**
 * stubFetch は AppShell（`/api/breakers`）・詳細画面本体・重なり警告
 * （`GET /api/sites/{site}/programs/{programId}/overlaps`）・ルール一覧
 * （`GET /api/rules`）・エンコードプロファイル一覧（`GET /api/encode-profiles`）
 * への問い合わせを振り分ける。`reservationOf` は `(site, programId)` から
 * 返す予約を引く関数で、再実体化（同じ `(site, programId)` でも呼び出しごとに
 * 違う内容を返す）をシミュレートできるようにする。`sites`（既定
 * `['default']`）は `GET /api/sites` の応答 --- URL の
 * `$site` と異なる値を渡せるようにしている（下記「ゲート済み site と URL の
 * site が違う」テスト参照）。`rules`（既定 `[]`）はルール名の解決先
 * （issue #300、`components/recording-detail-panel.tsx` の `RuleSection` と
 * 同じ `useListRules` キャッシュを引く）。`intentPutResponse` は
 * `PUT .../intent`（予約取消 /
 * 手動予約の Undo）の応答を差し替える（既定は 204 成功。サーバー本文つき
 * 失敗テスト用）。DELETE（ルール由来の Undo）は常に 204 で応答する。
 */
function stubFetch(
  reservationOf: (site: string, programId: number) => Reservation | Response | null,
  sites: string[] = ['default'],
  rules: Rule[] = [],
  intentPutResponse?: () => Response | Promise<Response>,
  programOf: (site: string, programId: number) => Record<string, unknown> | Response | null = () =>
    baseProgram(),
  capacityOverages: CapacityOverage[] = [],
) {
  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // サイトレジストリを先に解決する。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    // EncodeOverridesEditor（エンコードと保持セクション）が必ず引く。
    // 中身はこのファイルのテストの関心事ではないので既定は空配列。
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse([]))
    // 取消成功時に navigate する先（ReservationsPage）が引く。中身はこの
    // ファイルの関心事ではないので既定は空配列。Undo（`invalidateQueries`）の
    // 再取得もここを通る。
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/capacity/overages') return Promise.resolve(jsonResponse(capacityOverages))

    if (/^\/api\/sites\/[^/]+\/programs\/\d+\/intent$/.test(url.pathname) && init?.method === 'PUT') {
      return Promise.resolve(intentPutResponse?.() ?? new Response(null, { status: 204 }))
    }
    // ルール由来の Undo（DELETE .../intent。「意見を取り下げてルール評価に
    // 戻す」）はこのファイルのテストで常に成功させれば足りる --- 失敗経路は
    // PUT 側（手動予約の Undo）で既に確認済みで、同じ `onError` を共有する。
    if (
      /^\/api\/sites\/[^/]+\/programs\/\d+\/intent$/.test(url.pathname) &&
      init?.method === 'DELETE'
    ) {
      return Promise.resolve(new Response(null, { status: 204 }))
    }

    const reservationMatch = /^\/api\/sites\/([^/]+)\/programs\/(\d+)\/reservation$/.exec(
      url.pathname,
    )
    if (reservationMatch) {
      const [, site, programId] = reservationMatch
      const reservation = reservationOf(site, Number(programId))
      if (reservation instanceof Response) return Promise.resolve(reservation)
      if (!reservation) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      return Promise.resolve(jsonResponse(reservation))
    }

    const programMatch = /^\/api\/sites\/([^/]+)\/programs\/(\d+)$/.exec(url.pathname)
    if (programMatch) {
      const [, site, programId] = programMatch
      const program = programOf(site, Number(programId))
      if (program instanceof Response) return Promise.resolve(program)
      if (!program) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      return Promise.resolve(jsonResponse(program))
    }

    if (/^\/api\/sites\/[^/]+\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }

    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

// テストが作った QueryClient。終了時に clear() して、retry タイマーなどが teardown 後に
// 発火して `window is not defined` を起こすのを防ぐ安全網にする。
const clients: QueryClient[] = []

afterEach(() => {
  // vitest は globals 無しなので RTL の自動 cleanup は登録されず、cleanup は
  // src/test/setup.ts が afterEach で明示登録している。後に登録した afterEach が先に
  // 走るため、このファイルの afterEach は setup.ts の cleanup より先に走り、キャッシュを
  // clear する時点でまだマウント中になる。ここで先に unmount して、マウント中の React
  // observer へ通知が飛ばないようにする。
  cleanup()
  for (const c of clients.splice(0)) c.clear()
})

function renderAt(
  path: string,
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
) {
  clients.push(queryClient)
  window.scrollTo = vi.fn()
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

async function openReservationOverflow(
  user: Pick<ReturnType<typeof userEvent.setup>, 'click'>,
) {
  // 取消は主操作の位置に置かない: メニューを開く前は画面に出ていない。
  expect(screen.queryByRole('button', { name: '予約を取消' })).not.toBeInTheDocument()
  const trigger = await screen.findByRole('button', { name: '予約のその他の操作' })
  await screen.findByRole('heading', { name: 'テスト番組' })
  // 取消は主操作の位置に置かない: メニューを開く前は画面に出ていない。
  expect(screen.queryByRole('button', { name: '予約を取消' })).not.toBeInTheDocument()
  await user.click(trigger)
  await screen.findByRole('menuitem', { name: '予約を取消' })
}

async function cancelFromOverflow(
  user: Pick<ReturnType<typeof userEvent.setup>, 'click'>,
) {
  await openReservationOverflow(user)
  await user.click(screen.getByRole('menuitem', { name: '予約を取消' }))
}

describe('ReservationDetailPage', () => {
  // この issue (#99) の本体: ディープリンクは (site, programId) を宛先にする。
  // 予約行が ruler の導出削除・再実体化を経ても、同じ URL がそのまま解決する
  // ことを、生成された TanStack Query フックの実際のクエリキー
  // （getGetProgramReservationQueryKey、site/programId だけを含む）を通して確認する。
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

  // issue #467: PageHeader の leading スロットに乗せても「戻る」は
  // 一覧へのリンクのまま変えない。
  it('「戻る」は /reservations へのリンク', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByRole('link', { name: '戻る' })).toHaveAttribute(
      'href',
      '/reservations',
    )
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

  it('タイトルは番組表リンクではなく、局・開始時刻・尺の行が該当時刻の番組表へリンクする', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    const title = await screen.findByRole('heading', { name: 'テスト番組' })
    expect(title.querySelector('a')).toBeNull()
    const link = await screen.findByTestId('reservation-program-link')
    const destination = new URL((link as HTMLAnchorElement).href)
    expect(destination.pathname).toBe('/programs')
    expect(destination.searchParams.get('view')).toBe('grid')
    expect(destination.searchParams.get('at')).toBe(String(dayStart.getTime()))
    expect(link).toHaveTextContent('テスト局')
  })

  it('録画設定に保存先パスは表示しない', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
    expect(screen.queryByText('保存先パス')).not.toBeInTheDocument()
  })

  it('番組説明と拡張情報を予約タイトルの下に表示する', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      () =>
        baseProgram({
          description: '地域の人々の暮らしを紹介します。',
          extended: { 出演者: '山田花子' },
        }),
    )

    renderAt('/reservations/default/300000')

    const title = await screen.findByRole('heading', { name: 'テスト番組' })
    const description = await screen.findByText('地域の人々の暮らしを紹介します。')
    expect(screen.getByText('出演者')).toBeInTheDocument()
    expect(screen.getByText('山田花子')).toBeInTheDocument()
    // 題名の塊（題名・局・時刻）を分断しない: 説明は局・時刻の行より後ろ、「予約」欄より前。
    const link = screen.getByTestId('reservation-program-link')
    const follows = (a: Node, b: Node) =>
      Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING)
    expect(follows(title, link)).toBe(true)
    expect(follows(link, description)).toBe(true)
    expect(follows(description, screen.getByText('出自'))).toBe(true)
  })

  it('番組情報が 404 でも予約詳細は残し、番組詳細だけを隠す', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      () => errorResponse(404, 'program not found'),
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByRole('heading', { name: 'テスト番組' })).toBeInTheDocument()
    expect(await screen.findByText('録画予定')).toBeInTheDocument()
    expect(screen.queryByText('番組情報の取得に失敗しました')).not.toBeInTheDocument()
    expect(screen.queryByText('詳細の取得に失敗しました')).not.toBeInTheDocument()
  })

  // main.tsx の QueryClient は retry 既定（3 回、1s/2s/4s）。404 は初回応答で隠す。
  it('番組情報の 404 は retry せず初回応答で読み込み中表示を消す', async () => {
    const fetchMock = stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      () => errorResponse(404, 'program not found'),
    )

    // retry は既定（3 回）のまま待ち時間だけ 0 にする。404 の retry 除外を外すと
    // 番組 GET が 4 回になって落ちる。capabilities などスタブ外のクエリも即座に
    // retry を使い切るので、テスト終了後にタイマーが残らない。
    renderAt(
      '/reservations/default/300000',
      new QueryClient({ defaultOptions: { queries: { retryDelay: 0 } } }),
    )

    await screen.findByRole('heading', { name: 'テスト番組' })
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())
    const programGets = fetchMock.mock.calls.filter(([input]) =>
      /\/programs\/300000$/.test(new URL(String(input), 'http://localhost').pathname),
    )
    expect(programGets).toHaveLength(1)
  })

  it('番組情報の 5xx は指定の文言で表示する', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      () => errorResponse(503, 'epg unavailable'),
    )

    // retry（3 回）は残し、待ち時間だけ 0 にする。
    renderAt(
      '/reservations/default/300000',
      new QueryClient({ defaultOptions: { queries: { retryDelay: 0 } } }),
    )

    expect(await screen.findByText('番組情報の取得に失敗しました')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'テスト番組' })).toBeInTheDocument()
  })

  it('番組情報の 5xx 以外のエラーでは 5xx 用の文言を出さない', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      () => errorResponse(400, 'bad request'),
    )

    renderAt(
      '/reservations/default/300000',
      new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } }),
    )

    expect(await screen.findByRole('heading', { name: 'テスト番組' })).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())
    expect(screen.queryByText('番組情報の取得に失敗しました')).not.toBeInTheDocument()
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

  // 核心: 予約行が再実体化されても、同じ URL のまま
  // （ナビゲーションもクエリキーの変更も無く）新しい内容に更新される。
  // 複合キー以外をクエリキーやルートパラメータに使っていれば、この経路は
  // 「別のキャッシュエントリ」または「別の URL」を要求するはずで、この
  // テストは内容だけを変えた再取得が同じ画面にそのまま反映されることを見る。
  it('予約の再実体化を挟んでも同じ URL のまま新しい内容に更新される', async () => {
    let currentVersion = 111
    const fetchMock = stubFetch((site, programId) =>
      site === 'default' && programId === 300000
        ? baseReservation({ title: `番組 (version=${currentVersion})` })
        : null,
    )

    const { queryClient } = renderAt('/reservations/default/300000')

    expect(await screen.findByText('番組 (version=111)')).toBeInTheDocument()

    // ruler の導出削除・再実体化を模す: 同じ (site, programId) だが内容が更新される。
    currentVersion = 222
    await queryClient.invalidateQueries({
      queryKey: ['/api/reservations', 'detail', 'default', 300000],
    })

    await waitFor(() => expect(screen.getByText('番組 (version=222)')).toBeInTheDocument())
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

  it('予約直後に詳細が 404 なら、まだ無いともう無いの両方で真になる文言を出す', async () => {
    stubFetch(() => null)

    renderAt('/reservations/default/999999')

    expect(
      await screen.findByText('予約が見つかりません（予約した直後なら、作成され次第ここに出ます）'),
    ).toBeInTheDocument()
    // 404 は SSE の invalidate で自動的に出るので再試行ボタンは冗長
    // （issue #467 レビュー。onRetry を無条件に付けると落ちる）。
    expect(screen.queryByRole('button', { name: '再試行' })).not.toBeInTheDocument()
  })

  it('404 以外の失敗は予約が見つからないと誤案内しない', async () => {
    stubFetch(() => errorResponse(500, 'database unavailable'))

    renderAt('/reservations/default/300000')

    expect(await screen.findByText('予約の取得に失敗しました')).toBeInTheDocument()
    expect(
      screen.queryByText('予約が見つかりません（予約した直後なら、作成され次第ここに出ます）'),
    ).not.toBeInTheDocument()
    // 純粋な取得失敗には再試行ボタンを出す（issue #467 レビュー。onRetry を
    // 外すと落ちる）。
    expect(screen.getByRole('button', { name: '再試行' })).toBeInTheDocument()
  })

  // 予約 intent の直後で ruler がまだ行を作っていない → 404 になっても、
  // `reservations` への INSERT が SSE トピック `reservations` を発火し、この
  // ページのクエリキー（先頭要素 `/api/reservations`）がそのグループに
  // 入っているので、再マウントなしに自動で詳細が出る（手動リロード不要）。
  // `lib/events.ts` の `invalidateGroup` と同じ形の predicate をここから
  // 直接撃って SSE 受信を模す（stubFetch は最初 404、途中から予約を返す）。
  it('404 のあと予約行ができたら、再マウントなしに詳細が出る', async () => {
    let ready = false
    stubFetch((site, programId) =>
      ready && site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    const { queryClient } = renderAt('/reservations/default/300000')

    expect(
      await screen.findByText('予約が見つかりません（予約した直後なら、作成され次第ここに出ます）'),
    ).toBeInTheDocument()

    ready = true
    await queryClient.invalidateQueries({
      predicate: (query) => {
        const first = query.queryKey[0]
        return typeof first === 'string' && first.startsWith('/api/reservations')
      },
    })

    expect(await screen.findByText('テスト番組')).toBeInTheDocument()
  })

  // `ProgramOverlapWarning` に画面全体の site ではなく URL の `$site` を
  // 明示的に渡していることを固定する。
  //
  // このページの route は `/reservations/$site/$programId` で、`$site` は
  // ディープリンクが指す資源そのものの一部（issue #99）。一方、共通の site は
  // レジストリの先頭サイトに過ぎない
  // M4-12、サイト切り替え UI を持たない決定）。この 2 つはレジストリが 2 サイト
  // 以上のとき一致するとは限らないので、`ReservationDetailPage` が
  // 対象と異なる site の重なりを問い合わせてしまう --- ここでは共通の site
  // （tokyo）と URL の `$site`（osaka）を意図的に
  // 違えて、実際に叩かれる overlaps の URL が osaka であることを見る。
  it('重なり警告は URL の $site を使う（共通の site とは独立）', async () => {
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

  // 結論は予約の見出し直下に置き、state enum の翻訳を状態欄には出さない。
  it.each([
    ['active', '録画予定'],
    ['detached', '録画予定'],
    ['orphaned', '録画されませんでした（録画が開始されませんでした）'],
  ] as const)('state=%s の結論「%s」を見出しに出し状態欄を持たない', async (state, verdict) => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation({ state }) : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText(verdict)).toBeInTheDocument()
    expect(screen.queryByText('状態')).not.toBeInTheDocument()
    expect(screen.queryByText('有効')).not.toBeInTheDocument()
    expect(screen.queryByText('ルール外')).not.toBeInTheDocument()
    expect(screen.queryByText('EPG から消失')).not.toBeInTheDocument()
    expect(screen.queryByText(state)).not.toBeInTheDocument()
    if (state === 'detached') {
      expect(screen.getByText('手動・ルール条件外')).toBeInTheDocument()
    }
  })

  it('重複スキップの結論に録画リンクと類似度を含める', async () => {
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000
        ? baseReservation({ skip: true, dedupMatchRecordingId: 42, dedupSimilarity: 0.875 })
        : null,
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByText(/録画しません（重複:/)).toBeInTheDocument()
    const link = screen.getByRole('link', { name: '録画 #42' })
    expect(link).toHaveAttribute('href', '/recordings/42')
    expect(screen.getByText(/類似度 0\.88/)).toBeInTheDocument()
  })

  it('容量不足の結論は共通の不足メッセージを使う', async () => {
    const startMs = dayStart.getTime()
    stubFetch(
      (site, programId) => site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      undefined,
      undefined,
      [{
        site: 'default',
        startAt: new Date(startMs).toISOString(),
        endAt: new Date(startMs + 30 * 60_000).toISOString(),
        shortfall: 1,
        jammedTypes: ['GR'],
      }],
    )

    renderAt('/reservations/default/300000')

    expect(
      await screen.findByText('録画予定。この時間帯はチューナーが不足しています（GR が 1 本不足）'),
    ).toBeInTheDocument()
  })

  // issue #300: ルールは名前で出す。ルール一覧（`useListRules`）に該当ルール
  // があれば名前をリンクテキストにし、リンク先はルールの実質的な編集画面
  // `/search?ruleId=N`（`components/recording-detail-panel.tsx` の
  // `RuleSection` と同じ着地先）。
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

  it('同名のルールは予約詳細のリンクでも id を添えて押し分ける', async () => {
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000
          ? baseReservation({ source: 'rule', ruleId: 7 })
          : null,
      ['default'],
      [sampleRule({ id: 7, name: '同名ルール' }), sampleRule({ id: 8, name: '同名ルール' })],
    )

    renderAt('/reservations/default/300000')

    expect(await screen.findByRole('link', { name: '同名ルール (#7)' })).toHaveAttribute(
      'href',
      '/search?ruleId=7',
    )
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

  // issue #457: intent PUT のスタブに成功既定（204）の分岐を足した以上、
  // それを通る経路も固定する（死んだ分岐のまま残さない）。
  //
  // 取消は一覧へ遷移する（旧実装のまま）。`skip` 意図だけの予約行は ruler が
  // 次パスで削除するため、遷移せずこの画面に留まる形は成立しない ---
  // 留まると詳細の GET がやがて 404 になり「予約が見つかりません」に落ちる
  // ことは「存在しない (site, programId) は『見つかりません』を表示する」
  // テスト（本ファイル）が既に固定している。
  it('予約取消が成功すると、トーストが出て /reservations へ遷移する', async () => {
    const user = userEvent.setup()
    stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation() : null,
    )

    const { router } = renderAt('/reservations/default/300000')

    await cancelFromOverflow(user)

    expect(await screen.findByText('予約を取消しました')).toBeInTheDocument()
    await waitFor(() => expect(router.state.location.pathname).toBe('/reservations'))
  })

  // issue #457: 予約取消（intent の PUT）が失敗したとき、サーバーの本文
  // （`apiErrorMessage`）を汎用文言に付け加える。
  it('予約取消が失敗すると、汎用文言にサーバー本文を付けたトーストが出る', async () => {
    const user = userEvent.setup()
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      () => errorResponse(409, 'reservation already cleared'),
    )

    renderAt('/reservations/default/300000')

    await cancelFromOverflow(user)

    expect(
      await screen.findByText('予約の取消に失敗しました: reservation already cleared'),
    ).toBeInTheDocument()
  })

  // issue #453: 詳細の取消は Undo で守る（確認ダイアログではなく一覧・
  // グリッドと同じ Undo に揃えた）。取消は一覧へ遷移するが、`ToastProvider`
  // は `main.tsx` で `RouterProvider` の外側にあるため、トースト本体と
  // Undo のクロージャは遷移後も生きている --- ここではその生存を実際の
  // 遷移を経由して確認する（コンポーネントをアンマウントしないまま
  // ボタンを押すテストは、遷移で切れる経路を検証したことにならない）。
  it('取消後に一覧へ遷移しても、トーストの「元に戻す」で PUT {action: record} が飛ぶ（手動予約）', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch((site, programId) =>
      site === 'default' && programId === 300000 ? baseReservation({ source: 'manual' }) : null,
    )

    const { router } = renderAt('/reservations/default/300000')

    await cancelFromOverflow(user)
    await waitFor(() => expect(router.state.location.pathname).toBe('/reservations'))

    await user.click(await screen.findByRole('button', { name: '元に戻す' }))
    expect(await screen.findByText('予約を元に戻しました')).toBeInTheDocument()

    const reviveCall = fetchMock.mock.calls.find((call) => {
      const url = new URL(String(call[0]), 'http://localhost')
      const init = call[1] as RequestInit | undefined
      return (
        url.pathname === '/api/sites/default/programs/300000/intent' &&
        init?.method === 'PUT' &&
        JSON.parse(String(init.body)).action === 'record'
      )
    })
    expect(reviveCall).toBeDefined()
  })

  // ルール由来の予約は `PUT intent{record}` で戻すと「明示的に record を
  // 主張した予約」に変わり、以後ルールがマッチしなくなっても居座ってしまう
  // （`internal/api/handler.go` の source 導出）。厳密な逆操作は
  // `DELETE .../intent`（意見を取り下げてルール評価に戻す）。
  it('ルール由来の予約の Undo は DELETE .../intent を送る（PUT ではない）', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch((site, programId) =>
      site === 'default' && programId === 300000
        ? baseReservation({ source: 'rule', ruleId: 7 })
        : null,
    )

    renderAt('/reservations/default/300000')

    await cancelFromOverflow(user)
    await user.click(await screen.findByRole('button', { name: '元に戻す' }))
    expect(await screen.findByText('予約を元に戻しました')).toBeInTheDocument()

    const intentCalls = fetchMock.mock.calls.filter(
      (call) =>
        new URL(String(call[0]), 'http://localhost').pathname ===
        '/api/sites/default/programs/300000/intent',
    )
    const reviveCall = intentCalls.find((call) => (call[1] as RequestInit | undefined)?.method === 'DELETE')
    expect(reviveCall).toBeDefined()
    // PUT は取消（skip）の 1 回だけで、Undo としての PUT は飛んでいない
    expect(intentCalls.filter((call) => (call[1] as RequestInit | undefined)?.method === 'PUT')).toHaveLength(1)
  })

  // Undo の失敗（両方向の確認。CLAUDE.md テスト規律）: 成功トーストではなく
  // 復帰失敗のトーストが出る。
  it('Undo が失敗すると、予約への復帰に失敗した旨のトーストが出る（成功トーストは出ない）', async () => {
    const user = userEvent.setup()
    let putCalls = 0
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation({ source: 'manual' }) : null,
      ['default'],
      [],
      () => {
        putCalls++
        // 1 回目（取消）は成功、2 回目（Undo）だけ失敗させる
        return putCalls === 2 ? errorResponse(500, '') : new Response(null, { status: 204 })
      },
    )

    renderAt('/reservations/default/300000')

    await cancelFromOverflow(user)
    await screen.findByRole('button', { name: '元に戻す' })
    await user.click(screen.getByRole('button', { name: '元に戻す' }))

    expect(await screen.findByText('予約への復帰に失敗しました')).toBeInTheDocument()
    expect(screen.queryByText('予約を元に戻しました')).not.toBeInTheDocument()
  })

  it('取消リクエスト中はその他メニューの取消項目を無効にする', async () => {
    const user = userEvent.setup()
    let finishRequest: ((response: Response) => void) | undefined
    const pending = new Promise<Response>((resolve) => {
      finishRequest = resolve
    })
    stubFetch(
      (site, programId) =>
        site === 'default' && programId === 300000 ? baseReservation() : null,
      ['default'],
      [],
      () => pending,
    )

    renderAt('/reservations/default/300000')

    await openReservationOverflow(user)
    await user.click(screen.getByRole('menuitem', { name: '予約を取消' }))
    await user.click(screen.getByRole('button', { name: '予約のその他の操作' }))
    expect(await screen.findByRole('menuitem', { name: '取消中…' })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
    finishRequest?.(new Response(null, { status: 204 }))
  })
})
