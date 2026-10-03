import { SEEK_TILES_INTERVAL_SECONDS, SEEK_TILES_MAX_TILES, SEEK_TILES_WIDTH } from '@/lib/seek-tiles'

export type FilmstripRange = {
  startSeconds: number
  endSeconds: number
}

/** filmstripTimeToX は表示範囲内の時刻をトラック左端からの px に変換する。 */
export function filmstripTimeToX(seconds: number, range: FilmstripRange, width: number): number {
  if (
    !Number.isFinite(seconds) ||
    !Number.isFinite(range.startSeconds) ||
    !Number.isFinite(range.endSeconds) ||
    !Number.isFinite(width) ||
    range.endSeconds <= range.startSeconds ||
    width <= 0
  ) {
    return 0
  }
  const fraction = (seconds - range.startSeconds) / (range.endSeconds - range.startSeconds)
  return Math.max(0, Math.min(1, fraction)) * width
}

/** filmstripXToTime はトラック上の x を、表示範囲の原本時間（秒）へ変換する。 */
export function filmstripXToTime(x: number, range: FilmstripRange, width: number): number {
  if (
    !Number.isFinite(range.startSeconds) ||
    !Number.isFinite(range.endSeconds) ||
    range.endSeconds <= range.startSeconds
  ) {
    return 0
  }
  if (!Number.isFinite(x) || !Number.isFinite(width) || width <= 0) return range.startSeconds
  const fraction = Math.max(0, Math.min(1, x / width))
  return range.startSeconds + fraction * (range.endSeconds - range.startSeconds)
}

/**
 * filmstripTileIndices は表示範囲と重なる 10 秒格子の tile 番号を返す。
 * 終端は半開区間として扱い、動画長・生成上限を越える番号は返さない。
 */
export function filmstripTileIndices(range: FilmstripRange, durationSeconds: number): number[] {
  if (
    !Number.isFinite(range.startSeconds) ||
    !Number.isFinite(range.endSeconds) ||
    !Number.isFinite(durationSeconds) ||
    durationSeconds <= 0 ||
    range.endSeconds <= range.startSeconds
  ) {
    return []
  }
  const start = Math.max(0, range.startSeconds)
  const end = Math.min(durationSeconds, SEEK_TILES_INTERVAL_SECONDS * SEEK_TILES_MAX_TILES, range.endSeconds)
  if (end <= start) return []

  const first = Math.floor(start / SEEK_TILES_INTERVAL_SECONDS)
  const endExclusive = Math.min(
    SEEK_TILES_MAX_TILES,
    Math.ceil(end / SEEK_TILES_INTERVAL_SECONDS),
  )
  return Array.from({ length: Math.max(0, endExclusive - first) }, (_, offset) => first + offset)
}

/**
 * FILMSTRIP_MAX_CELL_WIDTH は 1 マス（10 秒）の最大の表示幅（CSS px）。
 *
 * **サーバーが作るタイルの実画素は 160px 幅**（`SEEK_TILES_WIDTH`）。マスがこれより広いと
 * 1 枚のタイルが実画素より広く引き伸ばされて、拡大しても情報は増えず粗くなるだけなので、
 * そこで拡大を止める（`minFilmstripRangeSeconds`）。
 */
export const FILMSTRIP_MAX_CELL_WIDTH = SEEK_TILES_WIDTH

/** FILMSTRIP_TARGET_CELL_WIDTH は最初に開くときの 1 マスの目標幅（CSS px）。 */
const FILMSTRIP_TARGET_CELL_WIDTH = 76

/** FILMSTRIP_MIN_LABEL_SPACING は時刻の目盛りラベルどうしの最小の間隔（CSS px）。 */
const FILMSTRIP_MIN_LABEL_SPACING = 64

/**
 * minFilmstripRangeSeconds は拡大の上限（表示範囲の最短の秒数）を返す。
 * 1 マスが `FILMSTRIP_MAX_CELL_WIDTH` を超えない範囲。幅が分からない（0）ときは 1 マスぶん。
 */
export function minFilmstripRangeSeconds(trackWidth: number): number {
  if (!Number.isFinite(trackWidth) || trackWidth <= 0) return SEEK_TILES_INTERVAL_SECONDS
  return Math.max(
    SEEK_TILES_INTERVAL_SECONDS,
    (SEEK_TILES_INTERVAL_SECONDS * trackWidth) / FILMSTRIP_MAX_CELL_WIDTH,
  )
}

/** defaultFilmstripRangeSeconds は開いた直後の表示範囲の長さ。1 マスが目標幅になるようにする。 */
export function defaultFilmstripRangeSeconds(trackWidth: number): number {
  if (!Number.isFinite(trackWidth) || trackWidth <= 0) return 140
  return Math.max(
    minFilmstripRangeSeconds(trackWidth),
    (SEEK_TILES_INTERVAL_SECONDS * trackWidth) / FILMSTRIP_TARGET_CELL_WIDTH,
  )
}

const TICK_STEPS = [1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600]

/** filmstripTicks は時刻ラベルを出す位置（秒）を、ラベルが重ならない刻みで返す。 */
export function filmstripTicks(range: FilmstripRange, width: number): number[] {
  const length = range.endSeconds - range.startSeconds
  if (!(length > 0) || !(width > 0)) return []
  const step =
    TICK_STEPS.find((candidate) => (candidate / length) * width >= FILMSTRIP_MIN_LABEL_SPACING) ??
    TICK_STEPS[TICK_STEPS.length - 1]
  const out: number[] = []
  for (let t = Math.ceil(range.startSeconds / step) * step; t <= range.endSeconds + 1e-9; t += step) out.push(t)
  return out
}
