import { describe, expect, it } from 'vitest'

import type { RecordingShelf } from '@/api/generated'
import { buildShelfRows, shelfInputError, sortShelfRows } from './shelves'

function shelf(
  value: string | null,
  title: string,
  count: number,
  id = 1,
  playableCount = count,
  latestStartAt = '2026-01-01T00:00:00Z',
): RecordingShelf {
  return value === null
    ? { title, count, playableCount, unwatchedCount: 0, latestStartAt, representativeId: id }
    : { value, title, count, playableCount, unwatchedCount: 0, latestStartAt, representativeId: id }
}

describe('buildShelfRows', () => {
  it('値のある棚を件数の順のまま並べる', () => {
    const rows = buildShelfRows([
      shelf('NHK高校講座', 'NHK高校講座　日本史　第1回', 120),
      shelf('作品X', 'アニメ　作品X　第2話', 1),
    ])

    expect(rows.map((row) => row.value)).toEqual(['NHK高校講座', '作品X'])
    expect(rows[0].count).toBe(120)
  })

  it('値の無い棚だけを除外する', () => {
    const rows = buildShelfRows([
      shelf('作品X', 'アニメ　作品X　第1話', 10, 7),
      shelf('単発', '単発の特番', 1, 8),
      shelf(null, '【特集】', 3, 9),
    ])

    expect(rows.map((row) => row.value)).toEqual(['作品X', '単発'])
  })

  it('棚が無ければ空を返す', () => {
    expect(buildShelfRows([])).toEqual([])
  })
})

describe('sortShelfRows', () => {
  const rows = (...latest: string[]) =>
    buildShelfRows(latest.map((at, i) => shelf(`s${i}`, `t${i}`, 1, i + 1, 1, at)))

  it('新着順は小数秒の有無・オフセットが混ざっても時刻の降順にする', () => {
    // 文字列比較だと '.500Z' < 'Z'、'+09:00' の 10:00 > 05:00Z になり逆転する。
    const sorted = sortShelfRows(
      rows(
        '2026-01-01T00:00:00Z', // s0: 00:00:00.000Z
        '2026-01-01T00:00:00.500Z', // s1: 0.5 秒遅い
        '2026-01-01T10:00:00+09:00', // s2: 01:00Z
        '2025-12-31T23:59:59.999Z', // s3: 最古
      ),
      'latest',
    )
    expect(sorted.map((row) => row.value)).toEqual(['s2', 's1', 's0', 's3'])
  })

  it('入力を変更しない', () => {
    const input = rows('2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')
    sortShelfRows(input, 'latest')
    expect(input.map((row) => row.value)).toEqual(['s0', 's1'])
  })
})

describe('shelfInputError', () => {
  it('キーワードと棚のキーの両方を要求する', () => {
    expect(shelfInputError('', '作品X')).toBeDefined()
    expect(shelfInputError('作品X', '')).toBeDefined()
    expect(shelfInputError('   ', '作品X')).toBeDefined()
    expect(shelfInputError('作品X', '作品X')).toBeUndefined()
  })
})
