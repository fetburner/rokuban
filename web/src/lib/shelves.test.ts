import { describe, expect, it } from 'vitest'

import type { RecordingShelf } from '@/api/generated'
import { buildShelfRows, minShelfSize, shelfInputError } from './shelves'

function shelf(value: string | null, title: string, count: number, id = 1): RecordingShelf {
  return value === null
    ? { title, count, representativeId: id }
    : { value, title, count, representativeId: id }
}

describe('buildShelfRows', () => {
  it('閾値以上の棚を件数の順のまま並べる', () => {
    const rows = buildShelfRows([
      shelf('NHK高校講座', 'NHK高校講座　日本史　第1回', 120),
      shelf('作品X', 'アニメ　作品X　第2話', minShelfSize),
    ])

    expect(rows.map((row) => row.value)).toEqual(['NHK高校講座', '作品X'])
    expect(rows[0].count).toBe(120)
    expect(rows[0].isOther).toBe(false)
  })

  it('閾値未満の棚と値の無い棚をその他にまとめる', () => {
    const rows = buildShelfRows([
      shelf('作品X', 'アニメ　作品X　第1話', 10, 7),
      shelf('単発', '単発の特番', minShelfSize - 1, 8),
      shelf(null, '【特集】', 3, 9),
    ])

    expect(rows).toHaveLength(2)
    const other = rows[1]
    expect(other.isOther).toBe(true)
    expect(other.value).toBeNull()
    // まとめた件数は元の行の合計（4 + 3）。
    expect(other.count).toBe(minShelfSize - 1 + 3)
    // 代表はまとめた中で最も件数の多い棚。
    expect(other.representativeId).toBe(8)
  })

  it('その他が空なら行を作らない', () => {
    const rows = buildShelfRows([shelf('作品X', 'アニメ　作品X', minShelfSize)])
    expect(rows.map((row) => row.isOther)).toEqual([false])
  })

  it('棚が無ければ空を返す', () => {
    expect(buildShelfRows([])).toEqual([])
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
