/**
 * CM の枠を教える画面の、コマ表示と座標変換の純関数。
 *
 * 枠は記録上の coded size の画素で保存する。表示側は sample aspect ratio
 * （SAR）を掛けた表示比で描くので、CSS 上の横倍率と縦倍率は同じとは限らない。
 * 表示枠の寸法は実ブラウザでしか測れないため、ここは寸法を引数に取る純関数に
 * して、座標・四隅の計算を単体テストで固定する。
 */

/** FRAME_ZOOM は「枠に寄る」の拡大率。全体表示は 1 倍。 */
export const FRAME_ZOOM = 2.5

/** MIN_AREA_SIZE は保存する枠の UI 上の最小幅・高さ。 */
export const MIN_AREA_SIZE = 8

/** CodedRect は記録上の画素で表した枠。 */
export type CodedRect = {
  x: number
  y: number
  w: number
  h: number
}

/** FrameView はコマの寸法、SAR、表示枠の寸法、ズーム状態。 */
export type FrameView = {
  codedWidth: number
  codedHeight: number
  /** 表示枠の CSS px。 */
  boxWidth: number
  boxHeight: number
  /** `sample_aspect_ratio` の width / height。無効値は 1 として扱う。 */
  sampleAspectRatio?: number
  zoom: number
  /** 枠に寄る表示で画面中央へ置く記録上の焦点。 */
  focus?: { x: number; y: number }
}

/** FrameScale は記録上の 1 画素が何 CSS px で描かれるか（横・縦別）。 */
export type FrameScale = { x: number; y: number }

/** 有効な SAR を返す。ffprobe の欠落・壊れた値は正方画素へ倒す。 */
function sampleAspectRatio(view: FrameView): number {
  return view.sampleAspectRatio !== undefined && Number.isFinite(view.sampleAspectRatio) && view.sampleAspectRatio > 0
    ? view.sampleAspectRatio
    : 1
}

/** frameScale は SAR と contain の結果を横・縦別に返す。 */
export function frameScale(view: FrameView): FrameScale {
  if (
    view.codedWidth <= 0 ||
    view.codedHeight <= 0 ||
    view.boxWidth <= 0 ||
    view.boxHeight <= 0 ||
    view.zoom <= 0
  ) {
    return { x: 0, y: 0 }
  }
  const sar = sampleAspectRatio(view)
  const displayWidth = view.codedWidth * sar
  const fit = Math.min(view.boxWidth / displayWidth, view.boxHeight / view.codedHeight)
  if (!Number.isFinite(fit) || fit <= 0) return { x: 0, y: 0 }
  return { x: fit * sar * view.zoom, y: fit * view.zoom }
}

/** frameImageBox は SAR を掛けたコマの CSS 上の大きさを返す。 */
export function frameImageBox(view: FrameView): { width: number; height: number } {
  const scale = frameScale(view)
  return { width: view.codedWidth * scale.x, height: view.codedHeight * scale.y }
}

/**
 * frameImageOffset はコマの左上位置を返す。
 *
 * 全体表示は表示枠の中央へ置く。ズーム時は focus を表示枠の中央へ置き、
 * 画像の外が見える位置へは移動させない。
 */
export function frameImageOffset(view: FrameView): { x: number; y: number } {
  const box = frameImageBox(view)
  if (box.width <= 0 || box.height <= 0 || view.boxWidth <= 0 || view.boxHeight <= 0) {
    return { x: 0, y: 0 }
  }
  const centered = {
    x: (view.boxWidth - box.width) / 2,
    y: (view.boxHeight - box.height) / 2,
  }
  if (view.zoom <= 1 || view.focus === undefined) return centered

  const scale = frameScale(view)
  const wanted = {
    x: view.boxWidth / 2 - view.focus.x * scale.x,
    y: view.boxHeight / 2 - view.focus.y * scale.y,
  }
  return {
    x: Math.min(Math.max(wanted.x, Math.min(view.boxWidth - box.width, 0)), Math.max(0, view.boxWidth - box.width)),
    y: Math.min(Math.max(wanted.y, Math.min(view.boxHeight - box.height, 0)), Math.max(0, view.boxHeight - box.height)),
  }
}

/** frameToCoded は表示枠の CSS 座標を記録上の座標へ写す。 */
export function frameToCoded(
  point: { x: number; y: number },
  view: FrameView,
): { x: number; y: number } {
  const scale = frameScale(view)
  if (scale.x <= 0 || scale.y <= 0) return { x: 0, y: 0 }
  const offset = frameImageOffset(view)
  return {
    x: (point.x - offset.x) / scale.x,
    y: (point.y - offset.y) / scale.y,
  }
}

/** codedToFrame は記録上の座標を表示枠の CSS 座標へ写す。 */
export function codedToFrame(
  point: { x: number; y: number },
  view: FrameView,
): { x: number; y: number } {
  const scale = frameScale(view)
  if (scale.x <= 0 || scale.y <= 0) return { x: 0, y: 0 }
  const offset = frameImageOffset(view)
  return {
    x: offset.x + point.x * scale.x,
    y: offset.y + point.y * scale.y,
  }
}

/** codedRectFromPoints は 2 点（記録上の座標）を包む矩形を返す。 */
export function codedRectFromPoints(
  a: { x: number; y: number },
  b: { x: number; y: number },
): CodedRect {
  return {
    x: Math.min(a.x, b.x),
    y: Math.min(a.y, b.y),
    w: Math.abs(a.x - b.x),
    h: Math.abs(a.y - b.y),
  }
}

/**
 * clampCodedRect は矩形を映像内の整数へ寄せる。
 * minSize を指定したときは、指定サイズを下限にして入力値も同じ規則にする。
 */
export function clampCodedRect(
  rect: CodedRect,
  codedWidth: number,
  codedHeight: number,
  minSize = 1,
): CodedRect {
  const round = Math.round
  const minW = Math.min(Math.max(round(minSize), 1), Math.max(codedWidth, 1))
  const minH = Math.min(Math.max(round(minSize), 1), Math.max(codedHeight, 1))
  const x = Math.min(Math.max(round(rect.x), 0), Math.max(codedWidth - minW, 0))
  const y = Math.min(Math.max(round(rect.y), 0), Math.max(codedHeight - minH, 0))
  const w = Math.min(Math.max(round(rect.w), minW), Math.max(codedWidth - x, minW))
  const h = Math.min(Math.max(round(rect.h), minH), Math.max(codedHeight - y, minH))
  return { x, y, w, h }
}

/** moveCodedRect は矩形を動かし、映像の内側へ収める。 */
export function moveCodedRect(
  rect: CodedRect,
  dx: number,
  dy: number,
  codedWidth: number,
  codedHeight: number,
): CodedRect {
  return {
    x: Math.min(Math.max(rect.x + dx, 0), Math.max(codedWidth - rect.w, 0)),
    y: Math.min(Math.max(rect.y + dy, 0), Math.max(codedHeight - rect.h, 0)),
    w: rect.w,
    h: rect.h,
  }
}

/** 四隅のリサイズ方向。 */
export type ResizeHandle = 'nw' | 'ne' | 'sw' | 'se'

/**
 * resizeCodedRect は指定した角を点へ寄せて矩形を変形する。
 * 最小サイズと映像境界を同時に守るので、マウスでも数値入力でも使える。
 */
export function resizeCodedRect(
  rect: CodedRect,
  handle: ResizeHandle,
  point: { x: number; y: number },
  codedWidth: number,
  codedHeight: number,
  minSize = MIN_AREA_SIZE,
): CodedRect {
  const minW = Math.min(Math.max(minSize, 1), codedWidth)
  const minH = Math.min(Math.max(minSize, 1), codedHeight)
  const fixedX = handle.includes('w') ? rect.x + rect.w : rect.x
  const fixedY = handle.includes('n') ? rect.y + rect.h : rect.y
  let x = handle.includes('w') ? Math.min(point.x, fixedX - minW) : rect.x
  let y = handle.includes('n') ? Math.min(point.y, fixedY - minH) : rect.y
  let right = handle.includes('e') ? Math.max(point.x, rect.x + minW) : rect.x + rect.w
  let bottom = handle.includes('s') ? Math.max(point.y, rect.y + minH) : rect.y + rect.h
  if (handle.includes('w')) right = fixedX
  if (handle.includes('n')) bottom = fixedY
  x = Math.max(0, Math.min(x, codedWidth - minW))
  y = Math.max(0, Math.min(y, codedHeight - minH))
  right = Math.max(x + minW, Math.min(right, codedWidth))
  bottom = Math.max(y + minH, Math.min(bottom, codedHeight))
  if (handle.includes('w')) x = Math.min(x, right - minW)
  if (handle.includes('n')) y = Math.min(y, bottom - minH)
  return { x, y, w: right - x, h: bottom - y }
}

/** containsCodedPoint は点が矩形の内側か返す。 */
export function containsCodedPoint(rect: CodedRect, point: { x: number; y: number }): boolean {
  return point.x >= rect.x && point.x <= rect.x + rect.w && point.y >= rect.y && point.y <= rect.y + rect.h
}

/** savedAreaMatchesFrame は保存済みの枠を今のコマに当ててよいか返す。 */
export function savedAreaMatchesFrame(
  area: { codedWidth: number; codedHeight: number } | undefined,
  view: Pick<FrameView, 'codedWidth' | 'codedHeight'>,
): boolean {
  return area !== undefined && area.codedWidth === view.codedWidth && area.codedHeight === view.codedHeight
}
