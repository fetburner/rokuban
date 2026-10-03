import { describe, expect, it } from 'vitest'

import {
  SEEK_TILES_COLUMNS,
  SEEK_TILES_DISPLAY_HEIGHT,
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileBackgroundSize,
  seekTilePlacement,
  seekTilesURL,
} from './seek-tiles'

// 形（列数・1 枚の大きさ・間隔）は internal/worker/seek_tiles.go と手で揃える
// 契約なので、リテラルで固定する。実装の定数と比べても何も主張しない。
describe('seek tiles geometry', () => {
  it('10 列・160x90 の 2 倍表示である', () => {
    expect(SEEK_TILES_COLUMNS).toBe(10)
    expect(SEEK_TILES_DISPLAY_WIDTH).toBe(320)
    expect(SEEK_TILES_DISPLAY_HEIGHT).toBe(180)
    expect(seekTileBackgroundSize(320)).toBe('3200px auto')
    expect(seekTileBackgroundSize(144)).toBe('1440px auto')
  })

  it('seekTilesURL は配信 URL を組み立てる', () => {
    expect(seekTilesURL(42)).toBe('/api/media/recordings/42/seek-tiles')
  })
})

describe('seekTilePlacement', () => {
  const at = (seconds: number, band = 1000, pointer = 500) => seekTilePlacement(seconds, band, pointer)

  it('10 秒間隔で位置を出し、列をまたぐと次の行へ移る（幅 320）', () => {
    expect(at(0)).toMatchObject({ x: 0, y: 0, width: 320, height: 180 })
    expect(at(9.9)).toMatchObject({ x: 0, y: 0 })
    expect(at(10)).toMatchObject({ x: -320, y: 0 })
    expect(at(90)).toMatchObject({ x: -2880, y: 0 })
    expect(at(100)).toMatchObject({ x: 0, y: -180 })
    expect(at(600)).toMatchObject({ x: 0, y: -1080 })
  })

  it('幅は帯の 4 割までを 16 の倍数に切り捨て、上限 320（400px 前後 / 800px 以上 / 1280px）', () => {
    expect(at(100, 368)).toMatchObject({ width: 144, height: 81, x: 0, y: -81 })
    expect(at(10, 308)).toMatchObject({ width: 112, height: 63, x: -112 })
    expect(at(0, 568)).toMatchObject({ width: 224, height: 126 })
    expect(at(0, 800)).toMatchObject({ width: 320, height: 180 })
    expect(at(0, 1240)).toMatchObject({ width: 320, height: 180 })
  })

  it('ポインタ中心に置き、帯からはみ出さない', () => {
    expect(at(0, 1000, 500)).toMatchObject({ left: 340 })
    expect(at(0, 1000, 0)).toMatchObject({ left: 0 })
    expect(at(0, 1000, 1000)).toMatchObject({ left: 680 })
    expect(at(0, 368, 368)).toMatchObject({ left: 224 })
  })

  it('上限（3 時間）以降と不正な値は null', () => {
    expect(at(10790)).toMatchObject({ x: -2880, y: -19260 })
    expect(at(10800)).toBeNull()
    expect(at(99999)).toBeNull()
    expect(at(-1)).toBeNull()
    expect(at(Number.NaN)).toBeNull()
    expect(at(Number.POSITIVE_INFINITY)).toBeNull()
  })
})
