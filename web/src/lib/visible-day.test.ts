import { describe, expect, it } from 'vitest'

import { firstIndexForDayOffset, visibleDayOffset } from '@/lib/visible-day'

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

describe('firstIndexForDayOffset（visibleDayOffset と対になる向き）', () => {
  it('指定した dayOffset の暦日に一致する最初の番組の添字を返す', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1), programAt(2, 5)]
    expect(firstIndexForDayOffset(programs, 1, now)).toBe(1)
    expect(firstIndexForDayOffset(programs, 2, now)).toBe(3)
  })

  it('先頭に一致しても、複数候補があれば「最初」の添字を返す', () => {
    const programs = [programAt(0, 1), programAt(0, 2), programAt(1, 0)]
    expect(firstIndexForDayOffset(programs, 0, now)).toBe(0)
  })

  it('対照: visibleDayOffset で得た offset を firstIndexForDayOffset に渡すと同じ番組の添字に戻る', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1)]
    const offset = visibleDayOffset(programs, 2, now)
    expect(firstIndexForDayOffset(programs, offset, now)).toBe(1)
  })

  it('該当する暦日の番組が 1 件も無ければ null を返す（まだ読み込んでいない日）', () => {
    const programs = [programAt(0), programAt(1)]
    expect(firstIndexForDayOffset(programs, 5, now)).toBeNull()
  })

  it('番組が 0 件でも null を返す', () => {
    expect(firstIndexForDayOffset([], 0, now)).toBeNull()
  })
})
