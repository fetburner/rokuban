import { describe, expect, it } from 'vitest'

import { calendarDayDiff, formatPlaybackTime, formatPlaybackTimeMs, formatTimeRange } from './format'

describe('formatPlaybackTime', () => {
  it('分と秒を使い、1 時間以上は時を含める', () => {
    expect(formatPlaybackTime(330)).toBe('5:30')
    expect(formatPlaybackTime(3723)).toBe('1:02:03')
    expect(formatPlaybackTime(3723, false)).toBe('62:03')
    expect(formatPlaybackTime(-30)).toBe('-0:30')
  })
})

describe('formatPlaybackTimeMs', () => {
  it('ミリ秒まで出し、1 フレーム (33ms) の差が見える', () => {
    expect(formatPlaybackTimeMs(30)).toBe('0:30.000')
    expect(formatPlaybackTimeMs(30.033)).toBe('0:30.033')
    expect(formatPlaybackTimeMs(330)).toBe('5:30.000')
    expect(formatPlaybackTimeMs(900)).toBe('15:00.000')
    expect(formatPlaybackTimeMs(3723.5)).toBe('1:02:03.500')
  })

  it('ミリ秒に丸めてから桁上げする', () => {
    expect(formatPlaybackTimeMs(59.9996)).toBe('1:00.000')
    expect(formatPlaybackTimeMs(Number.NaN)).toBe('0:00.000')
  })
})

describe('calendarDayDiff', () => {
  it('23:59 と 0:00 は暦日の境界で分ける', () => {
    const beforeMidnight = new Date(2026, 6, 25, 23, 59).getTime()
    const midnight = new Date(2026, 6, 26, 0, 0).getTime()

    expect(calendarDayDiff(beforeMidnight, new Date(2026, 6, 25, 0, 0).getTime())).toBe(0)
    expect(calendarDayDiff(midnight, beforeMidnight)).toBe(1)
  })

  it('月末から翌月初日の差を 1 日とする', () => {
    const monthEnd = new Date(2026, 6, 31, 23, 59).getTime()
    const nextMonth = new Date(2026, 7, 1, 0, 0).getTime()

    expect(calendarDayDiff(nextMonth, monthEnd)).toBe(1)
  })
})

describe('formatTimeRange', () => {
  it('時刻の種類にかかわらず「〜」で両端をつなぐ', () => {
    expect(formatTimeRange('20:00', '21:00')).toBe('20:00〜21:00')
    expect(formatTimeRange('5:30', '6:00')).toBe('5:30〜6:00')
    expect(formatTimeRange('20:00', undefined)).toBe('20:00〜')
    expect(formatTimeRange(undefined, '21:00')).toBe('〜21:00')
  })
})
