/** 番組表の条件レンズに関する純関数。検索一致の評価自体は検索 API が行う。 */

import type { ProgramSearchMatch, ProgramSearchRequest } from '@/api/generated'
import { canonicalSearchConditions } from '@/lib/program-search'

/** toggleSearchGenre はジャンル条件だけを追加・削除し、他の検索条件をそのまま保つ。 */
export function toggleSearchGenre(
  condition: ProgramSearchRequest | undefined,
  genre: number,
): ProgramSearchRequest | undefined {
  const genres = condition?.genres ?? []
  const nextGenres = genres.includes(genre)
    ? genres.filter((value) => value !== genre)
    : [...genres, genre].sort((a, b) => a - b)
  const next = canonicalSearchConditions({
    ...condition,
    genres: nextGenres.length > 0 ? nextGenres : undefined,
  })
  return Object.keys(next).length > 0 ? next : undefined
}

/**
 * countMatchesByLocalDay は一致番組を開始時刻のローカル暦日ごとに数える。
 *
 * 番組の日付は EPG の開始日を使う。終了が翌日にかかっても別日の番組として
 * 二重計上しない。UTC の `YYYY-MM-DD` 切り出しではなく、端末の暦日境界で割り当てる。
 */
export function countMatchesByLocalDay(
  matches: readonly Pick<ProgramSearchMatch, 'startAt'>[],
  nowMs: number,
  days: number,
): number[] {
  const counts = Array.from({ length: days }, () => 0)
  const firstDay = new Date(nowMs)
  firstDay.setHours(0, 0, 0, 0)
  const indexByDay = new Map<number, number>()

  for (let offset = 0; offset < days; offset++) {
    const day = new Date(firstDay)
    day.setDate(firstDay.getDate() + offset)
    indexByDay.set(day.getTime(), offset)
  }

  for (const match of matches) {
    const start = new Date(match.startAt)
    if (Number.isNaN(start.getTime())) continue
    start.setHours(0, 0, 0, 0)
    const index = indexByDay.get(start.getTime())
    if (index !== undefined) counts[index]++
  }

  return counts
}
