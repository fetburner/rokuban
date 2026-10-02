import { SEEK_TILES_INTERVAL_SECONDS, SEEK_TILES_MAX_TILES } from '@/lib/seek-tiles'

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
