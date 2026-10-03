/**
 * シークプレビュー（タイル画像）の形と、再生位置 → タイル矩形の純関数。
 *
 * 画像は 1 枚の JPEG で、`SEEK_TILES_COLUMNS` 列の格子に並んでいる
 * （サーバーが ffmpeg の tile フィルタで作る。枚数が列数の倍数でないときの
 * 余りは黒で埋まる）。そのため必要な情報は「列数」と「1 枚の大きさ」だけで、
 * 行数を知らなくても `background-size: <列数×表示幅>px auto` で位置が出せる。
 */

/**
 * **これらの値は internal/worker/seek_tiles.go の同名の定数と同じでなければ
 * ならない。** メディア配信は openapi.yaml の対象外なので値の伝達経路が無く、
 * 手で揃えるしかない（どちらか片方だけ変えると、出るタイルが再生位置とずれる）。
 */
export const SEEK_TILES_INTERVAL_SECONDS = 10
export const SEEK_TILES_WIDTH = 160
export const SEEK_TILES_HEIGHT = 90
export const SEEK_TILES_COLUMNS = 10
export const SEEK_TILES_MAX_TILES = 1080

/** 表示倍率。160x90 のままだと小さすぎるので 2 倍で出す（画素は粗くなる）。 */
const DISPLAY_SCALE = 2

/** プレビュー 1 枚の表示サイズ（CSS px）。 */
export const SEEK_TILES_DISPLAY_WIDTH = SEEK_TILES_WIDTH * DISPLAY_SCALE
export const SEEK_TILES_DISPLAY_HEIGHT = SEEK_TILES_HEIGHT * DISPLAY_SCALE

/** 1 枚のタイルの位置（表示幅を掛けた CSS px のオフセット）。 */
export type SeekTileRect = {
  x: number
  y: number
}

/** seekTileCell は再生位置に対応するタイルの格子上の列と行を返す。範囲外は null。 */
export function seekTileCell(seconds: number): { column: number; row: number } | null {
  if (!Number.isFinite(seconds) || seconds < 0) return null
  const index = Math.floor(seconds / SEEK_TILES_INTERVAL_SECONDS)
  if (index >= SEEK_TILES_MAX_TILES) return null
  return { column: index % SEEK_TILES_COLUMNS, row: Math.floor(index / SEEK_TILES_COLUMNS) }
}

/** seekTilesURL は streamer のタイル配信 URL を組み立てる（OpenAPI 外）。 */
export function seekTilesURL(recordingId: number): string {
  return `/api/media/recordings/${recordingId}/seek-tiles`
}

/** タイルの幅を帯の幅の何割までにするか。狭い窓で映像の大半を覆わないための上限。 */
const MAX_BAND_FRACTION = 0.4

/** タイル幅の刻み。16 の倍数なら 16:9 の高さが整数になり、隣のタイルが滲まない。 */
const WIDTH_STEP = 16

/** seekTileDisplayWidth は帯の幅からタイルの表示幅（16 の倍数、最大 320）を決める。 */
export function seekTileDisplayWidth(bandWidth: number): number {
  const fit = Math.floor((bandWidth * MAX_BAND_FRACTION) / WIDTH_STEP) * WIDTH_STEP
  return Math.max(WIDTH_STEP, Math.min(SEEK_TILES_DISPLAY_WIDTH, fit))
}

/** SeekTilePlacement は 1 枚のプレビューの表示幅・格子上のオフセット・帯内の左端（CSS px）。 */
export type SeekTilePlacement = SeekTileRect & { width: number; height: number; left: number }

/**
 * seekTilePlacement は再生位置と帯の寸法からプレビューの配置を返す（録画・ライブ共通）。
 *
 * 幅は帯の幅の 4 割まで（上限 320px）。ポインタ中心に置き、帯からはみ出さない。
 * 上限（`SEEK_TILES_MAX_TILES`）より後ろの位置や負の位置は null
 * （呼び出し側はプレビューを出さない）。
 */
export function seekTilePlacement(
  seconds: number,
  bandWidth: number,
  pointerOffsetX: number,
): SeekTilePlacement | null {
  const cell = seekTileCell(seconds)
  if (cell === null) return null
  const width = seekTileDisplayWidth(bandWidth)
  const height = (width * SEEK_TILES_HEIGHT) / SEEK_TILES_WIDTH
  return {
    x: cell.column === 0 ? 0 : -cell.column * width,
    y: cell.row === 0 ? 0 : -cell.row * height,
    width,
    height,
    left: Math.max(0, Math.min(bandWidth - width, pointerOffsetX - width / 2)),
  }
}

/**
 * seekTileBackgroundSize は格子画像を 1 枚ぶんの表示幅で割り付けるための
 * `background-size` を返す。高さは auto（画像の実際の行数に従う）なので、
 * クライアントは行数を知らなくてよい。
 */
export function seekTileBackgroundSize(width: number): string {
  return `${SEEK_TILES_COLUMNS * width}px auto`
}
