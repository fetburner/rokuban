import type { IngestProgress, RecordingStatus } from '@/api/generated'

export type RecordingPlaybackSource = 'recording-hls' | 'chase' | 'original-vod' | 'encoded' | 'none'

export type RecordingPlaybackSourceInput = {
  status: RecordingStatus
  ingestState?: IngestProgress['state']
  hasEncoded: boolean
  hasNonCutEncoded: boolean
  hasOriginal: boolean
  hasHlsPlayback: boolean
  liveEnabled: boolean
  isTrashed?: boolean
}

/**
 * 録画詳細で選ぶ再生元を決める。
 * 初期選択と、範囲外のシーク・終端・エラーでの選び直しが使う純関数。
 * 再生元を替えるかどうかの判定（今と同じなら張り直さない等）は呼び出し側（録画詳細）が持つ。
 */
export function selectRecordingPlaybackSource(
  input: RecordingPlaybackSourceInput,
): RecordingPlaybackSource {
  if (input.isTrashed) return 'none'
  const live = input.hasHlsPlayback ? 'recording-hls' : input.liveEnabled ? 'chase' : 'none'
  if (input.status === 'recording') return live
  if (input.status !== 'finished') return 'none'
  if (input.ingestState === 'pending' || input.ingestState === 'transferring') {
    if (live !== 'none') return live
  }
  if (input.hasNonCutEncoded) return 'encoded'
  if (input.hasOriginal) {
    if (input.hasHlsPlayback) return 'recording-hls'
    if (input.liveEnabled) return 'original-vod'
  }
  if (input.hasEncoded) return 'encoded'
  return 'none'
}
