import { describe, expect, it, vi } from 'vitest'

import { dayOrigin } from '@/lib/day-offset'

describe('dayOrigin', () => {
  it('null は時・分・秒を切り捨てた「今」になる', () => {
    const now = new Date(2026, 0, 30, 15, 42, 33, 500).getTime()

    const origin = dayOrigin(null, now)

    expect(origin).toEqual(new Date(2026, 0, 30, 15, 0, 0, 0))
  })

  it('0 は「今日」の 0 時になる', () => {
    const now = new Date(2026, 0, 30, 15, 42, 33, 500).getTime()

    const origin = dayOrigin(0, now)

    expect(origin).toEqual(new Date(2026, 0, 30, 0, 0, 0, 0))
  })

  it('数値は N 日先の 0 時になる（月をまたぐ）', () => {
    // 2026-01-30 の 3 日先は 2026-02-02（1 月は 31 日まで）
    const now = new Date(2026, 0, 30, 9, 0, 0, 0).getTime()

    const origin = dayOrigin(3, now)

    expect(origin).toEqual(new Date(2026, 1, 2, 0, 0, 0, 0))
  })

  it('now を省略すると Date.now() を使う', () => {
    const fixed = new Date(2026, 6, 25, 10, 30, 0, 0).getTime()
    vi.spyOn(Date, 'now').mockReturnValue(fixed)

    const origin = dayOrigin(null)

    expect(origin).toEqual(new Date(2026, 6, 25, 10, 0, 0, 0))

    vi.restoreAllMocks()
  })
})
