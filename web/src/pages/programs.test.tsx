import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, ProgramListItem, Reservation, Service } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { dayOrigin } from '@/lib/day-offset'
import { ProgramsPage } from '@/pages/programs'

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

function program(
  programId: number,
  serviceId: number,
  startOffsetHours: number,
  name: string,
): ProgramListItem {
  const startAt = origin + startOffsetHours * 3_600_000
  return {
    programId,
    networkId: 32736,
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
const alsoSoon = program(2, 1032, 1, '手話ニュース')
/** 8 時間後の番組。リストの最初の窓には入らず、グリッド（24 時間）には入る。 */
const later = program(3, 1024, 8, '深夜ドラマ')

const allPrograms = [soon, alsoSoon, later]

/** programAtAbsolute は絶対時刻を起点にした 1 時間番組を作る（日付ジャンプ・遡行のテスト用）。 */
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

function reservation(id: number, programId: number, title: string): Reservation {
  return {
    id,
    site: 'default',
    programId,
    source: 'manual',
    state: 'active',
    title,
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
 * `programs` は絞り込み対象の番組集合（既定は `allPrograms`）。日付ジャンプ・
 * 遡行のテストは絶対時刻で配置した専用の番組を渡す。`onProgramsCall` は
 * `/api/sites/default/programs` への何回目の呼び出しかを受け取り、Response を
 * 返せばそれで応答を差し替える（続きの読み込み失敗のテスト用）。
 */
function stubApi(
  reservations: Reservation[] = [],
  overages: CapacityOverage[] = [],
  programs: ProgramListItem[] = allPrograms,
  onProgramsCall?: (callIndex: number) => Response | undefined,
) {
  let programsCallIndex = 0
  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(jsonResponse(services))
    }
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/capacity/overages') {
      return Promise.resolve(jsonResponse(overages))
    }
    if (/^\/api\/sites\/default\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }
    if (/^\/api\/sites\/default\/programs\/\d+\/intent$/.test(url.pathname) && init?.method === 'PUT') {
      return Promise.resolve(noContentResponse())
    }
    if (url.pathname === '/api/sites/default/programs') {
      programsCallIndex++
      const override = onProgramsCall?.(programsCallIndex)
      if (override) return Promise.resolve(override)
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const serviceIds = url.searchParams.getAll('serviceId').map(Number)
      const matched = programs.filter(
        (p) =>
          new Date(p.endAt).getTime() > start &&
          new Date(p.startAt).getTime() < end &&
          (serviceIds.length === 0 || serviceIds.includes(p.serviceId)),
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

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  const view = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <ProgramsPage />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { ...view, queryClient }
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

  it('チャンネルを選ぶと API に serviceId が付く（サーバー側で絞る）', async () => {
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
    // グリッドへ切り替える順序にしないと、グリッド側だけ serviceId が
    // 付かないバグが再発しても、リストのクエリ（選択変更で既に再取得済み）
    // だけを見ていては気付けない
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
      requestsAfterSelection.every((url) => url.searchParams.getAll('serviceId').includes('1024')),
    ).toBe(true)
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

describe('ProgramsPage の遡行（前の時間窓の読み込み）', () => {
  it('今日（offset 0）のままでは「前を読み込む」ボタンが出ない（起点が既に下限）', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '前を読み込む' })).not.toBeInTheDocument()
  })

  it('先の日付へジャンプすると「前を読み込む」ボタンが出て、押すと前の窓の番組が増える', async () => {
    // offset 1（明日）ではなく 2（明後日）にする: `allPrograms` の `soon` /
    // `alsoSoon`（現在時刻 + 1 時間）が、実行時刻によっては明日 0 時と一致し
    // うる（現在が 23 時台だとちょうど一致する）。offset 2 ならどの実行時刻でも
    // 重ならない。
    const targetOriginMs = dayOrigin(2).getTime()
    // 前日深夜（targetOrigin の 1 時間前）は必ず「1 窓遡る」だけで届く
    // 位置に置く（windowHours は 6 時間なので、6 時間以内なら 1 回で届く）。
    const lateTonight = programAtAbsolute(201, 1024, targetOriginMs - 3_600_000, '前日深夜の番組')
    stubApi([], [], [...allPrograms, lateTonight])
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()

    const dayGroup = screen.getByRole('group', { name: '日付' })
    await userEvent.click(within(dayGroup).getAllByRole('button')[2]) // offset 2 = 明後日

    // ジャンプ直後は明後日 0 時からの窓なので、前日深夜の番組はまだ見えない
    await waitFor(() => expect(screen.queryByText('ニュース7')).not.toBeInTheDocument())
    expect(screen.queryByText('前日深夜の番組')).not.toBeInTheDocument()

    const loadPrevious = await screen.findByRole('button', { name: '前を読み込む' })
    await userEvent.click(loadPrevious)

    expect(await screen.findByText('前日深夜の番組')).toBeInTheDocument()
  })

  it('下限（now）まで遡ると「前を読み込む」ボタンが消える', async () => {
    // offset 1 ではなく 2 にする理由は上のテストと同じ（`allPrograms` の
    // 固定オフセットとの窓の重なりを実行時刻によらず避けるため）。
    const nowMs = dayOrigin(0).getTime()
    const targetOriginMs = dayOrigin(2).getTime()
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()

    const dayGroup = screen.getByRole('group', { name: '日付' })
    await userEvent.click(within(dayGroup).getAllByRole('button')[2]) // offset 2 = 明後日
    await waitFor(() => expect(screen.queryByText('ニュース7')).not.toBeInTheDocument())

    // windowHours（pages/programs.tsx のプライベート定数、6 時間）ぶんずつ
    // 遡ると下限（now）に達するまでに必要な回数。これを超えて遡ることは無い
    // （両方向のテスト: 下限に届く前はボタンがあり、届いたら消える）。
    const windowHoursMs = 6 * 3_600_000
    const stepsToLowerBound = Math.ceil((targetOriginMs - nowMs) / windowHoursMs)

    for (let i = 0; i < stepsToLowerBound; i++) {
      const button = await screen.findByRole('button', { name: '前を読み込む' })
      await userEvent.click(button)
      // このクリックの取得が終わってから次のクリックへ進む
      await waitFor(() => {
        const stillLoading = screen.queryAllByRole('button', { name: '読み込み中…' })
        expect(stillLoading).toHaveLength(0)
      })
    }

    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '前を読み込む' })).not.toBeInTheDocument(),
    )
  })

  /**
   * 3 回目の修正で、遡行のスクロール位置復元は DOM アンカー（`document.querySelector`
   * で挿入後に同じ行を探し直す方式）から、`ProgramList`（仮想化を持つコンポーネント）
   * 内部の `virtualizer.scrollToIndex` に置き換えた（`components/program-list.tsx`
   * のコメント参照）。DOM アンカー方式は、先頭への挿入直後にアンカーだった行が
   * 仮想化のオーバースキャン外へ弾き出されて DOM から消えるため機能しなかった
   * （実機で確認済み）。
   *
   * `scrollToIndex` の実効果（実際にスクロール位置が揃うか）は jsdom では検証
   * できない（レイアウトエンジンを持たないため、可視範囲バイパス
   * （`domLayoutMeasurable()` が false）の分岐に入り、`ProgramList` はこの環境では
   * そもそも `scrollToIndex` を呼ばない）。ここでは統合テストとして安全に確認できる
   * 範囲 --- 挿入前のリストが空（アンカーが 1 件も取れない）場合でもクラッシュせず
   * 前の窓の番組が増えること --- だけを見る。「控えた programId から新しい添字を
   * 引く」部分自体は `lib/program-list-key.test.ts` の `findProgramIndex` で
   * 純関数として両方向（見つかる／見つからない）をテスト済み。
   */
  it('挿入前のリストが空（アンカーが取れない）状態で「前を読み込む」を押しても、前の窓の番組が増える', async () => {
    // offset 1 ではなく 2（明後日）にするのは、上のテストと同じ理由
    // （`allPrograms` の固定オフセットとの窓の重なりを実行時刻によらず避けるため）。
    const dayAfter2Ms = dayOrigin(2).getTime()
    const lateTonight = programAtAbsolute(203, 1024, dayAfter2Ms - 3_600_000, '前日深夜の番組3')
    stubApi([], [], [...allPrograms, lateTonight])
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    const dayGroup = screen.getByRole('group', { name: '日付' })
    await userEvent.click(within(dayGroup).getAllByRole('button')[2]) // offset 2 = 明後日
    await waitFor(() => expect(screen.queryByText('ニュース7')).not.toBeInTheDocument())
    // アンカーになりうる行が無いこと（空のリスト）を確認したうえで進める
    expect(document.querySelector('[data-program-id]')).not.toBeInTheDocument()
    // 空でも「この時間帯の番組がありません」とボタンが両方出る
    expect(screen.getByText('この時間帯の番組がありません')).toBeInTheDocument()

    const loadPrevious = await screen.findByRole('button', { name: '前を読み込む' })
    await userEvent.click(loadPrevious)

    expect(await screen.findByText('前日深夜の番組3')).toBeInTheDocument()
  })
})

describe('ProgramsPage の日付ジャンプ（先頭の窓に重なる前日の番組を出さない。3 回目の修正）', () => {
  it('ジャンプ先の窓と重なって返ってきた前日の番組をリストの先頭に出さず、ハイライトもジャンプ先の日のまま', async () => {
    // offset 1 ではなく 2（明後日）にする理由は他の遡行テストと同じ
    // （`allPrograms` の固定オフセットとの窓の重なりを実行時刻によらず避けるため）。
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
