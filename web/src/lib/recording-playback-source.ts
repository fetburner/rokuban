import type { RecordingStatus } from '@/api/generated'

export type RecordingPlaybackSource = 'chase' | 'original-vod' | 'encoded' | 'none'

export type RecordingPlaybackSourceInput = {
  status: RecordingStatus
  hasEncoded: boolean
  hasOriginal: boolean
  liveEnabled: boolean
  isTrashed?: boolean
}

export type RecordingPlaybackTransitionTrigger =
  | 'opened'
  | 'source-range-exit'
  | 'ended'
  | 'source-error'
  | 'reopened'
  | 'recording-updated'

export type RecordingPlaybackTransitionInput = {
  currentSource?: RecordingPlaybackSource
  currentPositionSeconds?: number
  trigger: RecordingPlaybackTransitionTrigger
  recording: RecordingPlaybackSourceInput
}

export type RecordingPlaybackTransition = {
  kind: 'keep-current' | 'reselect'
  source: RecordingPlaybackSource
  /** 原本時間軸の秒。encoded の keepRanges 変換は呼び出し側で行う。 */
  positionSeconds?: number
}

/**
 * 録画詳細で選ぶ再生元を決める。
 * これは初期選択と、許可された再選択イベントで使う純関数。
 */
export function selectRecordingPlaybackSource(
  input: RecordingPlaybackSourceInput,
): RecordingPlaybackSource {
  if (input.isTrashed) return 'none'
  if (input.status === 'recording') return input.liveEnabled ? 'chase' : 'none'
  if (input.status !== 'finished') return 'none'
  if (input.hasEncoded) return 'encoded'
  if (input.hasOriginal && input.liveEnabled) return 'original-vod'
  return 'none'
}

/**
 * 再選択イベントと録画状態から次の再生元を決める。
 * 録画状態の更新だけでは現在のプレイヤーと位置を維持する。
 */
export function transitionRecordingPlaybackSource(
  input: RecordingPlaybackTransitionInput,
): RecordingPlaybackTransition {
  const carriedPosition = input.currentPositionSeconds === undefined
    ? {}
    : { positionSeconds: input.currentPositionSeconds }

  if (input.trigger === 'recording-updated' && input.currentSource !== undefined) {
    return {
      kind: 'keep-current',
      source: input.currentSource,
      ...carriedPosition,
    }
  }

  return {
    kind: 'reselect',
    source: selectRecordingPlaybackSource(input.recording),
    ...carriedPosition,
  }
}
