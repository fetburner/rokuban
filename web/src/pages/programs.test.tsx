import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, ProgramListItem, Reservation, Service } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { dayOrigin } from '@/lib/day-offset'
import { routeTree } from '@/routes'

/**
 * 「今」を昼間の安定した瞬間に固定する（issue #274）。
 *
 * `windowOrigin()`（下）は now を **時刻境界に切り捨てる**（日境界ではない）。
 * 壁時計が現地 23 時台だと、切り捨てた起点 + 1 時間の番組（`soon`）が暦日を
 * またいでしまい、`DayStrip` の「いま見ている日」（スクロール位置からの導出。
 * `program-list.tsx` の `onVisibleDayChange`）が 1（明日）になる。すると
 * 「今日」のセル（`dayButtons[0]`）から `aria-current` が外れ、それを見ている
 * テストが壁時計依存で落ちる。
 *
 * `vi.setSystemTime` は**瞬間**を固定するだけで、そこから導かれる現地時刻は
 * プロセスの TZ 次第（`07:00 UTC` は `Asia/Tokyo` なら昼、`Etc/GMT+8` なら
 * ちょうど 23 時になる、というように offset 次第でどの瞬間を選んでも危険な
 * TZ が必ず存在する）。そのため TZ 自体をテストプロセス全体で
 * `Asia/Tokyo` に固定している（`vite.config.ts` の `test.env.TZ`）。ここでは
 * その固定された JST の上で危険域（23 時台）を避けた瞬間を選ぶだけでよい。
 *
 * `shouldAdvanceTime: true` は実時間の経過に追従してフェイクの時計も進める
 * ため、`setTimeout` に依存する `userEvent` の内部待ちや TanStack Query の
 * 解決を止めない（素の `vi.useFakeTimers()` はこのファイルの他のテストを
 * 壊すことを確認済み。CLAUDE.md のテスト規律どおり実際に壊して確かめた）。
 *
 * 深夜（23 時台）の実挙動そのものは別途固定のテストで検証する
 * （「ProgramsPage の日付ハイライト（深夜、起点 + 1 時間の暦日またぎ）」参照）。
 */
vi.useFakeTimers({ shouldAdvanceTime: true })
const pinnedNow = new Date('2026-08-14T12:00:00+09:00')
vi.setSystemTime(pinnedNow)

/**
 * ページは「今」を時刻境界に切り捨てた時刻を時間窓の起点にする。テストの番組も
 * 同じ起点から組み立てて、リスト（6 時間窓）とグリッド（24 時間窓）の違いが
 * 見えるように配置する。
 */
function windowOrigin(): number {
  const origin = new Date()
  origin.setMinutes(0, 0, 0)
  return origin.getTime()
}

const origin = windowOrigin()

const services: Service[] = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3273701032,
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
    hasPrograms: true,
  },
]

/**
 * サブサービス（マルチ編成の無い時間帯は番組を持たない）。`hasPrograms: false`
 * なので `filterableServices`（ピッカーの候補の生成元）には入らないが、
 * `GET /api/sites/default/services` には実在する（issue #231 のレビュー
 * must-fix: ピッカーの定義域テスト用）。
 */
const subService: Service = {
  id: 3273601040,
  networkId: 32736,
  serviceId: 1040,
  name: 'NHK総合サブ',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: false,
}

function program(
  programId: number,
  serviceId: number,
  startOffsetHours: number,
  name: string,
  networkId = 32736,
): ProgramListItem {
  const startAt = origin + startOffsetHours * 3_600_000
  return {
    programId,
    networkId,
    serviceId,
    eventId: programId,
    startAt: new Date(startAt).toISOString(),
    endAt: new Date(startAt + 3_600_000).toISOString(),
    durationMs: 3_600_000,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

/** 1 時間後の番組。リストの最初の窓（6 時間）にもグリッドの窓にも入る。 */
const soon = program(1, 1024, 1, 'ニュース7')
/** 同時刻・別サービスの番組。グリッドでは横に並ぶ。 */
const alsoSoon = program(2, 1032, 1, '手話ニュース', 32737)
/** 8 時間後の番組。リストの最初の窓には入らず、グリッド（24 時間）には入る。 */
const later = program(3, 1024, 8, '深夜ドラマ')

const allPrograms = [soon, alsoSoon, later]

/** programAtAbsolute は絶対時刻を起点にした 1 時間番組を作る（日付ジャンプのテスト用）。 */
function programAtAbsolute(
  programId: number,
  serviceId: number,
  startAtMs: number,
  name: string,
): ProgramListItem {
  return {
    programId,
    networkId: 32736,
    serviceId,
    eventId: programId,
    startAt: new Date(startAtMs).toISOString(),
    endAt: new Date(startAtMs + 3_600_000).toISOString(),
    durationMs: 3_600_000,
    name,
    description: '',
    genres: [0],
    isFree: true,
  }
}

function reservation(id: number, programId: number, title: string, site = 'default'): Reservation {
  return {
    id,
    site,
    programId,
    source: 'manual',
    state: 'active',
    title,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: new Date(origin + 3_600_000).toISOString(),
    durationMs: 3_600_000,
    createdAt: new Date(origin).toISOString(),
    updatedAt: new Date(origin).toISOString(),
    skip: false,
  }
}

/** overage は origin からの時間で超過区間を作る。 */
function overage(
  startOffsetHours: number,
  endOffsetHours: number,
  options: Partial<CapacityOverage> = {},
): CapacityOverage {
  return {
    site: 'default',
    startAt: new Date(origin + startOffsetHours * 3_600_000).toISOString(),
    endAt: new Date(origin + endOffsetHours * 3_600_000).toISOString(),
    shortfall: 1,
    jammedTypes: ['BS'],
    ...options,
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function noContentResponse(): Response {
  return new Response(null, { status: 204 })
}

function errorResponse(status: number, message: string): Response {
  return new Response(JSON.stringify({ error: message }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * encodeProfiles は「番組表からの予約でエンコード設定を指定する」テスト
 * （issue #132）が使うプロファイル一覧のスタブ。`GET /api/encode-profiles` は
 * `EncodeSettingsFields` が予約前の番組行を展開すると必ず叩くため、
 * それを含む全テストで解決できる必要がある。
 */
const encodeProfiles = [{ name: 'h264', container: 'mp4' as const }]

/**
 * stubApi は番組・サービス・予約・容量超過を URL で振り分ける。
 *
 * `/api/sites/default/programs` は時間窓と serviceId で実際に絞る。絞り込みが
 * サーバー側に移ったので、`serviceId` を無視するスタブにすると「選ぶと他局の
 * 番組が消える」という実装の主張をテストが検証できない（クライアント側には
 * もう適用点が無い）。`serviceId` は複数指定可（`getAll`）で、未指定なら
 * 絞り込みなし。
 *
 * 予約 / 取消は `PUT /api/sites/default/programs/{id}/intent` を叩く
 * （issue #29。reservations 行は同期的に作らない）。テストは常に成功させる。
 *
 * `programs` は絞り込み対象の番組集合（既定は `allPrograms`）。日付ジャンプの
 * テストは絶対時刻で配置した専用の番組を渡す。`onProgramsCall` は
 * `/api/sites/default/programs` への何回目の呼び出しかを受け取り、Response を
 * 返せばそれで応答を差し替える（続きの読み込み失敗のテスト用）。
 *
 * `overridesPatchResponse` は `PATCH /api/sites/default/programs/{id}/overrides`
 * の応答を差し替える（issue #132 の「PATCH が失敗しても予約は成立する」テスト用）。
 * 未指定なら常に 204。
 *
 * `extraServices` は `services`（固定 2 局）に追加で載せるサービス（既定は
 * 追加無し）。ピッカーの定義域テスト（issue #231 のレビュー must-fix）だけが
 * `hasPrograms: false` の局を注入するために使う。
 */
function stubApi(
  reservations: Reservation[] = [],
  overages: CapacityOverage[] = [],
  programs: ProgramListItem[] = allPrograms,
  onProgramsCall?: (callIndex: number) => Response | undefined,
  overridesPatchResponse?: () => Response,
  extraServices: Service[] = [],
) {
  let programsCallIndex = 0
  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    // SiteGate（routes.tsx）が全ルートの手前で GET /api/sites を待つ
    // （issue #184 M4-12）。ページを本物の routeTree（`RouterProvider`）越しに
    // 描く（`useSearch`/`useNavigate` を使うため）ようになったので必要になった
    // （`routes.test.tsx` の '/search' テストと同じ理由）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(jsonResponse([...services, ...extraServices]))
    }
    // AppShell がナビゲーションの出し分けに読む（issue #209）。未 stub でも
    // react-query が飲み込んで緑になる（`useLiveEnabled` が fail-closed で false
    // に倒れる）が、明示的に返しておく
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(jsonResponse({ live: true }))
    }
    // AppShell が全ページの手前でサーキットブレーカーの有無を読む
    // （`pages/recordings.test.tsx` の fetchMock と同じ stub）
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/capacity/overages') {
      return Promise.resolve(jsonResponse(overages))
    }
    if (url.pathname === '/api/encode-profiles') {
      return Promise.resolve(jsonResponse(encodeProfiles))
    }
    if (/^\/api\/sites\/default\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }
    // 行を展開すると ProgramDetail（program-row.tsx）が単体取得を叩く
    // （段階的開示。issue #132 のテストで行を展開するので必要になった）。
    const singleProgramMatch = /^\/api\/sites\/default\/programs\/(\d+)$/.exec(url.pathname)
    if (singleProgramMatch) {
      const found = programs.find((p) => p.programId === Number(singleProgramMatch[1]))
      return Promise.resolve(
        found ? jsonResponse(found) : new Response(null, { status: 404 }),
      )
    }
    if (/^\/api\/sites\/default\/programs\/\d+\/intent$/.test(url.pathname) && init?.method === 'PUT') {
      return Promise.resolve(noContentResponse())
    }
    if (
      /^\/api\/sites\/default\/programs\/\d+\/overrides$/.test(url.pathname) &&
      init?.method === 'PATCH'
    ) {
      return Promise.resolve(overridesPatchResponse?.() ?? noContentResponse())
    }
    if (url.pathname === '/api/sites/default/programs') {
      programsCallIndex++
      const override = onProgramsCall?.(programsCallIndex)
      if (override) return Promise.resolve(override)
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const serviceIds = url.searchParams.getAll('service').map(Number)
      const matched = programs.filter(
        (p) =>
          new Date(p.endAt).getTime() > start &&
          new Date(p.startAt).getTime() < end &&
          (serviceIds.length === 0 ||
            serviceIds.includes(p.networkId * 100_000 + p.serviceId)),
      )
      return Promise.resolve(jsonResponse(matched))
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

/**
 * stubMatchMedia は画面幅の判定を差し込む。jsdom は `window.matchMedia` を
 * 実装していないため、既定（未定義）ではグリッドが出ない側に倒れる。
 *
 * 返り値の `set` で幅の変化（ウィンドウのリサイズ）を再現できる。番組表を選んだ
 * あとに幅が狭くなる経路は UI からは作れないが、実際には起きる。
 */
function stubMatchMedia(initial: boolean) {
  let matches = initial
  const listeners = new Set<() => void>()
  window.matchMedia = vi.fn((query: string) => ({
    get matches() {
      return matches
    },
    media: query,
    onchange: null,
    addEventListener: (_type: string, listener: () => void) => void listeners.add(listener),
    removeEventListener: (_type: string, listener: () => void) => void listeners.delete(listener),
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia

  return {
    set(next: boolean) {
      matches = next
      act(() => {
        for (const listener of listeners) listener()
      })
    },
  }
}

afterEach(() => {
  Reflect.deleteProperty(window, 'matchMedia')
})

/**
 * renderPage は本物の routeTree（`@/routes`）で `ProgramsPage`（`/programs`。
 * ホーム新設（M8-3）前は `/` だった）を描く。
 *
 * `useSearch`/`useNavigate` を使うため（issue #231。チャンネル絞り込みの
 * URL 化）、最小限のアドホックなルート木ではなく実際の `/programs` ルート定義
 * （`validateSearch` を含む）を使う必要がある --- `pages/recordings.test.tsx`
 * の `renderPage` と同じ理由・同じ形。`SiteContext` を直接注入する旧方式は
 * `SiteGate`（`GET /api/sites` を待つ）を経由しないためこの構成では使えない。
 */
function renderPage(path = '/programs') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { ...view, queryClient, router }
}

/**
 * overagesSettled は容量超過のクエリが成功し終わるまで待つ。
 *
 * 「帯が出ない」ことの確認はこれを通してから行う（クエリが始まる前の
 * 「まだ何も無い」状態を見て通ってしまうのを防ぐ）。
 */
async function overagesSettled(queryClient: QueryClient): Promise<void> {
  await waitFor(() => {
    const queries = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['/api/capacity/overages'] })
    expect(queries).not.toHaveLength(0)
    expect(queries.map((q) => q.state.status)).toEqual(queries.map(() => 'success'))
  })
}

/**
 * reservationsSettled は予約のクエリが成功し終わるまで待つ。
 *
 * 「予約済みにならない」ことの確認はこれを通してから行う。予約は番組とは別クエリ
 * （`useListReservations`）で取り、結合はクエリ解決後なので、待たずに不在を見ると
 * 予約が届く前の「まだ未予約」状態を見て通ってしまう（クエリの解決順に依存する）。
 */
async function reservationsSettled(queryClient: QueryClient): Promise<void> {
  await waitFor(() => {
    const queries = queryClient.getQueryCache().findAll({ queryKey: ['/api/reservations'] })
    expect(queries).not.toHaveLength(0)
    expect(queries.map((q) => q.state.status)).toEqual(queries.map(() => 'success'))
  })
}

describe('ProgramsPage の表示形式', () => {
  it('lg 未満ではグリッドを出さず、切り替えも見せない', async () => {
    stubApi()
    stubMatchMedia(false)
    renderPage()

    // リストが出揃うまで待つ。待たずに不在を確認すると、クエリが解決する前の
    // 「まだ何も無い」状態を見て通ってしまう
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()
    expect(screen.queryByRole('group', { name: '表示形式' })).not.toBeInTheDocument()
  })

  it('lg 以上では切り替えが出て、番組表を選ぶとグリッドになる', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByRole('group', { name: '表示形式' })).toBeInTheDocument()
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))

    expect(await screen.findByTestId('program-grid')).toBeInTheDocument()
    // リスト側の「予約」ボタン（行右端）は消える
    expect(screen.queryByRole('button', { name: 'さらに読み込む' })).not.toBeInTheDocument()
  })

  it('番組表を選んだあとに画面が狭くなるとリストへ戻る', async () => {
    stubApi()
    const media = stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    expect(await screen.findByTestId('program-grid')).toBeInTheDocument()

    // モバイル幅ではグリッドを出さない（docs/frontend.md の決定）。
    // 画面幅で view を捨てないので、広げれば番組表に戻る
    media.set(false)
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()
    expect(await screen.findByRole('button', { name: 'さらに読み込む' })).toBeInTheDocument()
    expect(screen.queryByRole('group', { name: '表示形式' })).not.toBeInTheDocument()

    media.set(true)
    expect(await screen.findByTestId('program-grid')).toBeInTheDocument()
  })

  it('グリッドは 24 時間ぶんを 1 回で取る（リストの窓では見えない番組が出る）', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    // リストの最初の窓は 6 時間なので 8 時間後の番組は出ない
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.queryByText('深夜ドラマ')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))

    await screen.findByTestId('program-grid')
    expect(screen.getByText('深夜ドラマ')).toBeInTheDocument()
  })

  it('日付・サービスの選択は表示形式を切り替えても保持される', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    // サービスの選択はポップオーバーの中。開いてから選ぶ
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHKEテレ'))
    // 項目を押しただけでは閉じない。Esc で閉じる
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // トリガーに現在値が出ていることで選択を確認する（閉じた状態でも現在値が
    // 読めることそのものが単一選択のときからの主目的）
    expect(screen.getByRole('button', { name: 'チャンネル: NHKEテレ' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    // トリガーの選択が残っている
    expect(screen.getByRole('button', { name: 'チャンネル: NHKEテレ' })).toBeInTheDocument()
    // 選択はサーバーへの問い合わせ（`serviceId`）を経てグリッドの列にも効く。
    // 実際に `serviceId` が付くことの確認は別テスト
    // 「チャンネルを選ぶと API に serviceId が付く」の役目
    await waitFor(() => {
      const columns = screen.getAllByTestId('program-grid-column')
      expect(columns).toHaveLength(1)
      expect(columns[0]).toHaveAttribute('data-service-id', '1032')
    })
    // 絞り込んだサービスの番組だけが出る
    expect(screen.getByText('手話ニュース')).toBeInTheDocument()
    expect(screen.queryByText('ニュース7')).not.toBeInTheDocument()
  })

  it('予約状態がリストとグリッドで一致する', async () => {
    stubApi([reservation(77, soon.programId, 'ニュース7')])
    stubMatchMedia(true)
    renderPage()

    // リストでは行右端が「取消」になる
    expect(await screen.findByRole('button', { name: '取消' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    const reserved = document.querySelector(`[data-program-id="${soon.programId}"]`)
    const notReserved = document.querySelector(`[data-program-id="${later.programId}"]`)
    expect(reserved).toHaveAttribute('data-reserved', 'true')
    expect(notReserved).not.toHaveAttribute('data-reserved')
  })

  it('別サイトにしか予約が無い番組は「予約済み」にならない（issue #324）', async () => {
    // 現在サイト（default）には予約が無く、同じ programId の予約は別サイト
    // （other）にだけある。programId は放送イベントから決まるので 2 サイトで
    // 一致するが、別サイトの予約を現在サイトの番組表に重ねてはいけない。
    stubApi([reservation(77, soon.programId, 'ニュース7', 'other')])
    stubMatchMedia(false)
    const { queryClient } = renderPage()
    await reservationsSettled(queryClient)

    // 現在サイトの予約は無いので「予約」ボタンのまま（「取消」にならない）
    expect((await screen.findAllByRole('button', { name: '予約' })).length).toBeGreaterThan(0)
    expect(screen.queryByRole('button', { name: '取消' })).not.toBeInTheDocument()
  })

  it('グリッドのセルを押すと、リストと同じ行で予約できる', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    // グリッドには予約ボタンが無い（セルの高さは放送時間なので操作は置かない）
    expect(screen.queryByRole('button', { name: '予約' })).not.toBeInTheDocument()

    const cell = document.querySelector(`[data-program-id="${soon.programId}"]`)
    await userEvent.click(cell as HTMLElement)

    // 選択した番組がリストの行として現れ、そこから予約できる
    expect(await screen.findByRole('button', { name: '予約' })).toBeInTheDocument()
  })
})

describe('ProgramsPage の容量超過の帯', () => {
  /** showGrid にしてグリッドが出るまで待つ。 */
  async function openGrid() {
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')
  }

  it('超過区間を同時刻の番組セルと同じ位置に描く', async () => {
    // 1 時間後から 2 時間後（同じ時間帯に soon / alsoSoon がある）
    stubApi([], [overage(1, 2)])
    stubMatchMedia(true)
    renderPage()
    await openGrid()

    const band = await screen.findByTestId('capacity-band')
    const cell = document.querySelector<HTMLElement>(`[data-program-id="${soon.programId}"]`)
    // 帯とセルが同じ spanToPx を通っていることの唯一の観測可能な帰結
    expect(band.style.top).toBe(cell?.style.top)
    expect(band.style.height).toBe(cell?.style.height)
    // 不足本数と詰まった種別まで出す
    expect(screen.getByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
  })

  it('別サイトの超過区間は帯として描かない（issue #324）', async () => {
    // 同じ時間帯の超過だが site が現在サイト（default）でない。判定はサイトごとに
    // 独立している（docs/data.md §6.5）ので、別サイトのチューナー不足を現在サイトの
    // 番組表に重ねてはいけない。
    stubApi([], [overage(1, 2, { site: 'other' })])
    stubMatchMedia(true)
    const { queryClient } = renderPage()
    await openGrid()
    await overagesSettled(queryClient)

    expect(screen.queryByTestId('capacity-band')).not.toBeInTheDocument()
  })

  it('超過区間が無ければ帯を出さない（沈黙を肯定にしない）', async () => {
    stubApi([], [])
    stubMatchMedia(true)
    const { queryClient } = renderPage()
    await openGrid()
    await overagesSettled(queryClient)

    expect(screen.queryByTestId('capacity-band')).not.toBeInTheDocument()
    // 「収まります」「競合なし」に相当する肯定的な表示は出さない
    expect(screen.queryByText(/チューナー/)).toBeNull()
  })

  it('グリッドの窓と同じ範囲を問い合わせる', async () => {
    const fetchMock = stubApi([], [overage(1, 2)])
    stubMatchMedia(true)
    const { queryClient } = renderPage()
    await openGrid()
    await overagesSettled(queryClient)

    const asked = fetchMock.mock.calls
      .map((call) => new URL(String(call[0]), 'http://localhost'))
      .filter((url) => url.pathname === '/api/capacity/overages')
    // 軸の外の区間を持っていても帯にできないので、窓は軸と一致させる
    expect(asked.at(-1)?.searchParams.get('start')).toBe(new Date(origin).toISOString())
    expect(asked.at(-1)?.searchParams.get('end')).toBe(
      new Date(origin + 24 * 3_600_000).toISOString(),
    )
  })

  it('予約すると超過を取り直す（帯は予約集合からの導出値）', async () => {
    const fetchMock = stubApi([], [])
    stubMatchMedia(true)
    const { queryClient } = renderPage()
    await openGrid()
    await overagesSettled(queryClient)

    const before = fetchMock.mock.calls.filter(
      (call) => new URL(String(call[0]), 'http://localhost').pathname === '/api/capacity/overages',
    ).length

    const cell = document.querySelector(`[data-program-id="${soon.programId}"]`)
    await userEvent.click(cell as HTMLElement)
    await userEvent.click(await screen.findByRole('button', { name: '予約' }))

    // 取り直さないと「予約したのに不足が出ない / 消えない」状態が残る
    await waitFor(() => {
      const after = fetchMock.mock.calls.filter(
        (call) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/capacity/overages',
      ).length
      expect(after).toBeGreaterThan(before)
    })
  })

  it('リストのままなら超過を問い合わせない（帯を描く場所がない）', async () => {
    const fetchMock = stubApi([], [overage(1, 2)])
    stubMatchMedia(false)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(0))

    const asked = fetchMock.mock.calls
      .map((call) => new URL(String(call[0]), 'http://localhost'))
      .filter((url) => url.pathname === '/api/capacity/overages')
    expect(asked).toHaveLength(0)
  })
})

describe('ProgramsPage のリスト（回帰）', () => {
  it('日付の選択、チャンネル絞り込みのトリガー、続きの読み込みが出る', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    // 「今」という別枠の選択肢は無い。今日のセルが aria-current="date" で
    // ハイライトされる（ジャンプ先・可視範囲ともに初期値は今日 = offset 0）
    const dayGroup = screen.getByRole('group', { name: '日付' })
    const dayButtons = within(dayGroup).getAllByRole('button')
    expect(dayButtons).toHaveLength(8)
    expect(dayButtons.filter((b) => b.getAttribute('aria-current') === 'date')).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'さらに読み込む' })).toBeInTheDocument()
  })

  it('続きを読み込むと次の窓の番組が増える', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.queryByText('深夜ドラマ')).not.toBeInTheDocument()

    // 6 時間窓を 2 回継ぎ足すと 8 時間後の番組に届く
    await userEvent.click(screen.getByRole('button', { name: 'さらに読み込む' }))
    await userEvent.click(await screen.findByRole('button', { name: 'さらに読み込む' }))

    expect(await screen.findByText('深夜ドラマ')).toBeInTheDocument()
  })

  it('日付の選択肢は横スクロールする容器に入っていない', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    const group = screen.getByRole('group', { name: '日付' })
    expect(group.className).not.toMatch(/overflow-x-auto/)
  })

  it('チャンネルを選ぶとトリガーにその名前が出る（項目を押しただけでは閉じない）', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    // 項目を押しただけではポップオーバーは閉じない（複数選ぶため）。
    // 開いたことを確かめたうえで、まだ開いていることを見る。
    expect(screen.getByRole('dialog', { name: 'チャンネル' })).toBeInTheDocument()
    // トリガーの表示は開いたままでも即座に更新される
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()

    // 閉じるのは外側クリック / Esc。閉じた状態でも現在値が読める
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
  })

  it('チャンネル絞り込みのトリガーはリスト表示でもグリッド表示でも存在する', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
  })
})

describe('ProgramsPage のチャンネル複数選択', () => {
  it('1 局選ぶと他局の番組が消え、もう 1 局足すと両方の番組が出る', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('手話ニュース')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    // NHK総合 だけに絞ったので、NHKEテレ の番組（手話ニュース）は消える
    expect(await screen.findByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.queryByText('手話ニュース')).not.toBeInTheDocument()
    expect(screen.getByText('ニュース7')).toBeInTheDocument()

    // ポップオーバーはまだ開いている（閉じていたら以下の click は要素が
    // 見つからず失敗する）。閉じずにもう 1 局足す
    expect(screen.getByRole('dialog', { name: 'チャンネル' })).toBeInTheDocument()
    await userEvent.click(within(dialog).getByText('NHKEテレ'))

    // 2 局とも選んだので、両方の番組が出る
    expect(
      await screen.findByRole('button', { name: 'チャンネル: 2 局を選択中' }),
    ).toBeInTheDocument()
    expect(screen.getByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('手話ニュース')).toBeInTheDocument()
  })

  it('1 局を選んだ状態でもピッカーの候補が減らない（filterableServices の回帰）', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    // NHK総合 に絞ると NHKEテレ の番組は一覧から消える
    expect(await screen.findByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.queryByText('手話ニュース')).not.toBeInTheDocument()

    // にもかかわらず、絞り込み候補には NHKEテレ が残ったまま出ている。
    // 候補は `hasPrograms`（EPG プロジェクション全体で 1 件でも番組を持つか）
    // から導いており、表示中の（サーバー側で絞り込んだ後の）番組から候補を
    // 導くと、1 局に絞った瞬間に候補がその 1 局だけになり、他局へ直接
    // 切り替えられなくなる。この設計では候補が絞り込みから独立しているので
    // 構造的に守られるが、回帰しないことを確認する意味でテストは残す
    expect(within(dialog).getByText('NHKEテレ')).toBeInTheDocument()
  })

  it('「すべて」を押すと選択が空に戻り、全局の番組が出る', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    expect(await screen.findByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.queryByText('手話ニュース')).not.toBeInTheDocument()

    await userEvent.click(within(dialog).getByText('すべて'))

    expect(await screen.findByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
    expect(screen.getByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('手話ニュース')).toBeInTheDocument()
  })

  it('チャンネルを選ぶと API に service（合成 id）が付く', async () => {
    const fetchMock = stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()

    const callsBeforeSelection = fetchMock.mock.calls.length

    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    expect(await screen.findByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByText('手話ニュース')).not.toBeInTheDocument())

    // グリッドのクエリは選択済みの状態で初めて有効になる。選択を変えてから
    // グリッドへ切り替え、リスト・グリッドとも厳密な service だけを送ることを見る。
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    // 選択より前のリクエスト（初期表示時の絞り込みなし取得）を含めると
    // `.every` が必ず失敗するので、選択後のリクエストだけを見る
    const requestsAfterSelection = fetchMock.mock.calls
      .slice(callsBeforeSelection)
      .map((call) => new URL(String(call[0]), 'http://localhost'))
      .filter((url) => url.pathname === '/api/sites/default/programs')
    expect(requestsAfterSelection.length).toBeGreaterThan(0)
    expect(
      requestsAfterSelection.every(
        (url) =>
          url.searchParams.getAll('service').includes('3273601024'),
      ),
    ).toBe(true)
  })

  it('別 network の同じ serviceId は別列・別名で描く', async () => {
    const bs: Service = {
      ...services[0],
      id: 400101,
      networkId: 4,
      serviceId: 101,
      name: 'BS 101',
      channelType: 'BS',
    }
    const cs: Service = {
      ...services[0],
      id: 600101,
      networkId: 6,
      serviceId: 101,
      name: 'CS 101',
      channelType: 'CS',
    }
    stubApi(
      [],
      [],
      [program(101, 101, 1, 'BS の番組', 4), program(102, 101, 1, 'CS の番組', 6)],
      undefined,
      undefined,
      [bs, cs],
    )
    stubMatchMedia(true)
    renderPage('/programs?service=400101&service=600101')

    expect(await screen.findByRole('button', { name: 'チャンネル: 2 局を選択中' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    expect(screen.getByText('BS 101')).toBeInTheDocument()
    expect(screen.getByText('CS 101')).toBeInTheDocument()
    expect(screen.getAllByText('BS の番組')).toHaveLength(1)
    expect(screen.getAllByText('CS の番組')).toHaveLength(1)
    expect(screen.getAllByTestId('program-grid-column')).toHaveLength(2)
  })

  it('グリッド表示で、選んだ局だけが列になる', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    // 絞り込み前は両方の局が列になる
    expect(
      screen.getAllByTestId('program-grid-column').map((el) => el.getAttribute('data-service-id')),
    ).toEqual(expect.arrayContaining(['1024', '1032']))

    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    await waitFor(() => {
      const columns = screen.getAllByTestId('program-grid-column')
      expect(columns).toHaveLength(1)
      expect(columns[0]).toHaveAttribute('data-service-id', '1024')
    })
  })
})

/**
 * チャンネル絞り込みの URL 化（issue #231）。「サーバー側で絞り込む」ことの
 * 確認は上の「ProgramsPage のチャンネル複数選択」が既に持っているので、
 * ここでは URL との往復（深いリンクで開く / 選択が URL に反映される）だけを見る。
 */
describe('ProgramsPage のチャンネル絞り込みの URL 化（issue #231）', () => {
  it('?service= 付きの URL で開くと、絞り込み済みの番組表がピッカーの表示と一致して開く', async () => {
    stubApi()
    renderPage('/programs?service=3273601024')

    // NHK総合（1024）に絞り込んだ状態で開く。ピッカーのラベルは URL の解決だけで
    // 決まる（データ取得を待たない）ので、先に番組の取得完了（データ取得の完了を
    // 待ってから見る --- CLAUDE.md「非同期の空虚な成功に注意する」）を待ってから
    // 両方を見る
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.queryByText('手話ニュース')).not.toBeInTheDocument()
  })

  it('チャンネルを選ぶと URL の厳密な ?service= に反映される（history を汚さず replace）', async () => {
    stubApi()
    const { router } = renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(router.state.location.search.service).toBeUndefined()

    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: すべて' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合'))

    await waitFor(() => {
      expect(router.state.location.search.service).toEqual([3273601024])
    })

    // history を汚さない（replace）。積んだままだと「戻る」で絞り込み変更が
    // 1 手ずつ再生されてしまう
    expect(router.history.length).toBe(1)
  })

  it('選択済みの URL からピッカーを操作すると組を足す', async () => {
    stubApi()
    const { router } = renderPage('/programs?service=3273601024')

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: NHK総合' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHKEテレ'))

    await waitFor(() => {
      expect(router.state.location.search.service).toEqual([3273601024, 3273701032])
    })
  })

  it('不正な値（?service=abc）は絞り込みなしに落ちて開ける（壊れたリンクを踏んでも画面は開く）', async () => {
    stubApi()
    renderPage('/programs?service=abc')

    expect(await screen.findByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
    // 絞り込みなしなので両局の番組が出る（データ取得の完了を待ってから見る ---
    // CLAUDE.md「非同期の空虚な成功に注意する」）
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByText('手話ニュース')).toBeInTheDocument()
  })
})

/**
 * ピッカーの定義域（issue #231 のレビュー must-fix）。
 *
 * 絞り込みが URL 化された時点で、選択（`selectedServiceIds`）は「外から入る値」
 * になる。この PR 以前は選択の唯一の生成元がピッカー自身だったため
 * `selected ⊆ filterableServices` が構造的に成り立っていたが、URL 化すると
 * この前提が消える（閉世界 → 開世界）。`filterableServices`（`hasPrograms`
 * から作る候補の生成元）に無い serviceId への深いリンクで、ピッカーが
 * 「0 件選択（＝すべて）」に見えてはならない --- 「絞り込みで全部隠れている」
 * ことと「絞り込みなしで番組が無い」ことは区別できる必要がある。
 */
describe('ProgramsPage のピッカーの定義域（issue #231 のレビュー must-fix）', () => {
  it('filterableServices に無い局（hasPrograms: false）への深いリンクでも「すべて」に見えず、個別に解除できる', async () => {
    stubApi([], [], allPrograms, undefined, undefined, [subService])
    renderPage('/programs?service=3273601040')

    // サブサービスは番組を持たないので一覧は空になる。だが「絞り込みで全部
    // 隠れている」ことがトリガーから読める必要がある
    expect(await screen.findByText('この時間帯の番組がありません')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合サブ' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'チャンネル: すべて' })).not.toBeInTheDocument()

    // 候補にも出ていて、個別に解除できる（「すべて」で全解除する以外の手段がある）
    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: NHK総合サブ' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('NHK総合サブ'))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument(),
    )
  })

  it('services にも実在しない id への深いリンク（削除された局・壊れた共有リンク）は「チャンネル #<id>」で示され、個別に解除できる', async () => {
    stubApi()
    renderPage('/programs?service=3273609999')

    expect(await screen.findByText('この時間帯の番組がありません')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'チャンネル: チャンネル #3273609999' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'チャンネル: すべて' })).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'チャンネル: チャンネル #3273609999' }))
    const dialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await userEvent.click(within(dialog).getByText('チャンネル #3273609999'))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument(),
    )
  })
})

describe('ProgramsPage の進行方向の無限スクロール', () => {
  it('計測できない環境（jsdom）では IntersectionObserver を作らず、ボタンだけを受け皿にする', async () => {
    stubApi()
    // jsdom には IntersectionObserver が無い。もし実装が `domLayoutMeasurable()`
    // のガードを外して常に IntersectionObserver を作るようになったら、
    // ここで検知する（このテストを「壊す」には該当ガードを消せばよい）。
    const observerCtor = vi.fn(() => ({
      observe: vi.fn(),
      unobserve: vi.fn(),
      disconnect: vi.fn(),
    }))
    const original = (globalThis as { IntersectionObserver?: unknown }).IntersectionObserver
    ;(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = observerCtor

    try {
      renderPage()
      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      // 自動が使えない環境なので、ボタンが最初から受け皿として出ている
      expect(screen.getByRole('button', { name: 'さらに読み込む' })).toBeInTheDocument()
      expect(observerCtor).not.toHaveBeenCalled()
    } finally {
      if (original === undefined) {
        Reflect.deleteProperty(globalThis, 'IntersectionObserver')
      } else {
        ;(globalThis as unknown as { IntersectionObserver: unknown }).IntersectionObserver = original
      }
    }
  })

  it('続きの読み込みが失敗すると、エラー表示のままボタンが残り、自動では再試行しない', async () => {
    const fetchMock = stubApi([], [], allPrograms, (callIndex) =>
      callIndex === 2 ? new Response(null, { status: 500 }) : undefined,
    )
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'さらに読み込む' }))

    expect(await screen.findByText('続きの取得に失敗しました')).toBeInTheDocument()
    // 失敗してもボタンは消えない（手動での再試行の受け皿）
    expect(screen.getByRole('button', { name: 'さらに読み込む' })).toBeInTheDocument()

    const programsCallsAfterFailure = () =>
      fetchMock.mock.calls.filter(
        (call) => new URL(String(call[0]), 'http://localhost').pathname === '/api/sites/default/programs',
      ).length
    const callsRightAfterFailure = programsCallsAfterFailure()

    // 何もしなくても自動で再試行が走らないこと（QueryClient は retry: false
    // だが、ここでは「無限にリクエストを投げ続けない」という実装側の約束を
    // 確認したい）
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect(programsCallsAfterFailure()).toBe(callsRightAfterFailure)

    // 自動を止めただけで手動の再試行は生きている
    await userEvent.click(screen.getByRole('button', { name: 'さらに読み込む' }))
    await waitFor(() => expect(screen.queryByText('続きの取得に失敗しました')).not.toBeInTheDocument())
  })
})

describe('ProgramsPage の日付ジャンプ（先頭の窓に重なる前日の番組を出さない。3 回目の修正）', () => {
  it('ジャンプ先の窓と重なって返ってきた前日の番組をリストの先頭に出さず、ハイライトもジャンプ先の日のまま', async () => {
    // offset 1 ではなく 2（明後日）にする: `allPrograms` の固定オフセットとの
    // 窓の重なりを実行時刻によらず避けるため。
    const targetOriginMs = dayOrigin(2).getTime()
    // API は問い合わせた時間窓に重なる番組を返す。この番組は前日 23:30 開始・
    // ジャンプ先 0:30 終了で、ジャンプ直後の最初の窓（明後日 0 時〜6 時）にも
    // 重なって返ってくる --- 実機で踏んだ不具合そのものの構図。
    const overlapping = programAtAbsolute(
      210,
      1024,
      targetOriginMs - 1_800_000,
      '前日からの重なり番組',
    )
    // 区別のため、実際にジャンプ先の日に始まる番組も置く
    const insideWindow = programAtAbsolute(211, 1024, targetOriginMs + 1_800_000, '窓内の番組')
    stubApi([], [], [...allPrograms, overlapping, insideWindow])
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    const dayGroup = screen.getByRole('group', { name: '日付' })
    await userEvent.click(within(dayGroup).getAllByRole('button')[2]) // offset 2 = 明後日

    // 重なり番組は出ない。窓内の番組は出る
    expect(await screen.findByText('窓内の番組')).toBeInTheDocument()
    expect(screen.queryByText('前日からの重なり番組')).not.toBeInTheDocument()

    // ハイライトはジャンプ先の日のまま（重なり番組の日である前日にずれない）
    await waitFor(() =>
      expect(within(dayGroup).getAllByRole('button')[2]).toHaveAttribute('aria-current', 'date'),
    )
  })
})

/**
 * 容量不足バッジ（`components/capacity-shortfall-badge.tsx`）からの導線
 * （issue #233 M6-5）。バッジは番組表ルートに `?view=grid&at=<epoch ms>` を
 * 積んでリンクする（`view` の URL 化は issue #437。以前は `at` だけを積み、
 * `lg` 以上かどうかを `useMediaQuery` から推論してグリッドへ自動切替していたが、
 * `view` を URL に持つようになったのでバッジ自身が明示する）。
 *
 * グリッドの実際のスクロール位置（px）・グリッドが実際に何レンダー目でマウント
 * されるか（`useMediaQuery` は初回レンダーでは必ず false を返すので、`showGrid`
 * が true になるのは早くても 1 レンダー遅れる。`docs/frontend/programs.md`
 * 「番組表への `at` 導線」参照）は jsdom で測れないので e2e の担当（`web/e2e/`）。
 * ここで見るのは jsdom でも判定できる部分だけ --- (1) `lg` 未満では `view=grid`
 * があってもグリッドを出さず、「その時刻が属する日」への日付ジャンプに
 * フォールバックすること、(2) `lg` 以上では `view=grid` どおりグリッド表示に
 * なること、(3) `at` だけで `view=grid` が無ければ `lg` 以上でもグリッドに
 * ならないこと（推論をやめたことの回帰確認）、(4) グリッドからユーザーが
 * 手動でリストへ戻すと URL の `view` も `list` に更新され、画面幅の再評価
 * （resize）だけでは戻らないこと。
 */
describe('ProgramsPage の at パラメータ（容量バッジからの導線。issue #233 M6-5）', () => {
  // dayOffset 2（明後日）に属する時刻。他の日付ジャンプテストと同じ理由で
  // offset 1 ではなく 2 を使う（allPrograms の固定オフセットとの窓の重なりを
  // 避ける）。この時刻を覆う番組を明示的に置く --- 置かないと day 2 の窓が
  // 空になり、「グリッドが出る/出ない」ではなく「空状態が出る」で判定が
  // 潰れてしまう（グリッド・リストのどちらも `programs.length === 0` で同じ
  // EmptyState に落ちるため、`program-grid` の testid の有無まで見分けられない）。
  const targetMs = dayOrigin(2).getTime() + 3 * 3_600_000
  const dayTwoProgram = programAtAbsolute(220, 1024, targetMs, '容量バッジ導線の番組')

  it('lg 未満では、view=grid があってもグリッドを出さずに at が属する日へ日付ジャンプする', async () => {
    stubApi([], [], [...allPrograms, dayTwoProgram])
    stubMatchMedia(false)
    renderPage(`/programs?view=grid&at=${targetMs}`)

    // 「ニュース7」（今日の番組）ではなく、day 2 の番組が出る ---
    // 日付ジャンプが実際に効いていることの証拠（効いていなければ今日のまま
    // 「ニュース7」が出続け、この見つけ方は失敗する）
    expect(await screen.findByText('容量バッジ導線の番組')).toBeInTheDocument()
    expect(screen.queryByText('ニュース7')).not.toBeInTheDocument()
    // グリッドは lg 未満では出ない（表示形式の切り替えごと存在しない）
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()

    const dayGroup = screen.getByRole('group', { name: '日付' })
    expect(within(dayGroup).getAllByRole('button')[2]).toHaveAttribute('aria-current', 'date')
  })

  it('view=grid が URL にあれば、lg 以上ではグリッド表示になる', async () => {
    stubApi([], [], [...allPrograms, dayTwoProgram])
    stubMatchMedia(true)
    renderPage(`/programs?view=grid&at=${targetMs}`)

    // `at` の有無や画面幅からの推論を経由せず `view=grid` がそのままグリッドに
    // なる（何レンダー目でマウントされるかは jsdom では測れないので e2e の担当）。
    // グリッドの中に day 2 の番組が実際に見えることまで確認する ---
    // 単に testid が存在するだけでは「軸が day 2 に合っている」保証にならない
    // （軸がずれていても `programs.length` が非 0 なら testid 自体は出る）。
    await screen.findByTestId('program-grid')
    expect(screen.getByText('容量バッジ導線の番組')).toBeInTheDocument()

    const dayGroup = screen.getByRole('group', { name: '日付' })
    expect(within(dayGroup).getAllByRole('button')[2]).toHaveAttribute('aria-current', 'date')
  })

  it('at だけでは（view=grid が無ければ）lg 以上でもグリッドにならない', async () => {
    // 以前は `at` の存在と `useMediaQuery` から「グリッドで見せたい」を推論
    // していたが、`view` を URL に持つようになったので推論はしない
    // （issue #437）。`at` 単独ではリストのまま。
    stubApi([], [], [...allPrograms, dayTwoProgram])
    stubMatchMedia(true)
    renderPage(`/programs?at=${targetMs}`)

    expect(await screen.findByText('容量バッジ導線の番組')).toBeInTheDocument()
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()
  })

  it('グリッドからユーザーがリストへ戻すと URL の view も更新され、画面幅の再評価だけではグリッドに戻らない', async () => {
    stubApi([], [], [...allPrograms, dayTwoProgram])
    const media = stubMatchMedia(true)
    const { router } = renderPage(`/programs?view=grid&at=${targetMs}`)

    await screen.findByTestId('program-grid')

    // ユーザーが手動でリストへ戻す（URL の `view` が `list` に replace される）
    await userEvent.click(screen.getByRole('button', { name: 'リスト' }))
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()

    // トグルが実際に URL を書き換えていることを見る --- グリッドの不在だけでは
    // component state に戻すだけの実装（URL を一切書かない）でも通ってしまう
    // （実測。ローカル state 変異で 898/898 全通過した）。
    await waitFor(() => {
      expect(router.state.location.search.view).toBe('list')
    })
    // history を汚さない（replace）。ピッカーの絞り込み更新と同じ規律
    expect(router.history.length).toBe(1)

    // 画面幅が狭くなって（他の画面遷移や resize で）また広くなっても、
    // URL の `view` は `list` のままなのでグリッドへ戻されない --- 推論を挟まず
    // URL だけで表示形式が決まるようになったので、専用の ref はもう要らない
    media.set(false)
    media.set(true)
    expect(screen.queryByTestId('program-grid')).not.toBeInTheDocument()
  })

  // レビュー nit 4: 素朴に「at を 1 回使ったら URL から navigate で消す」実装を
  // 試したところ、`navigate` の非同期解決がグリッドの初回スクロール確定より
  // 先に終わってしまい、肝心のスクロールが「今」にしか効かなくなる退行を
  // e2e（`web/e2e/badge-links.mjs` の②）で検出した。代わりに `scrollToMs` を
  // `dayOffset === atDayOffset` で条件付ける方式にしたので、at は URL に残る
  // （消費・削除しない）。ここで確認できるのは「別の日を選べば実際にその日へ
  // 切り替わる」こと（= at が現在地を固定してしまわないこと）まで --- 「今日へ
  // 戻したときに now へスクロールし直す」というスクロール位置そのものの主張は
  // jsdom では測れないため e2e の担当。
  it('at がある状態でも別の日を選べば、その日の内容に切り替わる（at が現在地を固定しない）', async () => {
    stubApi([], [], [...allPrograms, dayTwoProgram])
    stubMatchMedia(true)
    renderPage(`/programs?view=grid&at=${targetMs}`)

    await screen.findByTestId('program-grid')
    expect(screen.getByText('容量バッジ導線の番組')).toBeInTheDocument()

    // 「今日」（offset 0）を選び直す
    const dayGroup = screen.getByRole('group', { name: '日付' })
    await userEvent.click(within(dayGroup).getAllByRole('button')[0])

    await waitFor(() =>
      expect(within(dayGroup).getAllByRole('button')[0]).toHaveAttribute('aria-current', 'date'),
    )
    // day 2 専用の番組はもう見えない（軸が実際に today へ切り替わった証拠）
    expect(screen.queryByText('容量バッジ導線の番組')).not.toBeInTheDocument()
  })

  // レビューの must-fix 1（実測: 実ブラウザ・jsdom の両方で `/?at=1e30` 等が
  // "Something went wrong!" になった）の回帰テスト。`parseAt`
  // （lib/programs-search.ts）が Date の time value の定義域外を落とすので、
  // ここまで来る `at` は既に安全なはずだが、実際にページ全体を描いて
  // エラー境界（TanStack Router の既定のエラーコンポーネント）に落ちて
  // いないことまで確認する --- 単体関数のテストだけでは「呼び出し側で
  // 本当に守られているか」までは分からない。
  it('Date の time value の定義域を超える at を踏んでもエラー境界に落ちない（壊れたリンクでも画面は開く）', async () => {
    stubApi()
    stubMatchMedia(true)
    renderPage('/programs?at=1e30')

    // 通常表示（「今日」のまま）に落ちる。エラー境界の文言が出ていないこと
    // まで見る（`document.body.textContent` に "Something went wrong" を
    // 含めば実測どおりの回帰）
    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(/Something went wrong/)

    const dayGroup = screen.getByRole('group', { name: '日付' })
    // at が落とされているので、日付ジャンプは起きず「今日」のまま
    expect(within(dayGroup).getAllByRole('button')[0]).toHaveAttribute('aria-current', 'date')
  })
})

/**
 * 深夜（JST 23 時台）に「今日」の起点 + 1 時間が暦日をまたぐことを実挙動
 * として固定する（issue #274）。
 *
 * `dayOrigin(0)`（`lib/day-offset.ts`）は「今日」の窓の起点を **now を時刻境界
 * （0 時ではない）に切り捨てた時刻**にする。現地 23 時台だとその起点 + 1 時間
 * （最初に見える番組の時刻）が暦日としては翌日になり、`ProgramList` が
 * スクロール位置から導く「いま見ている日」（`onVisibleDayChange`
 * → `programs.tsx` の `visibleDay` → `DayStrip` の `current`）が 1（明日）に
 * なる。時刻を 23:13 JST に固定して実測したところ、`aria-current="date"` は
 * 消えるのではなく **「今日」ではなく「翌日」のセルに付く**
 * （`dayButtons[0]` は null、`dayButtons[1]` が `"date"`）。これは狙った仕様
 * ではなく、`dayOrigin` が窓の連続性のために意図的に時刻境界を使うことの
 * 副作用として今後も起き続ける実挙動なので、偶然 23 時台に実行したときにだけ
 * 観測される状態から、恒久的な判定に変える。
 *
 * 「23 時台」はテストプロセスのローカルタイムゾーン（`vite.config.ts` の
 * `test.env.TZ` で `Asia/Tokyo` に固定済み）に対しての現地時刻なので、ここでは
 * `vi.setSystemTime` で瞬間を JST 23:13 に固定するだけでよい。
 */
describe('ProgramsPage の日付ハイライト（深夜、起点 + 1 時間の暦日またぎ）', () => {
  afterEach(() => {
    // 他のテストへ影響しないよう、ファイル全体で固定した昼間の瞬間へ戻す
    vi.setSystemTime(pinnedNow)
  })

  it('JST 23 時台では「いま見ている日」が翌日になり、今日のセルではなく翌日のセルに aria-current が付く', async () => {
    vi.setSystemTime(new Date('2026-08-14T23:13:00+09:00'))
    const nightOrigin = new Date()
    nightOrigin.setMinutes(0, 0, 0)
    // dayOrigin(0) と同じ切り捨て（時刻境界）で起点を作り、+1 時間の番組を置く
    // ---「最初の窓（6 時間）にも入る直近の番組」という allPrograms の soon と
    // 同じ役目。ここでは実行時刻に依存させず 23:13 JST に固定して置く。
    const nightSoon = programAtAbsolute(
      301,
      1024,
      nightOrigin.getTime() + 3_600_000,
      '深夜またぎの番組',
    )
    stubApi([], [], [nightSoon])
    renderPage()

    expect(await screen.findByText('深夜またぎの番組')).toBeInTheDocument()
    const dayGroup = screen.getByRole('group', { name: '日付' })
    const dayButtons = within(dayGroup).getAllByRole('button')

    // 「いま見ている日」の導出（スクロール位置ベース）が確定するまで待ってから見る
    await waitFor(() => expect(dayButtons[1]).toHaveAttribute('aria-current', 'date'))
    expect(dayButtons[0]).not.toHaveAttribute('aria-current')
  })
})

/**
 * 番組表から「予約」する際に encodeProfiles / keepOriginal を指定できることの
 * 回帰テスト（issue #132）。
 *
 * 予約詳細画面（reservation-detail.tsx）へ遷移しなくても、番組表の行を展開
 * するだけで overrides を編集できることと、その値が「予約」ボタン 1 回の
 * 操作で intent の PUT と overrides の PATCH の両方に正しく渡ることを見る。
 * ダイアログの開閉・見た目の可視性は jsdom では測れない領域なので、e2e 側
 * （`web/e2e/`）の担当（issue コメント参照）。ここで固定するのは
 * fetch に飛んだ引数までとする。
 *
 * 各テストは `stubApi([], [], [soon])` で番組を 1 件に絞る --- 複数番組が
 * 並ぶと行ごとに同じ accessible name（「予約」）のボタンが並び、どの行を
 * 押したか一意に決められなくなるため。
 */
describe('ProgramsPage から予約時にエンコード設定を指定できる（issue #132）', () => {
  it('行を展開すると、まだ予約されていない番組にもエンコード設定欄が出る（予約詳細画面へ遷移しない）', async () => {
    stubApi([], [], [soon])
    renderPage()

    const title = await screen.findByText('ニュース7')
    await userEvent.click(title)

    expect(await screen.findByText('エンコードプロファイル')).toBeInTheDocument()
    expect(await screen.findByRole('checkbox', { name: /h264/ })).toBeInTheDocument()
    // 「予約」を押すまで反映されない、という注意も出す（保存だけで録画される
    // という誤解を避ける。issue #132 の罠）
    expect(
      screen.getByText(/「予約」を押した時点で反映されます/),
    ).toBeInTheDocument()
  })

  it('展開しても既定のまま予約すると、overrides の PATCH は送らない（意味の無い override 行を作らない。CLAUDE.md 不変条件 10）', async () => {
    const fetchMock = stubApi([], [], [soon])
    renderPage()

    const title = await screen.findByText('ニュース7')
    await userEvent.click(title) // 展開するが設定は既定のまま変えない
    await screen.findByText('エンコードプロファイル')

    await userEvent.click(screen.getByRole('button', { name: '予約' }))

    // intent の PUT が飛ぶのを待ってから、overrides の PATCH が無いことを見る
    // （クエリが解決する前に判定すると空虚な成功になるため、成功を先に待つ）
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          (call) =>
            new URL(String(call[0]), 'http://localhost').pathname ===
              `/api/sites/default/programs/${soon.programId}/intent` &&
            (call[1] as RequestInit | undefined)?.method === 'PUT',
        ),
      ).toBe(true)
    })

    expect(
      fetchMock.mock.calls.some((call) =>
        new URL(String(call[0]), 'http://localhost').pathname.endsWith('/overrides'),
      ),
    ).toBe(false)
  })

  it('エンコード設定を変えてから予約すると、intent の PUT と overrides の PATCH が正しい引数で両方飛ぶ', async () => {
    const fetchMock = stubApi([], [], [soon])
    renderPage()

    const title = await screen.findByText('ニュース7')
    await userEvent.click(title)

    const checkbox = await screen.findByRole('checkbox', { name: /h264/ })
    await userEvent.click(checkbox)

    await userEvent.click(screen.getByRole('button', { name: '予約' }))

    const overridesUrl = `/api/sites/default/programs/${soon.programId}/overrides`
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          (call) => new URL(String(call[0]), 'http://localhost').pathname === overridesUrl,
        ),
      ).toBe(true)
    })

    const overridesCall = fetchMock.mock.calls.find(
      (call) => new URL(String(call[0]), 'http://localhost').pathname === overridesUrl,
    )
    const overridesInit = overridesCall?.[1] as RequestInit
    expect(overridesInit.method).toBe('PATCH')
    expect(JSON.parse(String(overridesInit.body))).toEqual({
      keepOriginal: 'always',
      encodeProfiles: ['h264'],
    })

    // 予約作成（PUT .../intent）自体は action のみのまま変更しない（issue #29
    // の決定を維持。overrides は別リクエストのまま）
    const intentCall = fetchMock.mock.calls.find(
      (call) =>
        new URL(String(call[0]), 'http://localhost').pathname ===
          `/api/sites/default/programs/${soon.programId}/intent` &&
        (call[1] as RequestInit | undefined)?.method === 'PUT',
    )
    expect(intentCall).toBeDefined()
    const intentInit = intentCall?.[1] as RequestInit
    expect(JSON.parse(String(intentInit.body))).toEqual({
      action: 'record',
    })
  })

  it('overrides の PATCH が失敗しても、予約（intent）自体は成立している', async () => {
    const fetchMock = stubApi([], [], [soon], undefined, () =>
      errorResponse(400, 'program not found in the EPG projection'),
    )
    renderPage()

    const title = await screen.findByText('ニュース7')
    await userEvent.click(title)

    const checkbox = await screen.findByRole('checkbox', { name: /h264/ })
    await userEvent.click(checkbox)
    await userEvent.click(screen.getByRole('button', { name: '予約' }))

    // 予約に失敗した、ではなくエンコード設定の保存にだけ失敗したと分けて示す
    expect(await screen.findByText(/エンコード設定の保存に失敗しました/)).toBeInTheDocument()
    expect(screen.queryByText('予約に失敗しました')).not.toBeInTheDocument()

    // それでも intent の PUT は届いている（予約自体は成立する）
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.some(
          (call) =>
            new URL(String(call[0]), 'http://localhost').pathname ===
              `/api/sites/default/programs/${soon.programId}/intent` &&
            (call[1] as RequestInit | undefined)?.method === 'PUT',
        ),
      ).toBe(true)
    })
  })
})
