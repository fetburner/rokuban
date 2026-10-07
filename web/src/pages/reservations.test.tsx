import type { QueryClient } from '@tanstack/react-query'
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, Reservation, Rule } from '@/api/generated'
import { ReservationsPage } from '@/pages/reservations'
import { renderInRouter } from '@/test/router'
import { RESERVATION_GROUPING_KEY } from '@/lib/reservation-grouping'

// このファイルは日付別の時刻順一覧を判定する。新しい既定（シリーズ表示）とは
// 独立して M8-30 の URL 絞り込み・行レイアウトを固定する。
beforeEach(() => localStorage.setItem(RESERVATION_GROUPING_KEY, 'time'))
const originalMatchMedia = Object.getOwnPropertyDescriptor(window, 'matchMedia')
afterEach(() => {
  localStorage.clear()
  if (originalMatchMedia) {
    Object.defineProperty(window, 'matchMedia', originalMatchMedia)
  } else {
    Reflect.deleteProperty(window, 'matchMedia')
  }
})

function enableFinePointer() {
  window.matchMedia = vi.fn((query: string) => ({
    matches: query === '(pointer: fine)',
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia
}

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
  serviceName = 'テスト局',
  overrides: Partial<Reservation> = {},
): Reservation {
  return {
    site,
    programId: id * 10,
    source: 'manual',
    state: 'active',
    title,
    serviceName,
    channelType: 'GR',
    startAt: at(startMinutes),
    durationMs: durationMinutes * 60_000,
    createdAt: at(0),
    updatedAt: at(0),
    skip: false,
    series: null,
    ...overrides,
  }
}

function rule(id: number, name: string): Rule {
  return {
    id,
    name,
    enabled: true,
    priority: 10,
    keepOriginal: 'always',
    createdAt: at(0),
    updatedAt: at(0),
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

type CapacityResult =
  | CapacityOverage[]
  | Promise<CapacityOverage[]>
  | (() => CapacityOverage[] | Promise<CapacityOverage[]>)

/**
 * stubApi は予約一覧・超過区間・サーキットブレーカー（AppShell が常に訊く）を振り分ける。
 *
 * 超過区間は時間窓で実際に絞る。窓を無視して全件返すスタブにすると、「一覧の予約を
 * 覆う窓で訊く」という実装の主張をテストが検証できない。
 */
function stubApi(
  reservations: Reservation[],
  overages: CapacityResult,
  rules: Rule[] = [],
) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/capacity/overages') {
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const result = typeof overages === 'function' ? overages() : overages
      return Promise.resolve(result).then((items) =>
        jsonResponse(
          items.filter(
            (o) => new Date(o.endAt).getTime() > start && new Date(o.startAt).getTime() < end,
          ),
        ),
      )
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

/**
 * renderPage は `ReservationsPage` をルーターの中で描く。
 *
 * 行は詳細への `Link` を含むので、ルーターごと描かないと href を組めない
 * （`renderInRouter` を参照）。返す queryClient は「クエリが解決し終わった」
 * ことの待ち合わせに使う（バッジが出ないことを確かめるテストが、解決前に
 * 通るのを防ぐ）。
 */
function renderPage(initialEntries?: string[]) {
  return renderInRouter(<ReservationsPage />, { path: '/reservations', initialEntries })
}

function renderWith(
  reservations: Reservation[],
  overages: CapacityResult,
  initialEntries?: string[],
  rules: Rule[] = [],
) {
  const fetchMock = stubApi(reservations, overages, rules)
  return { ...renderPage(initialEntries), fetchMock }
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
    expect(screen.getByRole('button', { name: 'すべて（0）' })).toBeInTheDocument()
    // 予約一覧の問い合わせは起きている（スタブが効いていることの確認）
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(0))

    expect(capacityRequests(fetchMock)).toHaveLength(0)
  })
})

describe('予約一覧の要確認フィルタ', () => {
  it('?only=attention では録画されずと容量不足の行だけを残す', async () => {
    const { queryClient } = renderWith(
      [
        reservation(1, '通常の予約', 18 * 60, 60),
        { ...reservation(2, '条件外だけの予約', 19 * 60, 60), state: 'detached' as const },
        { ...reservation(3, '録画されなかった予約', 19 * 60 + 30, 30), state: 'orphaned' as const },
        reservation(4, '容量不足の予約', 20 * 60, 60),
      ],
      [overage(20 * 60, 21 * 60)],
      ['/reservations?only=attention'],
    )

    expect(await screen.findByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
    await overagesSettled(queryClient)

    expect(screen.getByRole('button', { name: '要確認（2）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByText('録画されなかった予約')).toBeInTheDocument()
    expect(screen.getByText('容量不足の予約')).toBeInTheDocument()
    expect(screen.queryByText('条件外だけの予約')).toBeNull()
    expect(screen.queryByText('通常の予約')).toBeNull()
  })

  it('容量の初回取得中は要確認 0 件の空状態を確定しない', async () => {
    let resolveOverages!: (value: CapacityOverage[]) => void
    const overages = new Promise<CapacityOverage[]>((resolve) => {
      resolveOverages = resolve
    })
    const { fetchMock } = renderWith(
      [reservation(1, '通常の予約', 18 * 60, 60)],
      overages,
      ['/reservations?only=attention'],
    )

    await waitFor(() => expect(capacityRequests(fetchMock)).toHaveLength(1))
    expect(screen.getByRole('status')).toHaveTextContent('読み込み中')
    expect(screen.queryByText('確認が要る予約はありません')).toBeNull()

    await act(async () => resolveOverages([]))
    expect(await screen.findByText('確認が要る予約はありません')).toBeInTheDocument()
  })

  it('容量の初回取得が失敗したら要確認なしを確定せず、理由と再試行を示す', async () => {
    const user = userEvent.setup()
    renderWith(
      [reservation(1, '容量未確認の予約', 18 * 60, 60)],
      () => Promise.reject(new Error('capacity unavailable')),
      ['/reservations?only=attention'],
    )

    expect(
      await screen.findByText('容量の確認に失敗しました。要確認の判定が不完全です'),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再試行' })).toBeInTheDocument()
    expect(screen.queryByText('確認が要る予約はありません')).toBeNull()
    // このフィクスチャは state: 'active' の 1 件だけなので、容量抜きの下界でも
    // 要確認は 0 件になる。それでも URL が only=attention なら、選択中の要確認
    // チップは 0 件のまま残す（どのチップも選ばれない状態を作らない）。
    expect(screen.getByRole('button', { name: '要確認（0）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    // 容量の絞り込みが壊れていても、全件表示へ戻る導線は使える
    expect(screen.getByRole('button', { name: 'すべて（1）' })).toBeInTheDocument()
    expect(screen.queryByText('容量未確認の予約')).toBeNull()
    await user.click(screen.getByRole('button', { name: 'すべて（1）' }))
    expect(screen.getByText('容量未確認の予約')).toBeInTheDocument()
  })

  it('録画されなかった予約は容量取得の失敗時も要確認から消えない', async () => {
    renderWith(
      [{ ...reservation(1, '録画されなかった予約', 18 * 60, 60), state: 'orphaned' as const }],
      () => Promise.reject(new Error('capacity unavailable')),
      ['/reservations?only=attention'],
    )

    // orphaned の結論は容量抜きでも確定できるので、取得失敗後も絞り込みに残る。
    expect(
      await screen.findByText('容量の確認に失敗しました。要確認の判定が不完全です'),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再試行' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '要確認（1）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByText('録画されなかった予約')).toBeInTheDocument()
  })

  it('detached だけの予約は要確認に含めない', async () => {
    renderWith(
      [{ ...reservation(1, '条件外だけの予約', 18 * 60, 60), state: 'detached' as const }],
      [],
      ['/reservations?only=attention'],
    )

    expect(await screen.findByRole('button', { name: '要確認（0）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.queryByText('条件外だけの予約')).toBeNull()
  })

  it('取得成功後の再取得失敗では、キャッシュ済みの超過区間を表示しない', async () => {
    let attempts = 0
    const { queryClient } = renderWith(
      [reservation(1, '再取得に失敗する予約', 19 * 60, 60)],
      () => {
        attempts += 1
        return attempts === 1
          ? [overage(19 * 60, 20 * 60)]
          : Promise.reject(new Error('capacity unavailable'))
      },
    )

    expect(await screen.findByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
    await overagesSettled(queryClient)

    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ['/api/capacity/overages'] })
    })

    expect(
      await screen.findByText('容量の確認に失敗しました。要確認の判定が不完全です'),
    ).toBeInTheDocument()
    expect(screen.queryByText('チューナー不足（BS が 1 本）')).toBeNull()
    // このフィクスチャも state: 'active' の 1 件のみなので、再取得失敗で容量分の
    // 判定が抜けると要確認は 0 件になる（チップを隠す実装かどうかではなく件数の主張）
    expect(screen.queryByRole('button', { name: /要確認/ })).toBeNull()
    expect(screen.getByText('再取得に失敗する予約')).toBeInTheDocument()
  })

  it('容量取得の再試行が成功すると不足バッジと要確認絞り込みが復旧する', async () => {
    let attempts = 0
    const user = userEvent.setup()
    renderWith(
      [reservation(1, '再試行で復旧する予約', 20 * 60, 60)],
      () => {
        attempts += 1
        return attempts === 1
          ? Promise.reject(new Error('capacity unavailable'))
          : [overage(20 * 60, 21 * 60)]
      },
      ['/reservations?only=attention'],
    )

    expect(
      await screen.findByText('容量の確認に失敗しました。要確認の判定が不完全です'),
    ).toBeInTheDocument()
    expect(screen.queryByText('再試行で復旧する予約')).toBeNull()

    await user.click(screen.getByRole('button', { name: '再試行' }))

    expect(await screen.findByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
    expect(screen.getByText('再試行で復旧する予約')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '要確認（1）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.queryByText('確認が要る予約はありません')).toBeNull()
  })

  it('要確認が 0 件なら要確認チップを置かず、URL 指定時は選択中の 0 件チップと専用の空状態を出す', async () => {
    const { queryClient } = renderWith(
      [reservation(1, '通常の予約', 18 * 60, 60)],
      [],
      ['/reservations?only=attention'],
    )

    expect(await screen.findByRole('button', { name: 'すべて（1）' })).toBeInTheDocument()
    await overagesSettled(queryClient)

    expect(screen.getByRole('button', { name: '要確認（0）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(screen.getByText('確認が要る予約はありません')).toBeInTheDocument()
    expect(screen.queryByText('通常の予約')).toBeNull()
  })

  it('要確認が 0 件で only が無いときは要確認チップを置かない', async () => {
    const { queryClient } = renderWith([reservation(1, '通常の予約', 18 * 60, 60)], [])

    expect(await screen.findByRole('button', { name: 'すべて（1）' })).toBeInTheDocument()
    await overagesSettled(queryClient)
    expect(screen.queryByRole('button', { name: /要確認/ })).toBeNull()
  })

  it('チップ操作を URL に書き、すべては only を省略する', async () => {
    const user = userEvent.setup()
    const { queryClient, router } = renderWith(
      [
        reservation(1, '通常の予約', 18 * 60, 60),
        { ...reservation(2, '消失した予約', 19 * 60, 60), state: 'orphaned' as const },
      ],
      [],
    )

    expect(await screen.findByText('通常の予約')).toBeInTheDocument()
    await overagesSettled(queryClient)

    await user.click(screen.getByRole('button', { name: '要確認（1）' }))
    await waitFor(() => expect(router.state.location.search).toEqual({ only: 'attention' }))
    expect(screen.queryByText('通常の予約')).toBeNull()

    await user.click(screen.getByRole('button', { name: 'すべて（2）' }))
    await waitFor(() => expect(router.state.location.search).toEqual({}))
    expect(screen.getByText('通常の予約')).toBeInTheDocument()
  })
})

describe('予約一覧の日付見出し・出自・ルールフィルタ（issue #1030）', () => {
  it('日付ごとに時刻順でまとめ、今日・明日だけの見出しと出自リンクを出す', async () => {
    const previousNow = Date.now()
    vi.setSystemTime(new Date(2026, 6, 25, 12, 0, 0, 0))
    try {
      const { queryClient } = renderWith(
        [
          reservation(1, '今日の遅い番組', 22 * 60, 60),
          reservation(2, '明日の番組', 24 * 60 + 30, 60, 'default', 'テスト局', {
            source: 'rule',
            ruleId: 7,
          }),
          reservation(3, '今日の早い番組', 18 * 60, 60, 'default', 'テスト局', {
            source: 'rule',
            ruleId: 7,
          }),
          reservation(4, '3日後の番組', 72 * 60 + 30, 60),
          reservation(5, '手動だがルールも一致', 23 * 60, 60, 'default', 'テスト局', {
            source: 'manual',
            ruleId: 7,
          }),
          reservation(6, '未解決ルール', 24 * 60 + 90, 60, 'default', 'テスト局', {
            source: 'rule',
            ruleId: 99,
          }),
          reservation(7, 'ルール出自で関連 ID なし', 24 * 60 + 150, 60, 'default', 'テスト局', {
            source: 'rule',
          }),
        ],
        [],
        undefined,
        [rule(7, 'ニュース'), rule(8, 'ニュース')],
      )

      const headings = await screen.findAllByTestId('reservation-date-heading')
      expect(headings.map((heading) => heading.textContent)).toEqual([
        '今日 7/25(土)3 件',
        '明日 7/26(日)3 件',
        '7/28(火)1 件',
      ])
      await overagesSettled(queryClient)

      const titles = screen.getAllByText(/番組|一致/).map((title) => title.textContent)
      expect(titles.indexOf('今日の早い番組')).toBeLessThan(titles.indexOf('今日の遅い番組'))
      expect(screen.getAllByRole('link', { name: 'ルール「ニュース (#7)」' })).toHaveLength(2)
      expect(screen.getAllByRole('link', { name: 'ルール「ニュース (#7)」' })[0]).toHaveAttribute(
        'href',
        '/search?ruleId=7',
      )
      expect(screen.getByRole('link', { name: 'ルール「#99」' })).toHaveAttribute(
        'href',
        '/search?ruleId=99',
      )
      expect(row('手動だがルールも一致')).toHaveTextContent('手動')
      expect(
        within(row('手動だがルールも一致')).queryByRole('link', { name: /ルール「/ }),
      ).toBeNull()
      expect(row('ルール出自で関連 ID なし')).toHaveTextContent('ルール')
      expect(row('ルール出自で関連 ID なし')).not.toHaveTextContent('手動')
      expect(
        within(row('ルール出自で関連 ID なし')).queryByRole('link', { name: /ルール「/ }),
      ).toBeNull()
      const rowLink = screen.getByRole('link', { name: /今日の早い番組/ })
      expect(rowLink).not.toHaveAccessibleName(/ニュース|ルール/)
      // 毎日の番組を行ごとに区別できるよう、名前には日付を残す
      expect(rowLink).toHaveAccessibleName(/7\/25 \d{2}:\d{2}/)
      expect(document.querySelectorAll('a a')).toHaveLength(0)
    } finally {
      vi.setSystemTime(new Date(previousNow))
    }
  })

  it('ルールメニューは全予約の件数順を保ち、要確認とルールを組み合わせられる', async () => {
    const user = userEvent.setup()
    const reservations = [
      reservation(1, '手動の予約', 18 * 60, 60),
      reservation(2, 'ルール7の有効予約', 19 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 7,
      }),
      reservation(3, 'ルール7の要確認予約', 20 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 7,
        state: 'detached',
      }),
      reservation(4, '手動だがルール7に一致', 21 * 60, 60, 'default', 'テスト局', {
        source: 'manual',
        ruleId: 7,
      }),
      reservation(5, 'ルール3の消失予約', 22 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 3,
        state: 'orphaned',
      }),
      reservation(6, 'ルール3の通常予約', 23 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 3,
      }),
      reservation(7, 'ルール4の通常予約', 24 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 4,
      }),
      reservation(8, 'ルール4の2件目', 25 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 4,
      }),
      reservation(9, '未解決ルール99', 26 * 60, 60, 'default', 'テスト局', {
        source: 'rule',
        ruleId: 99,
      }),
    ]
    const rules = [
      rule(7, 'Drama'),
      rule(8, 'Drama'),
      rule(3, 'Alpha rule'),
      rule(4, 'Beta rule'),
      rule(55, '予約のないルール'),
    ]
    const { queryClient, router } = renderWith(
      reservations,
      [overage(20 * 60, 21 * 60)],
      ['/reservations?only=attention'],
      rules,
    )

    expect(await screen.findByRole('button', { name: 'すべて（9）' })).toBeInTheDocument()
    await overagesSettled(queryClient)
    expect(screen.getByRole('button', { name: '要確認（2）' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'ルールで絞り込む' }))

    const items = await screen.findAllByRole('menuitem')
    expect(items.map((item) => item.textContent)).toEqual([
      'Drama (#7)3',
      'Alpha rule2',
      'Beta rule2',
      '#991',
    ])
    expect(screen.queryByRole('menuitem', { name: /予約のないルール/ })).toBeNull()

    await user.click(screen.getByRole('menuitem', { name: /Drama \(#7\)/ }))
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ only: 'attention', ruleId: 7 }),
    )
    expect(screen.getByRole('button', { name: 'すべて（3）' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '要確認（1）' })).toBeInTheDocument()
    expect(screen.getByText('ルール7の要確認予約')).toBeInTheDocument()
    expect(screen.queryByText('ルール7の有効予約')).toBeNull()
    expect(screen.getByRole('link', { name: 'ルールの条件を直す' })).toHaveAttribute(
      'href',
      '/search?ruleId=7',
    )

    await user.click(screen.getByRole('button', { name: 'すべて（3）' }))
    await waitFor(() => expect(router.state.location.search).toEqual({ ruleId: 7 }))
    expect(screen.getByText('ルール7の有効予約')).toBeInTheDocument()
    expect(screen.getByText('手動だがルール7に一致')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ルール「Drama (#7)」の絞り込みを解除' }))
    await waitFor(() => expect(router.state.location.search).toEqual({}))
    await user.click(screen.getByRole('button', { name: 'ルールで絞り込む' }))
    expect((await screen.findAllByRole('menuitem')).map((item) => item.textContent)).toEqual([
      'Drama (#7)3',
      'Alpha rule2',
      'Beta rule2',
      '#991',
    ])
  })

  it('削除済みルール ID は #N と専用の空状態になり、条件編集リンクも残す', async () => {
    renderWith(
      [reservation(1, '別ルールの予約', 19 * 60, 60)],
      [],
      ['/reservations?ruleId=99'],
      [rule(7, '別ルール')],
    )

    expect(await screen.findByText('このルールの予約はありません')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'ルール「#99」の絞り込みを解除' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ルールの条件を直す' })).toHaveAttribute(
      'href',
      '/search?ruleId=99',
    )
    expect(screen.queryByRole('button', { name: 'ルールで絞り込む' })).toBeNull()
  })
})

/**
 * 容量バッジの番組表への導線（issue #233 M6-5）。
 *
 * バッジは行本体の `Link`（詳細への導線）の中に元々あった。バッジ自身も
 * `Link` になった以上、**`<a>` の中に `<a>` を作っていないこと**（コンテンツ
 * モデル上不正で、クリックの宛先が不定になる）を構造で確かめる。href の値
 * そのもの（宛先ルート・`at` パラメータ）はここで確認し、実際のクリックによる
 * 画面遷移・グリッドのスクロールは `web/e2e` で確認する（jsdom はレイアウトを
 * 測れない。CLAUDE.md テスト規律）。
 */
describe('予約一覧の容量バッジのリンク化（issue #233 M6-5）', () => {
  it('<a> の中に <a> を作らない（行本体のリンクの外に置く）', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    await screen.findByText('チューナー不足（BS が 1 本）')

    // 行本体のリンクと容量バッジのリンクが「入れ子」ではなく「兄弟」になっている
    // ことを、実際の DOM 構造で確かめる（querySelectorAll('a a') が入れ子の
    // 唯一の機械的な証拠）。
    expect(document.querySelectorAll('a a')).toHaveLength(0)

    const links = screen.getAllByRole('link')
    expect(links).toHaveLength(2)
  })

  it('第2 site の番組も落とさず、番組表ルートへ view=grid と at を積む', async () => {
    renderWith(
      [reservation(1, '高松の交差する番組', 19 * 60, 60, 'takamatsu')],
      [overage(19 * 60, 20 * 60, { site: 'takamatsu' })],
    )

    const badge = await screen.findByText('チューナー不足（BS が 1 本）')
    const badgeLink = badge.closest('a')
    expect(badgeLink).not.toBeNull()
    const expectedAtMs = new Date(at(19 * 60)).getTime()
    // `view=grid` を明示することで `at` の有無や画面幅からの推論が要らなくなる
    // （グリッドが実際にマウントされるかは `showGrid` が `wideScreen` と URL の
    // view で決める。初回フレームの出し分けは実ブラウザの E2E で確認する）
    expect(badgeLink).toHaveAttribute('href', `/programs?view=grid&at=${expectedAtMs}`)
  })

  it('容量バッジが無い行では追加のリンクは増えない', async () => {
    renderWith([reservation(1, 'ニュース7', 19 * 60, 60)], [])

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getAllByRole('link')).toHaveLength(1)
  })
})

/**
 * 行本体のリンクの accessible name（レビュー nit 1）。
 *
 * 行本体のリンクは `absolute inset-0` にして子要素を持たないため（must-fix 2
 * の再構成）、accessible name は `children` からではなく `aria-label` から
 * 計算される。この配線は前回のテスト（リンクの**本数**だけを見るもの）では
 * 検知できない --- `aria-label` を別の属性（例えば `data-row-label`）に
 * 変える壊し方でも本数は変わらないまま全行のリンクが無名になり、577 テスト
 * 全通過・build/lint clean のまま気付けなかった（レビュー実測）。
 * ここでは `getByRole('link', { name: ... })` で**名前による検索そのもの**が
 * 機能することを見る。
 */
describe('予約一覧の行本体リンクの accessible name（issue #233 レビュー nit 1）', () => {
  it('タイトルを含む名前でリンクを引け、宛先は予約詳細になる', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    // バッジ（別のリンク）も同時に存在する状態で、名前による検索が行本体の
    // リンクだけを一意に引けることまで確認する
    await screen.findByText('チューナー不足（BS が 1 本）')

    const rowLink = screen.getByRole('link', { name: /交差する番組/ })
    expect(rowLink).toHaveAttribute('href', '/reservations/default/10')
  })
})

/**
 * 欠損タイトルの表示規則（`web/src/lib/program-labels.ts` の `programTitle`）。
 *
 * 上のブロックは行本体リンクのアクセシブルネームが取れることが主題（issue
 * #233）で、こちらはタイトルが空文字のときに表示・リンク名の双方が
 * 「番組名なし」に揃うこと自体が主題なので、別件として独立に固定する。
 */
describe('予約一覧の行本体表示のタイトル欠損', () => {
  it('タイトルが空文字なら表示とリンク名を「番組名なし」にする', async () => {
    renderWith([reservation(1, '', 19 * 60, 60)], [])

    expect(await screen.findByText('（番組名なし）')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /（番組名なし）/ })).toHaveAttribute(
      'href',
      '/reservations/default/10',
    )
  })
})

describe('予約行のコンテキストメニュー', () => {
  it('詳細リンクとコピーを残し、取消は既存の Undo 付き経路を使う', async () => {
    enableFinePointer()
    const item = reservation(1, '右クリック用の予約', 19 * 60, 60)
    const intentBodies: unknown[] = []
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'
      if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
      if (url.pathname === '/api/reservations' && method === 'GET') {
        return Promise.resolve(jsonResponse([item]))
      }
      if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/capacity/overages') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/sites/default/programs/10/intent' && method === 'PUT') {
        intentBodies.push(JSON.parse(String(init?.body)))
        return Promise.resolve(new Response(null, { status: 204 }))
      }
      throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined)

    renderPage()

    const title = await screen.findByText(item.title)
    const reservationRow = title.closest('li')
    expect(reservationRow).not.toBeNull()
    fireEvent.contextMenu(reservationRow!, { clientX: 80, clientY: 40 })
    const menu = await screen.findByRole('menu')
    expect(screen.getByRole('menuitem', { name: '開く' })).toHaveAttribute(
      'href',
      '/reservations/default/10',
    )
    expect(screen.getByRole('menuitem', { name: '新しいタブで開く' })).toHaveAttribute(
      'target',
      '_blank',
    )
    await user.click(screen.getByRole('menuitem', { name: 'リンクをコピー' }))
    expect(writeText).toHaveBeenCalledWith(
      `${window.location.origin}/reservations/default/10`,
    )
    expect(await screen.findByText('リンクをコピーしました')).toBeInTheDocument()

    fireEvent.contextMenu(reservationRow!, { clientX: 80, clientY: 40 })
    await user.click(await screen.findByRole('menuitem', { name: '予約を取消' }))
    await waitFor(() => expect(intentBodies).toEqual([{ action: 'skip' }]))
    expect(await screen.findByText('予約を取消しました')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '元に戻す' })).toBeInTheDocument()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(menu).not.toBeInTheDocument()
  })

  it('既定のシリーズ表示でも単独予約・シリーズ見出し・展開した各話から操作できる', async () => {
    enableFinePointer()
    localStorage.removeItem(RESERVATION_GROUPING_KEY)
    const individual = reservation(1, '単独の右クリック予約', 19 * 60, 60)
    const firstEpisode = reservation(2, '右クリックシリーズ 第一話', 20 * 60, 60, 'default', 'テスト局', {
      series: '右クリックシリーズ',
    })
    const secondEpisode = reservation(3, '右クリックシリーズ 第二話', 21 * 60, 60, 'default', 'テスト局', {
      series: '右クリックシリーズ',
    })
    const intentBodies: unknown[] = []
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'
      if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
      if (url.pathname === '/api/reservations' && method === 'GET') {
        return Promise.resolve(jsonResponse([individual, firstEpisode, secondEpisode]))
      }
      if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/recording-shelves') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/capacity/overages') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/sites/default/programs/30/intent' && method === 'PUT') {
        intentBodies.push(JSON.parse(String(init?.body)))
        return Promise.resolve(new Response(null, { status: 204 }))
      }
      throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch
    const user = userEvent.setup()

    renderPage()

    const individualTitle = await screen.findByText(individual.title)
    const individualRow = individualTitle
      .closest('[data-testid="reservation-series-row"]')
      ?.querySelector('[data-testid="reservation-series-header"]')
    expect(individualRow).not.toBeNull()
    fireEvent.contextMenu(individualRow!, { clientX: 80, clientY: 40 })
    const individualMenu = await screen.findByRole('menu')
    expect(screen.getByRole('menuitem', { name: '開く' })).toHaveAttribute(
      'href',
      '/reservations/default/10',
    )
    fireEvent.keyDown(individualMenu, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())

    const seriesHeader = screen
      .getAllByTestId('reservation-series-header')
      .find((header) => header.textContent?.includes('右クリックシリーズ'))
    expect(seriesHeader).toBeDefined()
    fireEvent.contextMenu(seriesHeader!, { clientX: 80, clientY: 40 })
    const seriesMenu = await screen.findByRole('menu')
    expect(screen.getByRole('menuitem', { name: '開く' })).toHaveAttribute(
      'href',
      '/reservations/default/20',
    )
    fireEvent.keyDown(seriesMenu, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())

    await user.click(screen.getByRole('button', { name: '右クリックシリーズの予約を開く' }))
    const episodeTitle = await screen.findByText('第二話')
    const episodeRow = episodeTitle.closest('li')
    expect(episodeRow).not.toBeNull()
    fireEvent.contextMenu(episodeRow!, { clientX: 80, clientY: 40 })
    const episodeMenu = await screen.findByRole('menu')
    expect(screen.getByRole('menuitem', { name: '開く' })).toHaveAttribute(
      'href',
      '/reservations/default/30',
    )
    await user.click(screen.getByRole('menuitem', { name: '予約を取消' }))
    await waitFor(() => expect(intentBodies).toEqual([{ action: 'skip' }]))
    expect(await screen.findByText('予約を取消しました')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '元に戻す' })).toBeInTheDocument()
    expect(episodeMenu).not.toBeInTheDocument()
  })

  // 取消の PUT が未解決の間は、開き直したメニューの「予約を取消」が disabled になる。
  function pendingCancelFetch(items: Reservation[], programId: number) {
    let resolvePut: () => void = () => {}
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'
      if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
      if (url.pathname === '/api/reservations' && method === 'GET') {
        return Promise.resolve(jsonResponse(items))
      }
      if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/recording-shelves') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/capacity/overages') return Promise.resolve(jsonResponse([]))
      if (url.pathname === `/api/sites/default/programs/${programId}/intent` && method === 'PUT') {
        return new Promise<Response>((resolve) => {
          resolvePut = () => resolve(new Response(null, { status: 204 }))
        })
      }
      throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
    })
    globalThis.fetch = fetchMock as unknown as typeof fetch
    return { fetchMock, resolvePut: () => resolvePut() }
  }

  async function cancelThenReopen(row: Element, fetchMock: ReturnType<typeof vi.fn>) {
    const user = userEvent.setup()
    fireEvent.contextMenu(row, { clientX: 80, clientY: 40 })
    await user.click(await screen.findByRole('menuitem', { name: '予約を取消' }))
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')).toBe(true),
    )
    fireEvent.contextMenu(row, { clientX: 80, clientY: 40 })
    expect(await screen.findByRole('menuitem', { name: '予約を取消' })).toHaveAttribute('aria-disabled', 'true')
  }

  it('時間順の行は取消の PUT が未解決の間、開き直したメニューの取消が disabled', async () => {
    enableFinePointer()
    const item = reservation(1, '取消保留の予約', 19 * 60, 60)
    const { fetchMock, resolvePut } = pendingCancelFetch([item], 10)
    renderPage()
    const row = (await screen.findByText(item.title)).closest('li')
    expect(row).not.toBeNull()
    await cancelThenReopen(row!, fetchMock)
    await act(async () => resolvePut())
  })

  it('シリーズ表示の単独行とエピソード行も取消の PUT が未解決の間は取消が disabled', async () => {
    enableFinePointer()
    localStorage.removeItem(RESERVATION_GROUPING_KEY)
    const individual = reservation(1, '単独の保留予約', 19 * 60, 60)
    const ep1 = reservation(2, '保留シリーズ 第一話', 20 * 60, 60, 'default', 'テスト局', { series: '保留シリーズ' })
    const ep2 = reservation(3, '保留シリーズ 第二話', 21 * 60, 60, 'default', 'テスト局', { series: '保留シリーズ' })

    const a = pendingCancelFetch([individual, ep1, ep2], 10)
    const { unmount } = renderPage()
    const header = (await screen.findByText(individual.title))
      .closest('[data-testid="reservation-series-row"]')
      ?.querySelector('[data-testid="reservation-series-header"]')
    expect(header).not.toBeNull()
    await cancelThenReopen(header!, a.fetchMock)
    await act(async () => a.resolvePut())
    unmount()

    const b = pendingCancelFetch([individual, ep1, ep2], 30)
    renderPage()
    await userEvent.setup().click(await screen.findByRole('button', { name: '保留シリーズの予約を開く' }))
    const episodeRow = (await screen.findByText('第二話')).closest('li')
    expect(episodeRow).not.toBeNull()
    await cancelThenReopen(episodeRow!, b.fetchMock)
    await act(async () => b.resolvePut())
  })
})

/**
 * 多サイト時に一覧が何を出すか（`docs/frontend/shell.md`「サイトの扱い」）。
 *
 * `GET /api/reservations` は全サイトの予約を返し（api は site に束縛されない ---
 * 不変条件 1）、UI はそれを先頭 site のような画面スコープで絞らない。
 * 上の「default 以外のサイトの予約にも自サイトの不足が出る」は容量バッジの
 * `site` の配線を見るテストで、**一覧そのものが絞られていないこと**は主張の
 * 副産物として通っているに過ぎない（バッジが無い構成に変えると消える）ので、
 * 決定そのものをここで独立に固定する。
 *
 * 期待値は href のリテラルで書く。行の有無だけを見ると、宛先に単一サイト前提の
 * 定数を書いた実装（`params={{ site: 'default', ... }}`）でも通ってしまう。
 */
describe('多サイトの予約一覧（issue #218）', () => {
  it('現在サイト以外の予約も一覧に出し、宛先はその予約自身の site になる', async () => {
    // renderInRouter の単一 site fixture は 'default'（test/router.tsx の testSite）
    renderWith(
      [
        reservation(1, '既定サイトの番組', 19 * 60, 60),
        reservation(2, '高松の番組', 20 * 60, 60, 'takamatsu'),
      ],
      [],
    )

    // 現在サイトの行が出たことを読み込み完了の目印にする（クエリ解決前に
    // getAllByRole が空を返して通る「空虚な成功」を防ぐ）
    await screen.findByRole('link', { name: /既定サイトの番組/ })

    expect(screen.getAllByRole('link').map((a) => a.getAttribute('href'))).toEqual([
      '/reservations/default/10',
      '/reservations/takamatsu/20',
    ])
  })
})

/**
 * 警告の信号色。jsdom は色を計算しないので、当たっているクラスだけを見る
 * （実画素での判定は `web/e2e/design.mjs`。docs/frontend/design.md）。
 */
describe('予約一覧の信号色', () => {
  it('チューナー不足は琥珀（--warning）で、Tailwind 標準パレットを直接使わない', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    const badge = (await screen.findByText('チューナー不足（BS が 1 本）')).closest('span')
      ?.parentElement
    expect(badge).not.toBeNull()
    expect(badge).toHaveClass('text-warning')
    expect(badge).toHaveClass('bg-warning/10')
    expect(badge!.className).not.toMatch(/amber|yellow|orange/)
  })

  it('録画されずは destructive のまま（警告色に落とさない）', async () => {
    renderWith(
      [{ ...reservation(1, '消えた番組', 19 * 60, 60), state: 'orphaned' as const }],
      [],
    )

    const badge = await screen.findByText('録画されず')
    expect(badge).toHaveClass('text-destructive')
    expect(badge.className).not.toMatch(/warning|tally/)
  })

  it('detached の情報は出自へ移し、結論バッジを出さない', async () => {
    renderWith(
      [{ ...reservation(1, 'ルールから外れた番組', 19 * 60, 60), state: 'detached' as const }],
      [],
    )

    expect(await screen.findByText('手動・ルール条件外')).toBeInTheDocument()
    expect(screen.queryByText('ルール外')).toBeNull()
    expect(screen.queryByText('録画予定')).toBeNull()
  })
})

/**
 * 予約一覧に局名（`program_snapshots.service_name` 由来）が出ること（issue #302）。
 *
 * 同じタイトルの番組が日付・局違いで並ぶと局名なしでは区別できないのが issue の
 * 観測そのものなので、**同タイトル 2 件を局名だけで見分けられる**ことを主張する。
 * タイトルが重複するため `row()` ヘルパー（`getByText` は一意な文字列前提）は
 * 使わず、`findAllByText` で得た 2 つのタイトル要素からそれぞれの行を辿る。
 */
describe('予約一覧の局名表示（issue #302）', () => {
  it('同タイトル・別局の予約を局名で区別できる', async () => {
    renderWith(
      [
        reservation(1, '同じ番組名', 19 * 60, 60, 'default', 'NHK総合'),
        reservation(2, '同じ番組名', 20 * 60, 60, 'default', 'NHK Eテレ'),
      ],
      [],
    )

    const titles = await screen.findAllByText('同じ番組名')
    expect(titles).toHaveLength(2)
    const rows = titles.map((el) => el.closest('li'))
    expect(rows[0]).not.toBeNull()
    expect(rows[1]).not.toBeNull()
    expect(within(rows[0]!).getByText('NHK総合')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('NHK Eテレ')).toBeInTheDocument()
  })

  /**
   * 行本体のリンクは `absolute inset-0` の空の `Link` なので、accessible name は
   * `aria-label`（= `rowLabel`）が唯一の情報源。上のテストは「行の中に局名の
   * テキストが居る」ことしか見ておらず、**リンク走査**（スクリーンリーダーの
   * リンク一覧・キーボード）では局名が読めないままでも通る（レビュー実測:
   * `aria-label` に局名が無い実装で 2 本のリンク名は
   * `["同じ番組名 7/25 19:00 1時間","同じ番組名 7/25 20:00 1時間"]`）。
   * 同時刻・別局（同名ニュースの裏かぶり）にすると 2 本の名前は完全に一致し、
   * 局名以外に差が無くなる。
   */
  it('同時刻・別局でも行本体リンクを局名を含む名前で一意に引ける', async () => {
    renderWith(
      [
        reservation(1, '同じ番組名', 19 * 60, 60, 'default', 'NHK総合'),
        reservation(2, '同じ番組名', 19 * 60, 60, 'default', 'NHK Eテレ'),
      ],
      [],
    )

    expect(await screen.findAllByText('同じ番組名')).toHaveLength(2)

    // 名前による検索で 1 本に絞れる（局名が名前に入っていなければ 2 本に
    // 当たって getByRole が投げる、または当たらずに投げる）
    expect(screen.getByRole('link', { name: /NHK総合/ })).toHaveAttribute(
      'href',
      '/reservations/default/10',
    )
    expect(screen.getByRole('link', { name: /NHK Eテレ/ })).toHaveAttribute(
      'href',
      '/reservations/default/20',
    )
  })
})
