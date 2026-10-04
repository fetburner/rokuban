import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'

import { countMatchesByLocalDay, toggleSearchGenre } from '@/lib/programs-condition'

// local 暦日の境界を検証するため、この suite は Node の既定 timezone に依存させない。
beforeAll(() => {
  vi.stubEnv('TZ', 'Asia/Tokyo')
})
afterAll(() => {
  vi.unstubAllEnvs()
})

describe('toggleSearchGenre', () => {
  it('genres を追加・削除し、他の検索条件を保つ', () => {
    const original = { textMatches: [{ target: 'name' as const, mode: 'keyword' as const, value: 'ニュース' }] }
    expect(toggleSearchGenre(original, 7)).toEqual({
      textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
      genres: [7],
    })
    expect(toggleSearchGenre({ ...original, genres: [1, 7] }, 7)).toEqual({
      ...original,
      genres: [1],
    })
  })

  it('最後の genre を外すと、他の条件が無ければ undefined に戻る', () => {
    expect(toggleSearchGenre({ genres: [7] }, 7)).toBeUndefined()
  })
})

describe('countMatchesByLocalDay', () => {
  it('開始時刻を端末の暦日で数え、日付境界をまたぐ番組も開始日の 1 件にする', () => {
    const now = new Date(2026, 7, 12, 21, 34).getTime()
    const matches = [
      { startAt: new Date(2026, 7, 12, 23, 40).toISOString() },
      // JST では 8/13 00:10、UTC では 8/12。UTC 日付で分類すると誤る。
      { startAt: '2026-08-12T15:10:00.000Z' },
    ]
    expect(countMatchesByLocalDay(matches, now, 3).slice(0, 3)).toEqual([1, 1, 0])
  })

  it('期間外と不正な開始時刻を無視する', () => {
    const now = new Date(2026, 7, 12, 12).getTime()
    expect(
      countMatchesByLocalDay(
        [{ startAt: '2026-08-11T12:00:00.000Z' }, { startAt: 'invalid' }],
        now,
        2,
      ),
    ).toEqual([0, 0])
  })
})
