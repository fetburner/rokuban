import { describe, expect, it } from 'vitest'

import { previousDayWindow } from '@/lib/previous-day-window'

describe('previousDayWindow', () => {
  it('下限に達していないとき、前日 0 時〜当日 0 時（＝現在の先頭窓の開始時刻）を返す', () => {
    const earliestLoadedMs = new Date(2026, 7, 6, 0, 0, 0, 0).getTime() // 8/6 0:00
    const lowerBoundMs = new Date(2026, 7, 1, 9, 0, 0, 0).getTime() // 十分に前

    const result = previousDayWindow(earliestLoadedMs, lowerBoundMs)

    expect(result).toEqual({
      startMs: new Date(2026, 7, 5, 0, 0, 0, 0).getTime(), // 8/5 0:00
      endMs: earliestLoadedMs, // 8/6 0:00
    })
  })

  it('月をまたいでも前日 0 時を正しく計算する', () => {
    const earliestLoadedMs = new Date(2026, 1, 1, 0, 0, 0, 0).getTime() // 2/1 0:00
    const lowerBoundMs = new Date(2026, 0, 1, 0, 0, 0, 0).getTime()

    const result = previousDayWindow(earliestLoadedMs, lowerBoundMs)

    expect(result).toEqual({
      startMs: new Date(2026, 0, 31, 0, 0, 0, 0).getTime(), // 1/31 0:00
      endMs: earliestLoadedMs,
    })
  })

  it('前日 0 時が下限より前になるとき、下限で打ち切る（24 時間に満たない窓を返す）', () => {
    const earliestLoadedMs = new Date(2026, 7, 6, 0, 0, 0, 0).getTime() // 8/6 0:00
    // 下限が前日（8/5）の日中 --- 前日 0 時（8/5 0:00）より後
    const lowerBoundMs = new Date(2026, 7, 5, 14, 0, 0, 0).getTime()

    const result = previousDayWindow(earliestLoadedMs, lowerBoundMs)

    expect(result).toEqual({
      startMs: lowerBoundMs,
      endMs: earliestLoadedMs,
    })
  })

  it('下限に達しているとき（先頭窓の開始時刻が下限と一致）は null を返す', () => {
    const lowerBoundMs = new Date(2026, 7, 6, 14, 0, 0, 0).getTime()

    const result = previousDayWindow(lowerBoundMs, lowerBoundMs)

    expect(result).toBeNull()
  })

  it('先頭窓の開始時刻が下限を下回っている（本来あり得ないが）ときも null を返す', () => {
    const lowerBoundMs = new Date(2026, 7, 6, 14, 0, 0, 0).getTime()
    const earliestLoadedMs = lowerBoundMs - 1

    const result = previousDayWindow(earliestLoadedMs, lowerBoundMs)

    expect(result).toBeNull()
  })
})
