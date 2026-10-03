import { describe, expect, it } from 'vitest'

import {
  defaultFilmstripRangeSeconds,
  filmstripTicks,
  filmstripTileIndices,
  filmstripTimeToX,
  filmstripXToTime,
  minFilmstripRangeSeconds,
} from './chapter-filmstrip'

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

  it('拡大の上限は 1 マスが実画素 160px を超えない範囲（リテラルで固定）', () => {
    expect(minFilmstripRangeSeconds(1600)).toBe(100)
    expect(minFilmstripRangeSeconds(800)).toBe(50)
    expect(minFilmstripRangeSeconds(80)).toBe(10)
    expect(minFilmstripRangeSeconds(0)).toBe(10)
  })

  it('開いたときの範囲は 1 マスが約 76px になり、拡大の上限より短くならない', () => {
    expect(defaultFilmstripRangeSeconds(760)).toBe(100)
    expect(defaultFilmstripRangeSeconds(100)).toBeCloseTo(13.16, 1)
    expect(defaultFilmstripRangeSeconds(0)).toBe(140)
  })

  it('時刻の目盛りはラベルが重ならない刻みで、範囲内の倍数だけを返す', () => {
    expect(filmstripTicks({ startSeconds: 840, endSeconds: 980 }, 400)).toEqual([840, 870, 900, 930, 960])
    expect(filmstripTicks({ startSeconds: 0, endSeconds: 70 }, 350)).toEqual([0, 30, 60])
    expect(filmstripTicks({ startSeconds: 5, endSeconds: 5 }, 350)).toEqual([])
  })
})
