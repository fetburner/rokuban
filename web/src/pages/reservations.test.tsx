import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { routeTree } from '@/routes'

/** 時刻はローカルの 0 時基準で組む（表示に時刻が入るのでタイムゾーンに依存させない）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

/** at は 0 時からの分数を ISO 文字列に直す。 */
function at(minutes: number): string {
  return new Date(dayStart.getTime() + minutes * 60_000).toISOString()
}

function reservation(
  id: number,
  title: string,
  startMinutes: number,
  durationMinutes: number,
  site = 'default',
): Reservation {
  return {
    id,
    site,
    programId: id * 10,
    source: 'manual',
    state: 'active',
    title,
    startAt: at(startMinutes),
    durationMs: durationMinutes * 60_000,
    createdAt: at(0),
    updatedAt: at(0),
    skip: false,
  }
}

function overage(
  startMinutes: number,
  endMinutes: number,
  options: Partial<CapacityOverage> = {},
): CapacityOverage {
  return {
    site: 'default',
    startAt: at(startMinutes),
    endAt: at(endMinutes),
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
 * stubApi は予約一覧・超過区間・サーキットブレーカー（AppShell が常に訊く）を振り分ける。
 *
 * 超過区間は時間窓で実際に絞る。窓を無視して全件返すスタブにすると、「一覧の予約を
 * 覆う窓で訊く」という実装の主張をテストが検証できない。
 */
function stubApi(reservations: Reservation[], overages: CapacityOverage[]) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/capacity/overages') {
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const matched = overages.filter(
        (o) => new Date(o.endAt).getTime() > start && new Date(o.startAt).getTime() < end,
      )
      return Promise.resolve(jsonResponse(matched))
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

/**
 * renderPage は本物のルートツリーで `/reservations` を描く。
 *
 * 行は詳細への `Link` を含むので、ルーターごと描かないと href を組めない。
 * 返す queryClient は「クエリが解決し終わった」ことの待ち合わせに使う
 * （バッジが出ないことを確かめるテストが、解決前に通るのを防ぐ）。
 */
function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ['/reservations'] }),
  })
  const view = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return { ...view, queryClient }
}

function renderWith(reservations: Reservation[], overages: CapacityOverage[]) {
  const fetchMock = stubApi(reservations, overages)
  return { ...renderPage(), fetchMock }
}

/** row はタイトルからその予約の行を引く。 */
function row(title: string): HTMLElement {
  const el = screen.getByText(title).closest('li')
  if (!el) throw new Error(`row ${title} not found`)
  return el
}

/**
 * overagesSettled は超過区間のクエリが解決し、飛んでいる問い合わせが無くなるまで待つ。
 *
 * 「バッジが出ない」ことの確認はこれを通してから行う。`isFetching() === 0` だけを
 * 見ると、クエリがまだ始まっていない瞬間を「解決済み」と読んで通ってしまう
 * （CLAUDE.md「非同期の空虚な成功」）ので、成功した超過クエリの存在も要求する。
 *
 * 窓は予約一覧から作るので、キャッシュには「予約が届く前の窓（= 停止中）」の
 * エントリも残る。全部が success になることは求められない。
 */
async function overagesSettled(queryClient: QueryClient): Promise<void> {
  await waitFor(() => {
    expect(queryClient.isFetching()).toBe(0)
    const statuses = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['/api/capacity/overages'] })
      .map((query) => query.state.status)
    expect(statuses).toContain('success')
  })
}

/** capacityRequests は超過区間への問い合わせの URL を返す。 */
function capacityRequests(fetchMock: ReturnType<typeof stubApi>): URL[] {
  return fetchMock.mock.calls
    .map((call) => new URL(String(call[0]), 'http://localhost'))
    .filter((url) => url.pathname === '/api/capacity/overages')
}

describe('予約一覧のチューナー不足バッジ', () => {
  it('超過区間と交差する予約にだけ出る', async () => {
    const { queryClient } = renderWith(
      [reservation(1, '交差する番組', 19 * 60, 60), reservation(2, '交差しない番組', 22 * 60, 60)],
      [overage(19 * 60, 20 * 60)],
    )

    // 交差する側にバッジが出ることが「クエリが解決した」ことの証拠になるので、
    // 出ない側の確認が空虚な成功にならない
    expect(await screen.findByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
    expect(within(row('交差する番組')).getByText(/チューナー不足/)).toBeInTheDocument()
    expect(within(row('交差しない番組')).queryByText(/チューナー不足/)).toBeNull()
    await overagesSettled(queryClient)
  })

  it('別サイトの超過区間では出ない（判定はサイトごとに独立）', async () => {
    renderWith(
      [
        reservation(1, '同じサイトの番組', 19 * 60, 60),
        reservation(2, '別サイトの時間帯の番組', 21 * 60, 60),
      ],
      [
        overage(19 * 60, 20 * 60, { site: 'default' }),
        overage(21 * 60, 22 * 60, { site: 'takamatsu', shortfall: 2, jammedTypes: ['GR'] }),
      ],
    )

    // 高松の不足は default の予約に効かない。同時に default の不足が出ているので
    // 「まだ届いていないから出ていない」ではないことが分かる
    await waitFor(() =>
      expect(within(row('同じサイトの番組')).getByText(/チューナー不足/)).toBeInTheDocument(),
    )
    expect(within(row('別サイトの時間帯の番組')).queryByText(/チューナー不足/)).toBeNull()
    // 高松側の内訳（GR が 2 本）がどこにも漏れていない
    expect(screen.queryByText(/GR/)).toBeNull()
  })

  // 上のテストは「site で絞っている」ことしか担保しない。予約自身の site ではなく
  // 単一サイト前提の定数（'default'）を渡す実装でも、フィクスチャが全部 default
  // なら通ってしまう。**default 以外のサイトの予約に、同じサイトの不足を当てる**
  // ケースを置いて、定数を書いた実装で落ちるようにする。
  it('default 以外のサイトの予約にも自サイトの不足が出る', async () => {
    renderWith(
      [reservation(1, '高松の番組', 19 * 60, 60, 'takamatsu')],
      [overage(19 * 60, 20 * 60, { site: 'takamatsu', shortfall: 2, jammedTypes: ['GR'] })],
    )

    expect(await screen.findByText('チューナー不足（GR が 2 本）')).toBeInTheDocument()
  })

  it('区間の端で接するだけなら出ない', async () => {
    const { queryClient } = renderWith(
      [
        reservation(1, '接するだけの番組', 20 * 60, 60),
        reservation(2, '食い込む番組', 19 * 60 + 30, 60),
      ],
      [overage(19 * 60, 20 * 60)],
    )

    // 19:00-20:00 の不足に対し、20:00 開始の予約は不足の外側
    await waitFor(() =>
      expect(within(row('食い込む番組')).getByText(/チューナー不足/)).toBeInTheDocument(),
    )
    expect(within(row('接するだけの番組')).queryByText(/チューナー不足/)).toBeNull()
    await overagesSettled(queryClient)
  })

  it('超過区間が無ければ何も言わない（沈黙を肯定にしない）', async () => {
    const { queryClient } = renderWith([reservation(1, 'ニュース7', 19 * 60, 60)], [])

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    // 予約一覧が出たあと、超過の問い合わせが解決し切るまで待ってから確かめる
    await overagesSettled(queryClient)

    expect(screen.queryByText(/チューナー/)).toBeNull()
    // 「収まります」「競合なし」に相当する肯定的な表示は出さない
    expect(screen.queryByText(/競合/)).toBeNull()
  })

  it('複数区間に跨るときは最も不足の大きい区間の内訳を出す', async () => {
    renderWith(
      [reservation(1, '2 区間に跨る番組', 19 * 60, 120)],
      [
        overage(19 * 60, 20 * 60, { shortfall: 1, jammedTypes: ['GR'] }),
        overage(20 * 60, 21 * 60, { shortfall: 2, jammedTypes: ['BS'] }),
      ],
    )

    // 種別を合併して「GR・BS が 3 本」とは言わない（どの区間でも成り立たない主張）
    expect(await screen.findByText('チューナー不足（BS が 2 本）')).toBeInTheDocument()
  })

  it('一覧の予約を覆う窓で問い合わせる', async () => {
    const { queryClient, fetchMock } = renderWith(
      [reservation(1, '早い番組', 10 * 60, 30), reservation(2, '遅い番組', 19 * 60, 60)],
      [],
    )

    expect(await screen.findByText('早い番組')).toBeInTheDocument()
    await overagesSettled(queryClient)

    // 窓が固定幅だと、その外に出た予約のバッジが黙って消える
    const asked = capacityRequests(fetchMock).at(-1)
    expect(asked?.searchParams.get('start')).toBe(at(10 * 60))
    expect(asked?.searchParams.get('end')).toBe(at(20 * 60))
  })

  it('予約が無ければ超過を問い合わせない', async () => {
    const { fetchMock } = renderWith([], [])

    expect(await screen.findByText('予約がありません')).toBeInTheDocument()
    // 予約一覧の問い合わせは起きている（スタブが効いていることの確認）
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(0))

    expect(capacityRequests(fetchMock)).toHaveLength(0)
  })
})
