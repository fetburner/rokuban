import { describe, expect, it } from 'vitest'

import { visibleDayOffset } from '@/lib/visible-day'

const now = new Date(2026, 7, 1, 10, 0, 0, 0).getTime() // 2026-08-01 10:00

function programAt(daysFromNow: number, hour = 12): { startAt: string } {
  const d = new Date(2026, 7, 1 + daysFromNow, hour, 0, 0, 0)
  return { startAt: d.toISOString() }
}

describe('visibleDayOffset', () => {
  it('先頭が今日の番組なら 0 を返す', () => {
    const programs = [programAt(0)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
  })

  it('先頭が明日の番組なら 1 を返す', () => {
    const programs = [programAt(1)]
    expect(visibleDayOffset(programs, 0, now)).toBe(1)
  })

  it('日をまたぐ位置で切り替わる（先頭インデックスが変わると offset も変わる）', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
    expect(visibleDayOffset(programs, 1, now)).toBe(1)
    expect(visibleDayOffset(programs, 2, now)).toBe(1)
  })

  it('月をまたぐ番組でも正しい offset になる', () => {
    // 2026-08-01 から 2026-09-01 は 31 日先
    const programs = [{ startAt: new Date(2026, 8, 1, 12, 0, 0, 0).toISOString() }]
    expect(visibleDayOffset(programs, 0, now)).toBe(31)
  })

  it('先頭インデックスが範囲外（空リスト）なら 0 を返す', () => {
    expect(visibleDayOffset([], 0, now)).toBe(0)
    expect(visibleDayOffset([programAt(3)], 5, now)).toBe(0)
  })

  it('now より前の番組（時計のずれ等）が先頭に来ても 0 未満にはならない', () => {
    const programs = [programAt(-2)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
  })
})
