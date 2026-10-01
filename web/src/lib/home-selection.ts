import type { Recording } from '@/api/generated'
import { isPlayableRecording } from '@/lib/series'

export type HomeHeroChoice = {
  recording: Recording
  kind: 'continue' | 'unwatched'
}

/**
 * 両方の問い合わせが解決するまで主役を決めない。`undefined` は待機中、`null` は
 * 両方を確認したうえで候補が無い状態を表す。
 */
export function chooseHomeHero(
  continueWatching: readonly Recording[] | undefined,
  finished: readonly Recording[] | undefined,
): HomeHeroChoice | null | undefined {
  if (continueWatching === undefined || finished === undefined) return undefined
  const continuation = continueWatching[0]
  if (continuation !== undefined) return { recording: continuation, kind: 'continue' }

  const recentUnwatched = finished
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
