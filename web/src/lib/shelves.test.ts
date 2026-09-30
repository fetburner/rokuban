import { describe, expect, it } from 'vitest'

import type { RecordingShelf } from '@/api/generated'
import { buildShelfRows, shelfInputError } from './shelves'

function shelf(value: string | null, title: string, count: number, id = 1): RecordingShelf {
  return value === null
    ? { title, count, representativeId: id }
    : { value, title, count, representativeId: id }
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

describe('shelfInputError', () => {
  it('キーワードと棚のキーの両方を要求する', () => {
    expect(shelfInputError('', '作品X')).toBeDefined()
    expect(shelfInputError('作品X', '')).toBeDefined()
    expect(shelfInputError('   ', '作品X')).toBeDefined()
    expect(shelfInputError('作品X', '作品X')).toBeUndefined()
  })
})
