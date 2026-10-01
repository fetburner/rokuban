import { describe, expect, it } from 'vitest'

import {
  clampCodedRect,
  codedRectFromPoints,
  codedToFrame,
  containsCodedPoint,
  frameImageBox,
  frameImageOffset,
  frameScale,
  frameToCoded,
  moveCodedRect,
  resizeCodedRect,
  savedAreaMatchesFrame,
  type FrameView,
} from '@/lib/cm-logo-frame'

const fullView: FrameView = {
  codedWidth: 1920,
  codedHeight: 1080,
  boxWidth: 640,
  boxHeight: 360,
  sampleAspectRatio: 1,
  zoom: 1,
}

describe('cm-logo-frame', () => {
  it('SAR を掛けた表示の横・縦倍率で座標を往復できる', () => {
    const view: FrameView = {
      codedWidth: 1440,
      codedHeight: 1080,
      boxWidth: 640,
      boxHeight: 360,
      sampleAspectRatio: 4 / 3,
      zoom: 1,
    }
    expect(frameScale(view).x).toBeCloseTo(4 / 9)
    expect(frameScale(view).y).toBeCloseTo(1 / 3)
    expect(frameImageBox(view)).toEqual({ width: 640, height: 360 })
    expect(frameImageOffset(view)).toEqual({ x: 0, y: 0 })

    const coded = { x: 1200, y: 540 }
    const frame = codedToFrame(coded, view)
    expect(frameToCoded(frame, view).x).toBeCloseTo(coded.x)
    expect(frameToCoded(frame, view).y).toBeCloseTo(coded.y)
  })

  it('枠に寄る表示は指定した枠の中心を画面中央へ置く', () => {
    const view: FrameView = {
      ...fullView,
      zoom: 2.5,
      focus: { x: 960, y: 540 },
    }
    const offset = frameImageOffset(view)
    const focus = codedToFrame(view.focus!, view)
    expect(focus.x).toBeCloseTo(view.boxWidth / 2)
    expect(focus.y).toBeCloseTo(view.boxHeight / 2)
    expect(offset.x).toBeCloseTo(-480)
  })

  it('表示枠に収まらないコマも中央へ置く', () => {
    const view: FrameView = { ...fullView, codedWidth: 1440, codedHeight: 1080, zoom: 1 }
    expect(frameImageBox(view)).toEqual({ width: 480, height: 360 })
    expect(frameImageOffset(view)).toEqual({ x: 80, y: 0 })
    expect(frameToCoded({ x: 80, y: 0 }, view)).toEqual({ x: 0, y: 0 })
    expect(frameToCoded({ x: 560, y: 360 }, view).x).toBeCloseTo(1440)
  })

  it('無効な寸法では座標変換を 0 に倒す', () => {
    const invalid = { ...fullView, codedWidth: 0, boxWidth: 0 }
    expect(frameScale(invalid)).toEqual({ x: 0, y: 0 })
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
    expect(clampCodedRect({ x: 100, y: 100, w: 1, h: 1 }, 1920, 1080, 8)).toEqual({
      x: 100,
      y: 100,
      w: 8,
      h: 8,
    })
  })

  it('四隅のリサイズは最小サイズと映像境界を守る', () => {
    const rect = { x: 100, y: 200, w: 300, h: 120 }
	expect(resizeCodedRect(rect, 'nw', { x: 500, y: 400 }, 1920, 1080)).toEqual({
      x: 392,
      y: 312,
      w: 8,
      h: 8,
    })
    expect(resizeCodedRect(rect, 'se', { x: 2000, y: 2000 }, 1920, 1080)).toEqual({
      ...rect,
      w: 1820,
      h: 880,
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
    expect(savedAreaMatchesFrame(area, fullView)).toBe(true)
    expect(savedAreaMatchesFrame({ ...area, codedWidth: 1440 }, fullView)).toBe(false)
    expect(savedAreaMatchesFrame(undefined, fullView)).toBe(false)
  })
})
