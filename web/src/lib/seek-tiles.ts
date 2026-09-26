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

/** 1 枚のタイルの位置（表示倍率を掛けた CSS px のオフセット）。 */
export type SeekTileRect = {
  x: number
  y: number
}

/** seekTilesURL は streamer のタイル配信 URL を組み立てる（OpenAPI 外）。 */
export function seekTilesURL(recordingId: number): string {
  return `/api/media/recordings/${recordingId}/seek-tiles`
}

/**
 * seekTileAt は再生位置に対応するタイルのオフセットを返す。
 *
 * 上限（`SEEK_TILES_MAX_TILES`）より後ろの位置にはタイルが無いので null を返す
 * （呼び出し側はプレビューを出さない）。負の位置も null。
 */
export function seekTileAt(seconds: number): SeekTileRect | null {
  if (!Number.isFinite(seconds) || seconds < 0) return null
  const index = Math.floor(seconds / SEEK_TILES_INTERVAL_SECONDS)
  if (index >= SEEK_TILES_MAX_TILES) return null
  const column = index % SEEK_TILES_COLUMNS
  const row = Math.floor(index / SEEK_TILES_COLUMNS)
  return {
    x: column === 0 ? 0 : -column * SEEK_TILES_DISPLAY_WIDTH,
    y: row === 0 ? 0 : -row * SEEK_TILES_DISPLAY_HEIGHT,
  }
}

/**
 * seekTileBackgroundSize は格子画像を 1 枚ぶんの表示サイズで割り付けるための
 * `background-size` を返す。高さは auto（画像の実際の行数に従う）なので、
 * クライアントは行数を知らなくてよい。
 */
export function seekTileBackgroundSize(): string {
  return `${SEEK_TILES_COLUMNS * SEEK_TILES_DISPLAY_WIDTH}px auto`
}
