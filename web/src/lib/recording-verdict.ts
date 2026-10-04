import type { Recording } from '@/api/generated'
import type { LiveCapability } from '@/lib/capabilities'
import { ingestDisplay } from '@/lib/ingest'
import { selectRecordingPlaybackSource } from '@/lib/recording-playback-source'

/** 録画の状態を混ぜず、「見られるか」だけに答える結論。 */
export type RecordingVerdict = 'recording' | 'failed' | 'viewable' | 'preparing' | 'unavailable'

export type RecordingVerdictInput = {
  recording: Recording
  liveCapability: LiveCapability
  /** ごみ箱では削除日時が結論になるため、結論を出さない。 */
  isTrashed?: boolean
  /** ingestDisplay の停滞判定用。結論では転送中かどうかだけを見る。 */
  nowMs: number
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
  liveCapability,
  isTrashed = false,
  nowMs,
}: RecordingVerdictInput): RecordingVerdict | undefined {
  if (isTrashed) return undefined
  const hasEncoded = (recording.encodedAssets?.length ?? 0) > 0
  const hasNonCutEncoded = recording.encodedAssets?.some((asset) => asset.cut !== true) ?? false
  const hasOriginal = recording.sizeBytes !== undefined
  const playbackSource = selectRecordingPlaybackSource({
    status: recording.status,
    hasEncoded,
    hasNonCutEncoded,
    hasOriginal,
    liveEnabled: liveCapability === 'enabled',
  })

  if (recording.status === 'recording') return 'recording'
  if (recording.status === 'failed') return 'failed'
  if (playbackSource !== 'none') return 'viewable'

  if (recording.status !== 'finished') return 'unavailable'

  // 原本だけの録画は、ライブ capability が未確定だと再生元も未確定。
  if (
    hasOriginal &&
    !hasEncoded &&
    (liveCapability === 'pending' || liveCapability === 'unknown')
  ) {
    return undefined
  }

  const ingest = ingestDisplay(recording, nowMs)
  const ingestInProgress = ingest?.kind === 'pending' || ingest?.kind === 'transferring'
  const encodeInProgress = recording.encodeStatus?.some(
    (status) => status.state === 'queued' || status.state === 'running',
  ) ?? false

  if (ingestInProgress || encodeInProgress) return 'preparing'

  return 'unavailable'
}
