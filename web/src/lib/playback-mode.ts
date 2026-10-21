import {
  chaseLeaveURL,
  liveLeaveURL,
  originalVODLeaveURL,
  originalVODSessionOriginSeconds,
} from '@/lib/live'
import {
  chasePlaybackTimeline,
  fixedPlaybackTimeline,
  liveProgramPlaybackTimeline,
} from '@/lib/playback-timeline'
import type { PlaybackTimeline } from '@/lib/playback-timeline'
import type { RecordingTimeline } from '@/lib/recording-timeline'

/** PlaybackMode は LivePlayer が切り替える再生経路を表す。 */
export type PlaybackMode = 'live' | 'chase' | 'original-vod'

type LiveProgramInput = Omit<Parameters<typeof liveProgramPlaybackTimeline>[0], 'hoverSeconds'>
type ChaseTimelineInput = Pick<RecordingTimeline, 'chaseHeadOffsetSeconds' | 'plannedSeconds' | 'recordedSeconds'>

/**
 * PlaybackStrategyInput は LivePlayer の props のうち strategy が読む欄を、mode で判別できる形のまま持つ。
 * 欄の取り違えはコンパイル時に落ちる。
 */
export type PlaybackStrategyInput =
  | {
      mode: 'live'
      site: string
      networkId: number
      serviceId: number
      liveProgram?: LiveProgramInput
    }
  | {
      mode: 'chase'
      site: string
      recordingId: number
      chaseTimeline: ChaseTimelineInput
    }
  | {
      mode: 'original-vod'
      site: string
      recordingId: number
      recordingDurationMs: number
    }

/** recordingDurationSeconds は原本の実尺（ミリ秒）を秒へ直す。不正な値は 0（シーク不可）にする。 */
export function recordingDurationSeconds(durationMs: number | undefined): number {
  return durationMs !== undefined && Number.isFinite(durationMs) && durationMs > 0 ? durationMs / 1000 : 0
}

/** PlaybackStrategy はモードごとの時間軸・録画シーク・離脱先・セッション起点をまとめる。 */
export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  /** 録画再生の strategy だけが持つ。seekTarget はシークできないとき undefined を返す。 */
  recording?: {
    seekTarget: (axisSeconds: number) => number | undefined
  }
  leaveURL: (offsetSeconds: number) => string
  originSeconds: (offsetSeconds: number) => number
}

const identityOriginSeconds = (offsetSeconds: number): number => offsetSeconds

/** playbackStrategy はモードごとの時間軸の strategy を作る。 */
export function playbackStrategy(input: PlaybackStrategyInput): PlaybackStrategy {
  switch (input.mode) {
    case 'live': {
      const { site, networkId, serviceId, liveProgram } = input
      return {
        axis: (hoverSeconds) => liveProgram
          ? liveProgramPlaybackTimeline({ ...liveProgram, hoverSeconds }) ?? undefined
          : undefined,
        leaveURL: () => liveLeaveURL(site, networkId, serviceId),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'chase': {
      const { site, recordingId, chaseTimeline } = input
      return {
        axis: (hoverSeconds) => chasePlaybackTimeline(chaseTimeline, hoverSeconds),
        recording: {
          seekTarget: (axisSeconds) => {
            const { headSeconds, liveEdgeSeconds } = chasePlaybackTimeline(chaseTimeline, null)
            return Math.round(Math.max(0, Math.min(liveEdgeSeconds, axisSeconds) - headSeconds))
          },
        },
        leaveURL: (offsetSeconds) => chaseLeaveURL(site, recordingId, offsetSeconds),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'original-vod': {
      const { site, recordingId } = input
      const durationSeconds = recordingDurationSeconds(input.recordingDurationMs)
      return {
        axis: () => fixedPlaybackTimeline(durationSeconds),
        recording: {
          seekTarget: (axisSeconds) =>
            durationSeconds > 0 ? Math.max(0, Math.min(durationSeconds, axisSeconds)) : undefined,
        },
        leaveURL: (offsetSeconds) => originalVODLeaveURL(site, recordingId, offsetSeconds),
        originSeconds: originalVODSessionOriginSeconds,
      }
    }
  }
}
