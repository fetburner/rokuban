import { describe, expect, it } from 'vitest'

import { formatTimelineTime } from '@/lib/format'

describe('formatTimelineTime', () => {
  it.each([
    [0, '0:00'],
    [59, '0:59'],
    [60, '1:00'],
    [3_600, '60:00'],
    [4_212, '70:12'],
    [-30, '-0:30'],
  ])('formats %i seconds as %s', (seconds, expected) => {
    expect(formatTimelineTime(seconds)).toBe(expected)
  })
})
