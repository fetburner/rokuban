import { describe, expect, it } from 'vitest'

import {
  clampCodedRect,
  codedRectFromPoints,
  codedToFrame,
  containsCodedPoint,
  frameImageBox,
  frameScale,
  frameToCoded,
  moveCodedRect,
  savedAreaMatchesFrame,
  type FrameView,
} from '@/lib/cm-logo-frame'

const zoomedView: FrameView = {
  codedWidth: 1920,
  codedHeight: 1080,
  boxWidth: 640,
  boxHeight: 360,
  zoom: 2.5,
}

describe('cm-logo-frame', () => {
  it('記録上の座標と表示座標を往復できる（右上拡大を含む）', () => {
    expect(frameScale(zoomedView)).toBeCloseTo(5 / 6)
    expect(frameImageBox(zoomedView).width).toBeCloseTo(1600)
    expect(frameImageBox(zoomedView).height).toBeCloseTo(900)

    const coded = { x: 1500, y: 120 }
    const frame = codedToFrame(coded, zoomedView)
    expect(frameToCoded(frame, zoomedView).x).toBeCloseTo(coded.x)
    expect(frameToCoded(frame, zoomedView).y).toBeCloseTo(coded.y)
  })

  it('無効な寸法では座標変換を 0 に倒す', () => {
    const invalid = { ...zoomedView, codedWidth: 0, boxWidth: 0 }
    expect(frameScale(invalid)).toBe(0)
    expect(frameImageBox(invalid)).toEqual({ width: 0, height: 0 })
    expect(frameToCoded({ x: 10, y: 20 }, invalid)).toEqual({ x: 0, y: 0 })
    expect(codedToFrame({ x: 10, y: 20 }, invalid)).toEqual({ x: 0, y: 0 })
  })

  it('2 点の順序によらず矩形を作り、枠内判定をする', () => {
    const rect = codedRectFromPoints({ x: 300, y: 200 }, { x: 100, y: 80 })
    expect(rect).toEqual({ x: 100, y: 80, w: 200, h: 120 })
    expect(containsCodedPoint(rect, { x: 100, y: 80 })).toBe(true)
    expect(containsCodedPoint(rect, { x: 301, y: 200 })).toBe(false)
  })

  it('保存前に矩形を整数かつ映像内へ収める', () => {
    expect(clampCodedRect({ x: -3, y: 1075, w: 100, h: 20 }, 1920, 1080)).toEqual({
      x: 0,
      y: 1075,
      w: 100,
      h: 5,
    })
    expect(clampCodedRect({ x: 1919.6, y: 1079.6, w: 0.2, h: 0.2 }, 1920, 1080)).toEqual({
      x: 1919,
      y: 1079,
      w: 1,
      h: 1,
    })
  })

  it('動かした矩形を上下左右の境界に収める', () => {
    const rect = { x: 100, y: 200, w: 300, h: 120 }
    expect(moveCodedRect(rect, -200, -300, 1920, 1080)).toEqual({ ...rect, x: 0, y: 0 })
    expect(moveCodedRect(rect, 2000, 2000, 1920, 1080)).toEqual({
      ...rect,
      x: 1620,
      y: 960,
    })
  })

  it('保存済みの枠は同じ記録上の解像度にだけ当てる', () => {
    const area = { codedWidth: 1920, codedHeight: 1080 }
    expect(savedAreaMatchesFrame(area, zoomedView)).toBe(true)
    expect(savedAreaMatchesFrame({ ...area, codedWidth: 1440 }, zoomedView)).toBe(false)
    expect(savedAreaMatchesFrame(undefined, zoomedView)).toBe(false)
  })
})
