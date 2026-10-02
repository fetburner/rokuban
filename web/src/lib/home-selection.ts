import type { Recording } from '@/api/generated'
import { isPlayableRecording } from '@/lib/series'

export type HomeHeroChoice = {
  recording: Recording
  kind: 'continue' | 'unwatched'
}

/** 主役選定に使う一覧の状態。`items` は未解決・失敗のとき意味を持たない。 */
export type HeroSource = { pending: boolean; error: boolean; items: readonly Recording[] }

/**
 * 両方の問い合わせが解決するまで主役を決めない（`undefined`）。続きからが空であることを
 * 確認できなければ完了録画へフォールバックしない。続きからに先頭があれば完了側の失敗に
 * 依らずそれを選ぶ。候補が無い・取得失敗は `null`。
 */
export function chooseHomeHero(
  continueWatching: HeroSource,
  finished: HeroSource,
): HomeHeroChoice | null | undefined {
  if (continueWatching.pending || finished.pending) return undefined
  if (continueWatching.error) return null
  const continuation = continueWatching.items[0]
  if (continuation !== undefined) return { recording: continuation, kind: 'continue' }
  if (finished.error) return null

  const recentUnwatched = finished.items
    .filter((recording) => recording.watchedAt === undefined && isPlayableRecording(recording))
    .sort((a, b) => Date.parse(b.startAt) - Date.parse(a.startAt))
  const recording = recentUnwatched[0]
  return recording === undefined ? null : { recording, kind: 'unwatched' }
}

/** 「ほかの新着」は続きからの 2 件目以降、その後に再生できる未視聴の完了録画。 */
export function homeNewArrivals(
  continueWatching: readonly Recording[],
  finished: readonly Recording[],
  heroRecordingId: number,
  limit: number,
): Recording[] {
  const candidates = [
    ...continueWatching.slice(1),
    ...finished
      .filter((recording) => recording.watchedAt === undefined && isPlayableRecording(recording))
      .sort((a, b) => Date.parse(b.startAt) - Date.parse(a.startAt)),
  ]
  const seen = new Set<number>([heroRecordingId])
  const result: Recording[] = []
  for (const recording of candidates) {
    if (seen.has(recording.id)) continue
    seen.add(recording.id)
    result.push(recording)
    if (result.length >= limit) break
  }
  return result
}
