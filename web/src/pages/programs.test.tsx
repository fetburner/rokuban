import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, ProgramListItem, Reservation, Service } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
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
  },
  {
    networkId: 32737,
    serviceId: 1032,
    name: 'NHKEテレ',
    channelType: 'GR',
    channel: '26',
    remoteControlKeyId: 2,
    hasLogoData: false,
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

/**
 * stubApi は番組・サービス・予約・容量超過を URL で振り分ける。
 *
 * `/api/programs` は時間窓で実際に絞る。窓の幅を無視して全件返すスタブにすると、
 * 「グリッドは 24 時間ぶんを 1 回で取る」という実装の主張をテストが検証できない。
 */
function stubApi(reservations: Reservation[] = [], overages: CapacityOverage[] = []) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/services') return Promise.resolve(jsonResponse(services))
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/capacity/overages') {
      return Promise.resolve(jsonResponse(overages))
    }
    if (/^\/api\/programs\/\d+\/overlaps$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse({ count: 0, reservations: [] }))
    }
    if (url.pathname === '/api/programs') {
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const serviceId = url.searchParams.get('serviceId')
      const matched = allPrograms.filter(
        (p) =>
          new Date(p.endAt).getTime() > start &&
          new Date(p.startAt).getTime() < end &&
          (serviceId === null || p.serviceId === Number(serviceId)),
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
    const fetchMock = stubApi()
    stubMatchMedia(true)
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'NHKEテレ' }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'NHKEテレ' })).toHaveAttribute(
        'aria-pressed',
        'true',
      ),
    )

    await userEvent.click(screen.getByRole('button', { name: '番組表' }))
    await screen.findByTestId('program-grid')

    // チップの選択が残っている
    expect(screen.getByRole('button', { name: 'NHKEテレ' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    // 選択がグリッドのクエリにも効いている（serviceId 付きで取りに行く）
    const gridRequests = fetchMock.mock.calls
      .map((call) => new URL(String(call[0]), 'http://localhost'))
      .filter((url) => url.pathname === '/api/programs' && url.searchParams.has('serviceId'))
    expect(gridRequests.length).toBeGreaterThan(0)
    expect(gridRequests.at(-1)?.searchParams.get('serviceId')).toBe('1032')
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
  it('日付チップとサービスチップ、続きの読み込みが従来どおり出る', async () => {
    stubApi()
    renderPage()

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '今' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'すべて' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'NHK総合' })).toBeInTheDocument()
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
})
