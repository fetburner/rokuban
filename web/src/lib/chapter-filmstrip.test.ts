import { describe, expect, it } from 'vitest'

import { filmstripTileIndices, filmstripTimeToX, filmstripXToTime } from './chapter-filmstrip'

describe('filmstrip time/position mapping', () => {
  const range = { startSeconds: 20, endSeconds: 60 }

  it('時刻→x は表示範囲を線形に写し、外側は端に留める', () => {
    expect(filmstripTimeToX(20, range, 800)).toBe(0)
    expect(filmstripTimeToX(30, range, 800)).toBe(200)
    expect(filmstripTimeToX(60, range, 800)).toBe(800)
    expect(filmstripTimeToX(70, range, 800)).toBe(800)
  })

  it('x→時刻 は同じ表示範囲へ逆写像し、幅ゼロや外側を扱う', () => {
    expect(filmstripXToTime(200, range, 800)).toBe(30)
    expect(filmstripXToTime(-20, range, 800)).toBe(20)
    expect(filmstripXToTime(900, range, 800)).toBe(60)
    expect(filmstripXToTime(50, range, 0)).toBe(20)
  })

  it('表示範囲に重なる10秒tileだけを半開区間・動画長・生成上限内で返す', () => {
    expect(filmstripTileIndices({ startSeconds: 25, endSeconds: 45 }, 120)).toEqual([2, 3, 4])
    expect(filmstripTileIndices({ startSeconds: 30, endSeconds: 40 }, 120)).toEqual([3])
    expect(filmstripTileIndices({ startSeconds: 10_790, endSeconds: 10_830 }, 20_000)).toEqual([1079])
    expect(filmstripTileIndices({ startSeconds: 10_800, endSeconds: 10_830 }, 20_000)).toEqual([])
    expect(filmstripTileIndices({ startSeconds: 10, endSeconds: 10 }, 120)).toEqual([])
  })
})
