import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { ProgramListItem, Service } from '@/api/generated'
import { ProgramList, type ReservationActions } from '@/components/program-list'
import { formatDate } from '@/lib/format'

const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0).getTime()

const services = new Map<number, Service>([
  [
    1024,
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
  ],
])

/** program は `startOffsetHours` 時間後に開始する 30 分番組を作る。 */
function program(programId: number, startOffsetHours: number, name = `番組 ${programId}`): ProgramListItem {
  const startAt = dayStart + startOffsetHours * 3_600_000
  return {
    programId,
    networkId: 32736,
    serviceId: 1024,
    eventId: programId,
    startAt: new Date(startAt).toISOString(),
    endAt: new Date(startAt + 1_800_000).toISOString(),
    durationMs: 1_800_000,
    name,
    description: '',
    genres: [3],
    isFree: true,
  }
}

function actions(overrides: Partial<ReservationActions> = {}): ReservationActions {
  return {
    reserve: vi.fn(),
    cancel: vi.fn(),
    isBusy: () => false,
    reservedProgramIds: new Set(),
    ...overrides,
  }
}

/**
 * jsonResponse は overlaps エンドポイントのスタブ応答。ProgramRow は未予約の行に
 * 常に ProgramOverlapWarning を出し、それが `GET .../overlaps` を叩くため、
 * ProgramList を単体でマウントするだけでも fetch のスタブが要る
 * （programs.test.tsx の stubApi と同じ理由）。
 */
function stubFetch() {
  globalThis.fetch = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify({ count: 0, reservations: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ) as unknown as typeof fetch
}

function renderList(
  programs: ProgramListItem[],
  reservationActions = actions(),
  extra: { onVisibleDayChange?: (dayOffset: number) => void; now?: number } = {},
) {
  stubFetch()
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <ProgramList
        programs={programs}
        serviceById={services}
        actions={reservationActions}
        onVisibleDayChange={extra.onVisibleDayChange}
        now={extra.now}
      />
    </QueryClientProvider>,
  )
}

describe('ProgramList', () => {
  it('大量の番組（500 件）を渡しても、最初・中間・最後の行が screen から引ける', async () => {
    // jsdom は DOM のレイアウトを計算できない（web/src/lib/list-virtualization.ts）
    // ので、このテストは「間引かれて消えていないか」を実質的に固定する。仮想化の
    // 一般的な実装のまま jsdom で動かすと、素朴には 1 行も描かれない
    // （可視範囲の計算が壊れる）か、間引かれて末尾の行が見つからないかのどちらかで
    // 壊れる。全部描く側に倒れていることの確認。
    const programs = Array.from({ length: 500 }, (_, i) => program(i, i * 0.5))
    renderList(programs)

    expect(await screen.findByText('番組 0')).toBeInTheDocument()
    expect(screen.getByText('番組 250')).toBeInTheDocument()
    expect(screen.getByText('番組 499')).toBeInTheDocument()
  })

  it('日付ヘッダを描画し、top が --page-header-height を参照する', async () => {
    // 2 日にまたがる番組を用意して日付境界を作る
    const day1 = program(1, 1, '朝のニュース')
    const day2 = program(2, 30, '深夜の番組') // 30 時間後 = 翌々日相当だが日付は進む
    renderList([day1, day2])

    expect(await screen.findByText('朝のニュース')).toBeInTheDocument()

    const headers = screen.getAllByRole('heading', { level: 2 })
    expect(headers.length).toBeGreaterThanOrEqual(2)
    for (const header of headers) {
      expect(header.className).toMatch(/top-\[var\(--page-header-height,0px\)\]/)
    }
    expect(screen.getByText(formatDate(day1.startAt))).toBeInTheDocument()
    expect(screen.getByText(formatDate(day2.startAt))).toBeInTheDocument()
  })

  it('同じ日の番組が続く間は日付ヘッダを 1 回しか出さない', async () => {
    const programs = [program(1, 1, '一つ目'), program(2, 2, '二つ目'), program(3, 3, '三つ目')]
    renderList(programs)

    expect(await screen.findByText('一つ目')).toBeInTheDocument()
    expect(screen.getAllByRole('heading', { level: 2 })).toHaveLength(1)
  })

  it('予約ボタンを押すと actions.reserve が呼ばれる', async () => {
    const reserve = vi.fn()
    const user = userEvent.setup()
    renderList([program(1, 1, '対象番組')], actions({ reserve }))

    await screen.findByText('対象番組')
    await user.click(screen.getByRole('button', { name: '予約' }))

    expect(reserve).toHaveBeenCalledTimes(1)
    expect(reserve.mock.calls[0][0]).toMatchObject({ programId: 1 })
  })

  it('予約済みの番組は取消ボタンになり、押すと actions.cancel が呼ばれる', async () => {
    const cancel = vi.fn()
    const user = userEvent.setup()
    renderList(
      [program(1, 1, '予約済み番組')],
      actions({ cancel, reservedProgramIds: new Set([1]) }),
    )

    await screen.findByText('予約済み番組')
    await user.click(screen.getByRole('button', { name: '取消' }))

    expect(cancel).toHaveBeenCalledWith(1)
  })

  it('番組が 0 件なら行も日付ヘッダも出さない', () => {
    renderList([])
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0)
    expect(screen.queryByRole('button', { name: '予約' })).not.toBeInTheDocument()
  })

  describe('onVisibleDayChange（「いま見ている日」の通知）', () => {
    it('マウント時に先頭の番組の日で呼ばれる（今日の番組なら 0）', async () => {
      const onVisibleDayChange = vi.fn()
      renderList([program(1, 1, '対象番組')], actions(), { onVisibleDayChange, now: dayStart })

      await screen.findByText('対象番組')
      await waitFor(() => expect(onVisibleDayChange).toHaveBeenCalledWith(0))
    })

    it('先頭の番組が翌日なら、その offset（1）で呼ばれる（ハードコードされた 0 ではないことの確認）', async () => {
      const onVisibleDayChange = vi.fn()
      // 25 時間後 = 翌日
      const tomorrowProgram = program(2, 25, '翌日の番組')
      renderList([tomorrowProgram], actions(), { onVisibleDayChange, now: dayStart })

      await screen.findByText('翌日の番組')
      await waitFor(() => expect(onVisibleDayChange).toHaveBeenCalledWith(1))
    })
  })
})
