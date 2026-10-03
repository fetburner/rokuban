import { describe, expect, it } from 'vitest'

import { formatPlaybackTimeMs } from './format'

describe('formatPlaybackTimeMs', () => {
  it('ミリ秒まで出し、1 フレーム (33ms) の差が見える', () => {
    expect(formatPlaybackTimeMs(30)).toBe('0:30.000')
    expect(formatPlaybackTimeMs(30.033)).toBe('0:30.033')
    expect(formatPlaybackTimeMs(900)).toBe('15:00.000')
    expect(formatPlaybackTimeMs(3723.5)).toBe('1:02:03.500')
  })

  it('ミリ秒に丸めてから桁上げする', () => {
    expect(formatPlaybackTimeMs(59.9996)).toBe('1:00.000')
    expect(formatPlaybackTimeMs(Number.NaN)).toBe('0:00.000')
  })
})
