import { formatPlaybackTime, formatTime } from '@/lib/format'
import { liveProgramAxis, programRecordingAccess } from '@/lib/live'
import type { RecordingTimeline } from '@/lib/recording-timeline'

/** PlaybackTimelineMarker は軸上の意味のある位置と表示・操作可否を表す。 */
export type PlaybackTimelineMarker = {
  kind: 'planned-end' | 'recording-start' | 'live-edge'
  seconds: number
  visible: boolean
  actionable?: boolean
}

/** PlaybackTimelineUnavailableSegment はシークできない時間範囲を表す。 */
export type PlaybackTimelineUnavailableSegment = {
  id: 'before' | 'after' | 'all'
  startSeconds: number
  endSeconds: number
}

type PlaybackTimelineBase = {
  minSeconds: number
  maxSeconds: number
  canSeek: boolean
  unavailableSegments: PlaybackTimelineUnavailableSegment[]
  markers: PlaybackTimelineMarker[]
  hoverSeconds: number | null
  hoverLabel: string | null
  extended: boolean
}

/** FixedPlaybackTimeline は一定尺の録画再生用の時間軸記述。 */
export type FixedPlaybackTimeline = PlaybackTimelineBase & {
  kind: 'fixed'
  minSeconds: 0
  canSeek: true
}

/** ChasePlaybackTimeline は録画先頭を起点にした追っかけ用の時間軸記述。 */
export type ChasePlaybackTimeline = PlaybackTimelineBase & {
  kind: 'chase'
  headSeconds: number
  recordedEndSeconds: number
  plannedEndSeconds: number
  liveEdgeSeconds: number
  recordedSegment: { startSeconds: number; endSeconds: number }
}

/** LiveProgramPlaybackTimeline は番組予定を起点にしたライブ用の時間軸記述。 */
export type LiveProgramPlaybackTimeline = PlaybackTimelineBase & {
  kind: 'live-program'
  plannedEndSeconds: number
  recordingStartSeconds: number
  liveEdgeSeconds: number
  canStartOver: boolean
  ariaValueText: string
  startClock: string
  endClock: string
}

/** PlaybackTimeline は共有操作バーが描く固定尺・追っかけ・ライブの時間軸記述。 */
export type PlaybackTimeline = FixedPlaybackTimeline | ChasePlaybackTimeline | LiveProgramPlaybackTimeline

/** fixedPlaybackTimeline は録画尺の固定軸を操作バー向けの記述にする。 */
export function fixedPlaybackTimeline(durationSeconds: number): FixedPlaybackTimeline {
  const maxSeconds = Number.isFinite(durationSeconds) ? Math.max(0, durationSeconds) : 0
  return {
    kind: 'fixed',
    minSeconds: 0,
    maxSeconds,
    canSeek: true,
    unavailableSegments: [],
    markers: [],
    hoverSeconds: null,
    hoverLabel: null,
    extended: false,
  }
}

/** chasePlaybackTimeline は録画ファイル先頭を基準に追っかけの時間軸記述を作る。 */
export function chasePlaybackTimeline(
  source: Pick<RecordingTimeline, 'chaseHeadOffsetSeconds' | 'plannedSeconds' | 'recordedSeconds'>,
  hoverSeconds: number | null,
): ChasePlaybackTimeline {
  const headSeconds = source.chaseHeadOffsetSeconds
  const recordedEndSeconds = headSeconds + source.recordedSeconds
  const plannedEndSeconds = source.plannedSeconds
  const maxSeconds = Math.max(1, plannedEndSeconds, recordedEndSeconds)
  const liveEdgeSeconds = Math.max(0, recordedEndSeconds - 1)
  const unavailableSegments: PlaybackTimelineUnavailableSegment[] = []
  if (headSeconds > 0) unavailableSegments.push({ id: 'before', startSeconds: 0, endSeconds: headSeconds })
  if (recordedEndSeconds < maxSeconds) {
    unavailableSegments.push({ id: 'after', startSeconds: recordedEndSeconds, endSeconds: maxSeconds })
  }

  return {
    kind: 'chase',
    minSeconds: 0,
    maxSeconds,
    canSeek: true,
    unavailableSegments,
    markers: [
      { kind: 'planned-end', seconds: plannedEndSeconds, visible: true },
      { kind: 'live-edge', seconds: liveEdgeSeconds, visible: true, actionable: true },
    ],
    hoverSeconds,
    hoverLabel: hoverSeconds === null
      ? null
      : hoverSeconds > recordedEndSeconds || hoverSeconds < headSeconds
        ? 'まだ録画されていません'
        : null,
    extended: recordedEndSeconds > plannedEndSeconds,
    headSeconds,
    recordedEndSeconds,
    plannedEndSeconds,
    liveEdgeSeconds,
    recordedSegment: { startSeconds: headSeconds, endSeconds: recordedEndSeconds },
  }
}

/** LiveProgramPlaybackTimelineInput は番組予定と録画状態からライブ軸を作る入力。 */
export type LiveProgramPlaybackTimelineInput = {
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
  const unavailableSegments: PlaybackTimelineUnavailableSegment[] = []
  if (!access.canSeek) {
    unavailableSegments.push({ id: 'all', startSeconds: 0, endSeconds: axis.maxSeconds })
  } else {
    if (recordingStartSeconds > 0) {
      unavailableSegments.push({ id: 'before', startSeconds: 0, endSeconds: recordingStartSeconds })
    }
    if (axis.liveEdgeSeconds < axis.maxSeconds) {
      unavailableSegments.push({ id: 'after', startSeconds: axis.liveEdgeSeconds, endSeconds: axis.maxSeconds })
    }
  }

  return {
    kind: 'live-program',
    minSeconds: 0,
    maxSeconds: axis.maxSeconds,
    canSeek: access.canSeek,
    unavailableSegments,
    markers: [
      { kind: 'planned-end', seconds: axis.plannedSeconds, visible: axis.liveEdgeSeconds > axis.plannedSeconds },
      { kind: 'recording-start', seconds: recordingStartSeconds, visible: access.canSeek },
      { kind: 'live-edge', seconds: axis.liveEdgeSeconds, visible: true },
    ],
    hoverSeconds: program.hoverSeconds,
    hoverLabel: program.hoverSeconds === null ? null : 'ここから見る（録画中）',
    extended: axis.liveEdgeSeconds > axis.plannedSeconds,
    plannedEndSeconds: axis.plannedSeconds,
    recordingStartSeconds,
    liveEdgeSeconds: axis.liveEdgeSeconds,
    canStartOver: access.canStartOver,
    ariaValueText: `${formatPlaybackTime(axis.liveEdgeSeconds, false)} / ${formatPlaybackTime(axis.plannedSeconds, false)}（番組表上の予定）`,
    startClock: formatTime(program.startAt),
    endClock: formatTime(program.endAt),
  }
}
