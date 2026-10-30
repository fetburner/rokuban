import type { IngestProgress, RecordingStatus } from '@/api/generated'

export type RecordingPlaybackSource = 'recording-hls' | 'encoded' | 'none'

export type RecordingPlaybackSourceInput = {
  status: RecordingStatus
  ingestState?: IngestProgress['state']
  hasEncoded: boolean
  hasNonCutEncoded: boolean
  hasHlsPlayback: boolean
  isTrashed?: boolean
}

/** 録画詳細で選ぶ再生元を決める。 */
export function selectRecordingPlaybackSource(
  input: RecordingPlaybackSourceInput,
): RecordingPlaybackSource {
  if (input.isTrashed) return 'none'
  if (input.status !== 'recording' && input.status !== 'finished') return 'none'

  if (
    input.status === 'finished' &&
    (input.ingestState === 'pending' || input.ingestState === 'transferring') &&
    input.hasHlsPlayback
  ) return 'recording-hls'

  // 完了済みで通常版の encoded があれば引き続きそれを既定にする。
  if (input.status === 'finished' && input.hasNonCutEncoded) return 'encoded'

  // 録画中、または ingest 中は世代付き HLS が現在の入力を追う。
  if (input.hasHlsPlayback) return 'recording-hls'

  // hlsPlayback が空の応答では、利用可能なファイル版へ落とす。
  if (input.hasEncoded) return 'encoded'

  // 録画中の HLS URL がまだ用意されていない場合も、配信要求は作らない。
  return 'none'
}
