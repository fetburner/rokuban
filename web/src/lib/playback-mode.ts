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

type PlaybackAxisInput =
  | { mode: 'live'; program?: LiveProgramInput; hoverSeconds: number | null }
  | { mode: 'chase'; timeline: ChaseTimelineInput; hoverSeconds: number | null }
  | { mode: 'original-vod'; durationSeconds: number }

/** playbackModeAxis はモードに応じて軸の範囲・印・操作可能範囲を導出する。 */
export function playbackModeAxis(input: PlaybackAxisInput): PlaybackTimeline | undefined {
  switch (input.mode) {
    case 'live':
      return input.program
        ? liveProgramPlaybackTimeline({ ...input.program, hoverSeconds: input.hoverSeconds }) ?? undefined
        : undefined
    case 'chase':
      return chasePlaybackTimeline(input.timeline, input.hoverSeconds)
    case 'original-vod':
      return fixedPlaybackTimeline(input.durationSeconds)
  }
}

/** playbackSessionOriginSeconds は playlist offset を録画軸上の起点へ写す。 */
export function playbackSessionOriginSeconds(mode: PlaybackMode, offsetSeconds: number): number {
  return mode === 'original-vod'
    ? originalVODSessionOriginSeconds(offsetSeconds)
    : offsetSeconds
}

type PlaybackSeekTargetInput =
  | { mode: 'chase'; axisSeconds: number; headSeconds: number; liveEdgeSeconds: number }
  | { mode: 'original-vod'; axisSeconds: number; durationSeconds: number }

/** playbackSeekTarget は時間軸上の秒を、録画再生へ渡すシーク先に丸める。 */
export function playbackSeekTarget(input: PlaybackSeekTargetInput): number {
  switch (input.mode) {
    case 'chase':
      return Math.round(Math.max(0, Math.min(input.liveEdgeSeconds, input.axisSeconds) - input.headSeconds))
    case 'original-vod':
      return Math.max(0, Math.min(input.durationSeconds, input.axisSeconds))
  }
}

type PlaybackLeaveTarget =
  | { mode: 'live'; site: string; networkId: number; serviceId: number }
  | { mode: 'chase'; site: string; recordingId: number }
  | { mode: 'original-vod'; site: string; recordingId: number }

/** playbackLeaveURL はモードと現在のセッションから離脱ヒントの URL を導出する。 */
export function playbackLeaveURL(target: PlaybackLeaveTarget, offsetSeconds = 0): string {
  switch (target.mode) {
    case 'live':
      return liveLeaveURL(target.site, target.networkId, target.serviceId)
    case 'chase':
      return chaseLeaveURL(target.site, target.recordingId, offsetSeconds)
    case 'original-vod':
      return originalVODLeaveURL(target.site, target.recordingId, offsetSeconds)
  }
}
