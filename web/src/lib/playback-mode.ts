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
type RecordingPlaybackMode = Exclude<PlaybackMode, 'live'>

type LiveProgramInput = Omit<Parameters<typeof liveProgramPlaybackTimeline>[0], 'hoverSeconds'>
type ChaseTimelineInput = Pick<RecordingTimeline, 'chaseHeadOffsetSeconds' | 'plannedSeconds' | 'recordedSeconds'>

type PlaybackAxisInput =
  | { mode: 'live'; program?: LiveProgramInput; hoverSeconds: number | null }
  | { mode: 'chase'; timeline: ChaseTimelineInput; hoverSeconds: number | null }
  | { mode: 'original-vod'; durationSeconds: number }

type ChaseAxis = Extract<PlaybackTimeline, { kind: 'chase' }>
type LiveProgramAxis = Extract<PlaybackTimeline, { kind: 'live-program' }>

/** PlaybackModeAxis は操作バー用の軸と、モード固有の軸情報をまとめる。 */
export type PlaybackModeAxis = {
  timeline?: PlaybackTimeline
  chase?: ChaseAxis
  liveProgram?: LiveProgramAxis
}

/** playbackModeAxis はモードに応じて軸の範囲・印・操作可能範囲を導出する。 */
export function playbackModeAxis(input: PlaybackAxisInput): PlaybackModeAxis {
  switch (input.mode) {
    case 'live': {
      const liveProgram = input.program
        ? liveProgramPlaybackTimeline({ ...input.program, hoverSeconds: input.hoverSeconds }) ?? undefined
        : undefined
      return { timeline: liveProgram, liveProgram }
    }
    case 'chase': {
      const chase = chasePlaybackTimeline(input.timeline, input.hoverSeconds)
      return { timeline: chase, chase }
    }
    case 'original-vod': {
      return { timeline: fixedPlaybackTimeline(input.durationSeconds) }
    }
  }
}

/** playbackSessionOriginSeconds は playlist offset を録画軸上の起点へ写す。 */
export function playbackSessionOriginSeconds(mode: PlaybackMode, offsetSeconds: number): number {
  return mode === 'original-vod'
    ? originalVODSessionOriginSeconds(offsetSeconds)
    : offsetSeconds
}

type RecordingPlaybackPositionInput = {
  mode: RecordingPlaybackMode
  mediaSeconds: number
  headSeconds: number
  offsetSeconds: number
  sessionStartSeconds: number
}

/** recordingPlaybackPosition はメディア時刻を chase/original-vod の録画軸へ写す。 */
export function recordingPlaybackPosition(input: RecordingPlaybackPositionInput): number {
  return input.mode === 'chase'
    ? input.headSeconds + input.offsetSeconds + input.mediaSeconds
    : input.sessionStartSeconds + input.mediaSeconds
}

type RecordingSeekByInput = RecordingPlaybackPositionInput & { deltaSeconds: number }

/** recordingSeekByTarget は相対シークの移動先を録画軸上の秒へ写す。 */
export function recordingSeekByTarget(input: RecordingSeekByInput): number {
  return recordingPlaybackPosition({ ...input, mediaSeconds: input.mediaSeconds + input.deltaSeconds })
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
