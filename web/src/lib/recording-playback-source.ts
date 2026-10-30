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

/**
 * isRecordingHlsGenerationGap は、録画 HLS の再生中に hlsPlayback が空になった状態が
 * 世代の作り直しによる一時的な空き（待てば次の世代の URL が届く）かを返す。
 * 録画中、または完了後の ingest が pending / transferring の間だけ待つ価値がある。
 * 失敗・キャンセル・ごみ箱・取り込み済み（committed や行なし）で空なら、次の世代は
 * 来ないのでエラーとして扱う。hlsPlayback が空でなければ false を返す。
 */
export function isRecordingHlsGenerationGap(
  input: Pick<RecordingPlaybackSourceInput, 'status' | 'ingestState' | 'hasHlsPlayback' | 'isTrashed'>,
): boolean {
  if (input.hasHlsPlayback || input.isTrashed) return false
  if (input.status === 'recording') return true
  return input.status === 'finished' &&
    (input.ingestState === 'pending' || input.ingestState === 'transferring')
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
