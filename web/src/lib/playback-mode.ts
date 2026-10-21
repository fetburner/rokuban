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

type PlaybackStrategyInput = {
  mode: PlaybackMode
  site: string
  networkId?: number
  serviceId?: number
  recordingId?: number
  program?: LiveProgramInput
  timeline?: ChaseTimelineInput
  durationSeconds?: number
}

/** PlaybackStrategy はモードごとの時間軸・録画シーク・離脱先・セッション起点をまとめる。 */
export type PlaybackStrategy = {
  axis: (hoverSeconds: number | null) => PlaybackTimeline | undefined
  recording?: {
    seekTarget: (axisSeconds: number) => number
  }
  leaveURL: (offsetSeconds: number) => string
  originSeconds: (offsetSeconds: number) => number
}

const identityOriginSeconds = (offsetSeconds: number): number => offsetSeconds

function required<T>(value: T | undefined, name: string): T {
  if (value === undefined) throw new Error(`playbackStrategy requires ${name}`)
  return value
}

/** playbackStrategy はモードごとの時間軸方略を作る。 */
export function playbackStrategy(input: PlaybackStrategyInput): PlaybackStrategy {
  switch (input.mode) {
    case 'live': {
      const networkId = required(input.networkId, 'networkId for live')
      const serviceId = required(input.serviceId, 'serviceId for live')
      return {
        axis: (hoverSeconds) => input.program
          ? liveProgramPlaybackTimeline({ ...input.program, hoverSeconds }) ?? undefined
          : undefined,
        leaveURL: () => liveLeaveURL(input.site, networkId, serviceId),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'chase': {
      const recordingId = required(input.recordingId, 'recordingId for chase')
      const timeline = required(input.timeline, 'timeline for chase')
      return {
        axis: (hoverSeconds) => chasePlaybackTimeline(timeline, hoverSeconds),
        recording: {
          seekTarget: (axisSeconds) => {
            const { headSeconds, liveEdgeSeconds } = chasePlaybackTimeline(timeline, null)
            return Math.round(Math.max(0, Math.min(liveEdgeSeconds, axisSeconds) - headSeconds))
          },
        },
        leaveURL: (offsetSeconds) => chaseLeaveURL(input.site, recordingId, offsetSeconds),
        originSeconds: identityOriginSeconds,
      }
    }
    case 'original-vod': {
      const recordingId = required(input.recordingId, 'recordingId for original-vod')
      const durationSeconds = required(input.durationSeconds, 'durationSeconds for original-vod')
      return {
        axis: () => fixedPlaybackTimeline(durationSeconds),
        recording: {
          seekTarget: (axisSeconds) => Math.max(0, Math.min(durationSeconds, axisSeconds)),
        },
        leaveURL: (offsetSeconds) => originalVODLeaveURL(input.site, recordingId, offsetSeconds),
        originSeconds: originalVODSessionOriginSeconds,
      }
    }
  }
}
