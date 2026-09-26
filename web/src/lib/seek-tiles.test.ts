import { describe, expect, it } from 'vitest'

import {
  SEEK_TILES_COLUMNS,
  SEEK_TILES_DISPLAY_HEIGHT,
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileAt,
  seekTileBackgroundSize,
  seekTilesURL,
} from './seek-tiles'

// 形（列数・1 枚の大きさ・間隔）は internal/worker/seek_tiles.go と手で揃える
// 契約なので、リテラルで固定する。実装の定数と比べても何も主張しない。
describe('seek tiles geometry', () => {
  it('10 列・160x90 の 2 倍表示である', () => {
    expect(SEEK_TILES_COLUMNS).toBe(10)
    expect(SEEK_TILES_DISPLAY_WIDTH).toBe(320)
    expect(SEEK_TILES_DISPLAY_HEIGHT).toBe(180)
    expect(seekTileBackgroundSize()).toBe('3200px auto')
  })

  it('seekTilesURL は配信 URL を組み立てる', () => {
    expect(seekTilesURL(42)).toBe('/api/media/recordings/42/seek-tiles')
  })
})

describe('seekTileAt', () => {
  it('10 秒間隔で位置を出し、列をまたぐと次の行へ移る', () => {
    // 0s → タイル 0（左上）
    expect(seekTileAt(0)).toEqual({ x: 0, y: 0 })
    // 9.9s → まだタイル 0
    expect(seekTileAt(9.9)).toEqual({ x: 0, y: 0 })
    // 10s → タイル 1（2 列目）
    expect(seekTileAt(10)).toEqual({ x: -320, y: 0 })
    // 90s → タイル 9（最終列）
    expect(seekTileAt(90)).toEqual({ x: -2880, y: 0 })
    // 100s → タイル 10（2 行目の先頭）
    expect(seekTileAt(100)).toEqual({ x: 0, y: -180 })
    // 600s → タイル 60（7 行目の先頭）
    expect(seekTileAt(600)).toEqual({ x: 0, y: -1080 })
  })

  it('上限（3 時間）以降と不正な値は null', () => {
    // 1080 枚ぶん = 10800 秒。その 1 枚手前までは出る。
    expect(seekTileAt(10790)).toEqual({ x: -2880, y: -19260 })
    expect(seekTileAt(10800)).toBeNull()
    expect(seekTileAt(99999)).toBeNull()
    expect(seekTileAt(-1)).toBeNull()
    expect(seekTileAt(Number.NaN)).toBeNull()
    expect(seekTileAt(Number.POSITIVE_INFINITY)).toBeNull()
  })
})
