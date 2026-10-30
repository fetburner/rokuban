import type { Recording } from '@/api/generated'
import { ingestDisplay } from '@/lib/ingest'
import { selectRecordingPlaybackSource } from '@/lib/recording-playback-source'

/** 録画の状態を混ぜず、「見られるか」だけに答える結論。 */
export type RecordingVerdict = 'recording' | 'failed' | 'viewable' | 'preparing' | 'unavailable'

export type RecordingVerdictInput = {
  recording: Recording
  /** ごみ箱では削除日時が結論になるため、結論を出さない。 */
  isTrashed?: boolean
}

/**
 * recordingVerdict は録画 1 件が見られるかを決める唯一の関数。
 *
 * 再生元は詳細画面と同じ selectRecordingPlaybackSource に任せる。
 * 「要対応」のように視聴可否と内訳の失敗を混ぜる語は行に出さない。
 * 見られる録画のエンコード失敗やドロップは、内訳バッジが destructive で示す。
 *
 * capability が pending / unknown の間は原本だけの録画を再生不可と断定できない。
 * capability を取得できた後に enabled なら視聴可、disabled なら再生不可になる。
 */
export function recordingVerdict({
  recording,
  isTrashed = false,
}: RecordingVerdictInput): RecordingVerdict | undefined {
  if (isTrashed) return undefined
  const hasEncoded = (recording.encodedAssets?.length ?? 0) > 0
  const hasNonCutEncoded = recording.encodedAssets?.some((asset) => asset.cut !== true) ?? false
  const playbackSource = selectRecordingPlaybackSource({
    status: recording.status,
    hasEncoded,
    hasNonCutEncoded,
    hasHlsPlayback: recording.hlsPlayback.length > 0,
  })

  if (recording.status === 'recording') return 'recording'
  if (recording.status === 'failed') return 'failed'
  if (playbackSource !== 'none') return 'viewable'

  if (recording.status !== 'finished') return 'unavailable'

  // 結論は kind（転送中かどうか）だけを見る。停滞（stale）は kind に影響しないので時刻は固定値でよい。
  const ingest = ingestDisplay(recording, 0)
  const ingestInProgress = ingest?.kind === 'pending' || ingest?.kind === 'transferring'
  const encodeInProgress = recording.encodeStatus?.some(
    (status) => status.state === 'queued' || status.state === 'running',
  ) ?? false

  if (ingestInProgress || encodeInProgress) return 'preparing'

  return 'unavailable'
}
