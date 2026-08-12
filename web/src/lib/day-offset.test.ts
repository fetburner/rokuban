import { describe, expect, it, vi } from 'vitest'

import { dayOffsetForMs, dayOrigin } from '@/lib/day-offset'

describe('dayOrigin', () => {
  it('0 は now を時・分・秒で切り捨てた時刻になる（0 時ではない）', () => {
    const now = new Date(2026, 0, 30, 15, 42, 33, 500).getTime()

    const origin = dayOrigin(0, now)

    expect(origin).toEqual(new Date(2026, 0, 30, 15, 0, 0, 0))
  })

  it('0 より大きい数値は N 日先の 0 時になる（月をまたぐ）', () => {
    // 2026-01-30 の 3 日先は 2026-02-02（1 月は 31 日まで）
    const now = new Date(2026, 0, 30, 9, 0, 0, 0).getTime()

    const origin = dayOrigin(3, now)

    expect(origin).toEqual(new Date(2026, 1, 2, 0, 0, 0, 0))
  })

  it('0 より大きい数値は 0 時に揃う（月末をまたいで 1 日先でも）', () => {
    // 2026-01-31 の 1 日先は 2026-02-01
    const now = new Date(2026, 0, 31, 23, 50, 0, 0).getTime()

    const origin = dayOrigin(1, now)

    expect(origin).toEqual(new Date(2026, 1, 1, 0, 0, 0, 0))
  })

  it('now を省略すると Date.now() を使う', () => {
    const fixed = new Date(2026, 6, 25, 10, 30, 0, 0).getTime()
    vi.spyOn(Date, 'now').mockReturnValue(fixed)

    const origin = dayOrigin(0)

    expect(origin).toEqual(new Date(2026, 6, 25, 10, 0, 0, 0))

    vi.restoreAllMocks()
  })
})

describe('dayOffsetForMs', () => {
  const now = new Date(2026, 6, 25, 15, 0, 0, 0).getTime()

  it('今日の時刻は 0 になる（未明でも前日に誤判定しない）', () => {
    const earlyMorning = new Date(2026, 6, 25, 0, 30, 0, 0).getTime()
    expect(dayOffsetForMs(earlyMorning, now, 8)).toBe(0)
  })

  it('3 日先の時刻は 3 になる', () => {
    const threeDaysLater = new Date(2026, 6, 28, 21, 0, 0, 0).getTime()
    expect(dayOffsetForMs(threeDaysLater, now, 8)).toBe(3)
  })

  it('過去の時刻は 0 にクランプする', () => {
    const yesterday = new Date(2026, 6, 24, 21, 0, 0, 0).getTime()
    expect(dayOffsetForMs(yesterday, now, 8)).toBe(0)
  })

  it('selectableDays より先の時刻は selectableDays - 1 にクランプする', () => {
    const farFuture = new Date(2026, 7, 20, 0, 0, 0, 0).getTime()
    expect(dayOffsetForMs(farFuture, now, 8)).toBe(7)
  })
})
