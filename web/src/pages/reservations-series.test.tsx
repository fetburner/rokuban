import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, RecordingShelf, Reservation, Rule } from '@/api/generated'
import { RESERVATION_GROUPING_KEY } from '@/lib/reservation-grouping'
import { ReservationsPage } from '@/pages/reservations'
import { renderInRouter } from '@/test/router'

const base = new Date('2026-10-02T00:00:00+09:00').getTime()
const stamp = new Date(base).toISOString()

function at(hour: number): string {
  return new Date(base + hour * 3_600_000).toISOString()
}

function reservation(
  id: number,
  title: string,
  series: string | null,
  hour: number,
  overrides: Partial<Reservation> = {},
): Reservation {
  return {
    site: 'default',
    programId: id * 10,
    source: 'rule',
    ruleId: 8,
    state: 'active',
    title,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: at(hour),
    durationMs: 60 * 60_000,
    createdAt: stamp,
    updatedAt: stamp,
    skip: false,
    series,
    ...overrides,
  }
}

function rule(): Rule {
  return {
    id: 8,
    name: '毎週ドラマ',
    enabled: true,
    priority: 10,
    keepOriginal: 'always',
    createdAt: stamp,
    updatedAt: stamp,
  }
}

function overage(hour: number, site = 'default'): CapacityOverage {
  return {
    site,
    startAt: at(hour),
    endAt: at(hour + 1),
    shortfall: 1,
    jammedTypes: ['BS'],
  }
}

function shelf(value: string | null, overrides: Partial<RecordingShelf> = {}): RecordingShelf {
  return {
    value,
    title: '毎週ドラマ 第3話',
    count: 4,
    playableCount: 4,
    unwatchedCount: 2,
    latestStartAt: at(1),
    representativeId: 71,
    ...overrides,
  }
}

type ShelfResult = RecordingShelf[] | (() => RecordingShelf[] | Promise<RecordingShelf[]>)

function renderPage({
  reservations,
  shelves = [],
  overages = [],
  rules = [rule()],
  sites = ['default'],
  initialEntries = ['/reservations'],
}: {
  reservations: Reservation[]
  shelves?: ShelfResult
  overages?: CapacityOverage[]
  rules?: Rule[]
  sites?: string[]
  initialEntries?: string[]
}) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))
    if (url.pathname === '/api/capacity/overages') return Promise.resolve(jsonResponse(overages))
    if (url.pathname === '/api/recording-shelves') {
      const result = typeof shelves === 'function' ? shelves() : shelves
      return Promise.resolve(result).then(jsonResponse)
    }
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return {
    ...renderInRouter(<ReservationsPage />, { path: '/reservations', initialEntries }),
    fetchMock,
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

afterEach(() => {
  localStorage.clear()
})

describe('予約一覧のシリーズ表示', () => {
  it('localStorage が空ならシリーズ表示にし、スキップを飛ばした次回と棚への導線を出す', async () => {
    const user = userEvent.setup()
    const { fetchMock } = renderPage({
      reservations: [
        reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18, {
          skip: true,
          dedupMatchRecordingId: 99,
        }),
        reservation(2, '毎週ドラマ 第2話', '毎週ドラマ', 20),
      ],
      shelves: [shelf('毎週ドラマ')],
    })

    const grouping = await screen.findByRole('group', { name: '予約のまとめ方' })
    expect(within(grouping).getByRole('button', { name: 'シリーズ' })).toHaveAttribute('aria-pressed', 'true')
    expect(within(grouping).getByRole('button', { name: '時間順' })).toHaveAttribute('aria-pressed', 'false')

    const row = await screen.findByTestId('reservation-series-row')
    expect(within(row).getByTestId('reservation-series-title')).toHaveTextContent('毎週ドラマ')
    expect(within(row).getByText(/20:00/)).toBeInTheDocument()
    expect(within(row).queryByText(/18:00/)).toBeNull()
    expect(within(row).getByText('今後 2 本')).toBeInTheDocument()
    const hubLinks = within(row).getAllByRole('link', { name: /録画 4 本/ })
    expect(hubLinks.every((link) => link.getAttribute('href') === '/recordings/71/series')).toBe(true)

    await user.click(within(row).getByRole('button', { name: '毎週ドラマの予約を開く' }))
    expect(await within(row).findByText('第1話')).toBeInTheDocument()
    expect(within(row).getByText('第2話')).toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([input]) => String(input).includes('/api/recording-shelves'))).toBe(true)
  })

  it('棚の取得が未完了の間は「まだ録画なし」と空箱を出さない', async () => {
    let resolveShelves!: (items: RecordingShelf[]) => void
    const pending = new Promise<RecordingShelf[]>((resolve) => {
      resolveShelves = resolve
    })
    renderPage({
      reservations: [reservation(1, '保存中の番組', '保存中の番組', 18)],
      shelves: () => pending,
    })

    const row = await screen.findByTestId('reservation-series-row')
    expect(within(row).getByText('保存中の番組')).toBeInTheDocument()
    expect(within(row).queryByText('まだ録画なし')).toBeNull()
    expect(within(row).queryByTestId('reservation-recording-shelf-empty')).toBeNull()

    await act(async () => resolveShelves([]))
    await waitFor(() => expect(within(row).getAllByText('まだ録画なし')).toHaveLength(2))
  })

  it('棚の取得に失敗したら「まだ録画なし」を出さず、ライブラリ箱を隠す', async () => {
    const { queryClient } = renderPage({
      reservations: [reservation(1, '保存中の番組', '保存中の番組', 18)],
      shelves: () => Promise.reject(new Error('shelves unavailable')),
    })

    const row = await screen.findByTestId('reservation-series-row')
    await waitFor(() => {
      const query = queryClient
        .getQueryCache()
        .findAll()
        .find((candidate) => candidate.queryKey[0] === '/api/recording-shelves')
      expect(query?.state.status).toBe('error')
    })
    expect(within(row).queryByText('まだ録画なし')).toBeNull()
    expect(within(row).queryByTestId('reservation-recording-shelf-empty')).toBeNull()
    expect(within(row).queryByRole('link', { name: /録画/ })).toBeNull()
  })

  it('成功済みの棚は再取得中も箱を残し、再取得が失敗したら箱ごと隠す', async () => {
    let calls = 0
    let rejectRefetch!: (error: Error) => void
    const refetch = new Promise<RecordingShelf[]>((_resolve, reject) => {
      rejectRefetch = reject
    })
    const { queryClient } = renderPage({
      reservations: [reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18)],
      shelves: () => {
        calls += 1
        return calls === 1 ? [shelf('毎週ドラマ')] : refetch
      },
    })
    const row = await screen.findByTestId('reservation-series-row')
    expect(await within(row).findAllByRole('link', { name: /録画 4 本/ })).toHaveLength(2)

    // (a) 再取得の応答を保留している間も、成功済みの棚から箱を出し続ける。
    act(() => {
      void queryClient.invalidateQueries({ queryKey: ['/api/recording-shelves'] })
    })
    await waitFor(() => expect(calls).toBe(2))
    expect(within(row).getAllByRole('link', { name: /録画 4 本/ })).toHaveLength(2)

    // (b) 再取得が失敗したら、棚の有無を主張しない（箱も「まだ録画なし」も出さない）。
    await act(async () => rejectRefetch(new Error('shelves unavailable')))
    await waitFor(() => expect(within(row).queryByRole('link', { name: /録画/ })).toBeNull())
    expect(within(row).queryByText('まだ録画なし')).toBeNull()
    expect(within(row).queryByTestId('reservation-recording-shelf-empty')).toBeNull()
  })

  it('開閉できる行の右端は遷移の › ではなく ∨ にし、1 本だけの行は › にする', async () => {
    const user = userEvent.setup()
    renderPage({
      reservations: [
        reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18),
        reservation(2, '毎週ドラマ 第2話', '毎週ドラマ', 20),
        reservation(3, 'ひとり番組', 'ひとり番組', 22),
      ],
    })

    const rows = await screen.findAllByTestId('reservation-series-row')
    const multi = rows.find((row) => within(row).queryByRole('button') !== null)!
    const single = rows.find((row) => within(row).queryByRole('button') === null)!
    // 閉じているときも ∨。›（chevron-right）は遷移の印だけに使う。
    expect(multi.querySelector('svg.lucide-chevron-down')).not.toBeNull()
    expect(multi.querySelector('svg.lucide-chevron-right')).toBeNull()
    await user.click(within(multi).getByRole('button'))
    expect(multi.querySelector('svg.lucide-chevron-right')).toBeNull()
    expect(single.querySelector('svg.lucide-chevron-right')).not.toBeNull()
    expect(single.querySelector('svg.lucide-chevron-down')).toBeNull()
  })

  it('単一サイト構成では各回に site を出さない', async () => {
    const user = userEvent.setup()
    renderPage({
      sites: ['default'],
      reservations: [
        reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18),
        reservation(2, '毎週ドラマ 第2話', '毎週ドラマ', 20),
      ],
    })

    const row = await screen.findByTestId('reservation-series-row')
    await user.click(within(row).getByRole('button', { name: '毎週ドラマの予約を開く' }))
    const episodes = await within(row).findByRole('list', { name: '毎週ドラマの予約' })
    expect(within(episodes).queryByText('default')).toBeNull()
  })

  it('2 拠点構成なら、予約が 1 site だけでも各回に site を出す', async () => {
    const user = userEvent.setup()
    renderPage({
      sites: ['default', 'takamatsu'],
      reservations: [
        reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18),
        reservation(2, '毎週ドラマ 第2話', '毎週ドラマ', 20),
      ],
    })

    const row = await screen.findByTestId('reservation-series-row')
    await user.click(within(row).getByRole('button', { name: '毎週ドラマの予約を開く' }))
    const episodes = await within(row).findByRole('list', { name: '毎週ドラマの予約' })
    await waitFor(() => expect(within(episodes).getAllByText('default')).toHaveLength(2))
  })

  it('value=null の録画棚を series=null の予約へ突き合わせない', async () => {
    renderPage({
      reservations: [reservation(1, 'EPG から消えた番組', null, 18, { state: 'orphaned' })],
      shelves: [shelf(null, { count: 99, unwatchedCount: 99, representativeId: 999 })],
    })

    const row = await screen.findByTestId('reservation-series-row')
    expect(within(row).getByTestId('reservation-series-title')).toHaveTextContent('EPG から消えた番組')
    expect(within(row).queryByText(/99/)).toBeNull()
    expect(within(row).queryByRole('link', { name: /番組ハブ/ })).toBeNull()
    expect(within(row).queryByTestId('reservation-recording-shelf-empty')).toBeNull()
  })

  it('only=attention と ruleId で絞った予約だけを再集計する', async () => {
    localStorage.setItem(RESERVATION_GROUPING_KEY, 'series')
    renderPage({
      initialEntries: ['/reservations?only=attention&ruleId=8'],
      reservations: [
        reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18, {
          skip: true,
          dedupMatchRecordingId: 99,
        }),
        reservation(2, '毎週ドラマ 第2話', '毎週ドラマ', 20, { state: 'detached' }),
        reservation(3, '毎週ドラマ 第3話', '毎週ドラマ', 22, { ruleId: 9 }),
      ],
      overages: [overage(22)],
    })

    const row = await screen.findByTestId('reservation-series-row')
    expect(within(row).getByText('今後 1 本')).toBeInTheDocument()
    expect(within(row).getByText('ルール外 1')).toBeInTheDocument()
    expect(within(row).queryByText(/重複スキップ/)).toBeNull()
    expect(within(row).queryByText(/容量不足/)).toBeNull()
    expect(within(row).queryByRole('button', { name: /予約を開く/ })).toBeNull()
  })

  it('時間順へ切り替えると URL を変えず localStorage に保存する', async () => {
    const user = userEvent.setup()
    renderPage({ reservations: [reservation(1, '毎週ドラマ 第1話', '毎週ドラマ', 18)] })

    await user.click(await screen.findByRole('button', { name: '時間順' }))
    expect(screen.getByRole('button', { name: '時間順' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.queryByTestId('reservation-series-row')).toBeNull()
    expect(localStorage.getItem(RESERVATION_GROUPING_KEY)).toBe('time')
  })

  it('単件の除外予約にも除外バッジを残す', async () => {
    renderPage({
      reservations: [
        reservation(1, '手動で除外した番組', '手動で除外した番組', 18, { skip: true }),
      ],
    })

    const row = await screen.findByTestId('reservation-series-row')
    expect(within(row).getByText('除外')).toBeInTheDocument()
    expect(within(row).getByRole('link', { name: /手動で除外した番組/ })).toBeInTheDocument()
  })
})
