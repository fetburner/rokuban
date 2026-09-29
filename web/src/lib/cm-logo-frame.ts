/**
 * CM の枠を教える画面（`/cm-logos`）の、コマの表示と座標の純関数。
 *
 * 教えた枠は**記録上の画素**（`codedWidth` × `codedHeight`。poster やシーク
 * タイルの座標は SAR を焼き込んでいるので使えない）。画面はコマを表示枠へ
 * 収め、右上を既定で 2.5 倍に拡大する（ロゴは右上にある）。その表示座標から
 * 記録上の座標へ戻すのがここである。
 *
 * **表示枠の寸法は実測でしか取れない**（jsdom の `getBoundingClientRect()` は
 * 常に 0 を返す）ので、配線の判定は `web/e2e/cm-logo-area.mjs` にある。
 * ここは寸法を引数で受ける純関数だけで、単体テストで全部測れる。
 */

/** FRAME_ZOOM は既定の拡大率。右上のロゴを狙う（全体表示 = 1 へ戻せる）。 */
export const FRAME_ZOOM = 2.5

/** MIN_AREA_SIZE はこれより小さい枠を保存させない（不変条件 10 の CHECK に合わせる）。 */
export const MIN_AREA_SIZE = 8

/** CodedRect は記録上の画素で表した枠。 */
export type CodedRect = {
  x: number
  y: number
  w: number
  h: number
}

/** FrameView は表示中のコマの寸法と、それを収める表示枠の寸法。 */
export type FrameView = {
  codedWidth: number
  codedHeight: number
  /** 表示枠の CSS px。 */
  boxWidth: number
  boxHeight: number
  zoom: number
}

/**
 * frameScale は記録上の 1 画素が何 CSS px で描かれるかを返す。
 *
 * 全体表示（zoom = 1）では `object-fit: contain` と同じく表示枠に収まる倍率
 * （縦横の小さい方）。拡大はその倍率に掛ける。
 */
export function frameScale(view: FrameView): number {
  if (view.codedWidth <= 0 || view.codedHeight <= 0) return 0
  const fit = Math.min(view.boxWidth / view.codedWidth, view.boxHeight / view.codedHeight)
  return fit > 0 ? fit * view.zoom : 0
}

/**
 * frameImageBox はコマを描く大きさ（CSS px）を返す。
 *
 * **位置は右上合わせ**（`right: 0` / `top: 0`）。拡大しても右上が表示枠の右上に
 * 残るので、右上にあるロゴを見失わない。全体表示でも枠より小さければ右寄せに
 * なるだけである。
 */
export function frameImageBox(view: FrameView): { width: number; height: number } {
  const scale = frameScale(view)
  return { width: view.codedWidth * scale, height: view.codedHeight * scale }
}

/**
 * frameToCoded は表示枠の中の 1 点（枠の左上が原点、CSS px）を記録上の画素へ写す。
 *
 * **丸めない。** 描いている間は連続値のまま扱い、保存の直前にだけ
 * `clampCodedRect` で整数へ丸める（動かすたびに丸めると、拡大表示では
 * 1 画素が 2 px 以上あるので引っかかる）。
 */
export function frameToCoded(
  point: { x: number; y: number },
  view: FrameView,
): { x: number; y: number } {
  const scale = frameScale(view)
  if (scale <= 0) return { x: 0, y: 0 }
  return {
    x: view.codedWidth - (view.boxWidth - point.x) / scale,
    y: point.y / scale,
  }
}

/**
 * codedToFrame は記録上の 1 点を表示枠の中の座標（枠の左上が原点、CSS px）へ写す。
 * `frameToCoded` の逆で、枠を描くときに使う。
 */
export function codedToFrame(
  point: { x: number; y: number },
  view: FrameView,
): { x: number; y: number } {
  const scale = frameScale(view)
  if (scale <= 0) return { x: 0, y: 0 }
  return {
    x: view.boxWidth - (view.codedWidth - point.x) * scale,
    y: point.y * scale,
  }
}

/** codedRectFromPoints は 2 点（記録上の座標）を包む矩形を返す（順序は問わない）。 */
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
 * clampCodedRect は矩形をコマの内側の整数の矩形へ寄せる。
 *
 * DB の CHECK（`x >= 0` / `w > 0` / `x + w <= coded_width`）と同じ条件を
 * 画面側でも作る（保存してから 400 を受け取るより、描いた時点で収める）。
 */
export function clampCodedRect(rect: CodedRect, codedWidth: number, codedHeight: number): CodedRect {
  const round = Math.round
  const x = Math.min(Math.max(round(rect.x), 0), Math.max(codedWidth - 1, 0))
  const y = Math.min(Math.max(round(rect.y), 0), Math.max(codedHeight - 1, 0))
  const w = Math.min(Math.max(round(rect.w), 1), codedWidth - x)
  const h = Math.min(Math.max(round(rect.h), 1), codedHeight - y)
  return { x, y, w, h }
}

/** moveCodedRect は矩形を (dx, dy) だけ動かし、コマの内側へ収める。大きさは変えない。 */
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

/** containsCodedPoint は点が矩形の内側か（ドラッグで動かすか、描き直すかの分岐）。 */
export function containsCodedPoint(rect: CodedRect, point: { x: number; y: number }): boolean {
  return (
    point.x >= rect.x && point.x <= rect.x + rect.w && point.y >= rect.y && point.y <= rect.y + rect.h
  )
}

/** savedAreaMatchesFrame は保存済みの枠を今のコマに当ててよいかを返す。 */
export function savedAreaMatchesFrame(
  area: { codedWidth: number; codedHeight: number } | undefined,
  view: Pick<FrameView, 'codedWidth' | 'codedHeight'>,
): boolean {
  return (
    area !== undefined &&
    area.codedWidth === view.codedWidth &&
    area.codedHeight === view.codedHeight
  )
}
