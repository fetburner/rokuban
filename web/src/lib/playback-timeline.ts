import { formatPlaybackTime, formatTime } from '@/lib/format'
import { liveProgramAxis, programRecordingAccess } from '@/lib/live'

type PlaybackTimelineBase = {
  minSeconds: number
  maxSeconds: number
  canSeek: boolean
  extended: boolean
  /** 掴める・確定できる範囲。省略は軸の全域。null は選べる点が無い。 */
  selectableRange?: { startSeconds: number; endSeconds: number } | null
}

/** FixedPlaybackTimeline は一定尺の録画再生用の時間軸記述。 */
type FixedPlaybackTimeline = PlaybackTimelineBase & {
  kind: 'fixed'
  minSeconds: 0
  canSeek: true
}

/** LiveProgramPlaybackTimeline は番組予定を起点にしたライブ用の時間軸記述。 */
type LiveProgramPlaybackTimeline = PlaybackTimelineBase & {
  kind: 'live-program'
  plannedEndSeconds: number
  recordingStartSeconds: number
  liveEdgeSeconds: number
  selectableRange: { startSeconds: number; endSeconds: number } | null
  hoverSeconds: number | null
  hoverLabel: string | null
  canStartOver: boolean
  ariaValueText: string
  startClock: string
  endClock: string
}

/** PlaybackTimeline は共有操作バーが描く固定尺・ライブの時間軸記述。 */
export type PlaybackTimeline = FixedPlaybackTimeline | LiveProgramPlaybackTimeline

/** fixedPlaybackTimeline は録画尺の固定軸を操作バー向けの記述にする。 */
export function fixedPlaybackTimeline(durationSeconds: number): FixedPlaybackTimeline {
  const maxSeconds = Number.isFinite(durationSeconds) ? Math.max(0, durationSeconds) : 0
  return {
    kind: 'fixed',
    minSeconds: 0,
    maxSeconds,
    canSeek: true,
    extended: false,
  }
}

/** LiveProgramPlaybackTimelineInput は番組予定と録画状態からライブ軸を作る入力。 */
type LiveProgramPlaybackTimelineInput = {
  startAt: string
  endAt: string
  nowMs: number
  recordingId?: number | null
  recordingStartedAt?: string | null
  hoverSeconds: number | null
}

/** liveProgramPlaybackTimeline は EPG の番組軸と録画可能範囲を操作バー向けにまとめる。 */
export function liveProgramPlaybackTimeline(
  program: LiveProgramPlaybackTimelineInput,
): LiveProgramPlaybackTimeline | null {
  const axis = liveProgramAxis(program.startAt, program.endAt, program.nowMs)
  if (axis === null) return null

  const access = programRecordingAccess(program.recordingId, program.startAt, program.recordingStartedAt)
  const recordingStartSeconds = access.recordingHeadSeconds ?? 0

  return {
    kind: 'live-program',
    minSeconds: 0,
    maxSeconds: axis.maxSeconds,
    canSeek: access.canSeek,
    hoverSeconds: program.hoverSeconds,
    hoverLabel: program.hoverSeconds === null ? null : 'ここから見る（録画中）',
    extended: axis.liveEdgeSeconds > axis.plannedSeconds,
    plannedEndSeconds: axis.plannedSeconds,
    recordingStartSeconds,
    liveEdgeSeconds: axis.liveEdgeSeconds,
    selectableRange: access.canSeek && recordingStartSeconds <= axis.liveEdgeSeconds
      ? { startSeconds: recordingStartSeconds, endSeconds: axis.liveEdgeSeconds }
      : null,
    canStartOver: access.canStartOver,
    ariaValueText: `${formatPlaybackTime(axis.liveEdgeSeconds, false)} / ${formatPlaybackTime(axis.plannedSeconds, false)}（番組表上の予定）`,
    startClock: formatTime(program.startAt),
    endClock: formatTime(program.endAt),
  }
}
