import { formatPlaybackTime, formatTime } from '@/lib/format'
import { liveProgramAxis, programRecordingAccess } from '@/lib/live'
import type { RecordingTimeline } from '@/lib/recording-timeline'

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

/**
 * LiveRecordingPlaybackTimeline はライブ画面が録画 HLS を再生しているときの番組軸の記述。
 * 軸は番組開始を左端にし、録画ファイルの先頭（headSeconds）から録画済みの先端までを再生できる。
 */
type LiveRecordingPlaybackTimeline = PlaybackTimelineBase & {
  kind: 'live-recording'
  headSeconds: number
  recordedEndSeconds: number
  plannedEndSeconds: number
  liveEdgeSeconds: number
  hoverSeconds: number | null
  hoverLabel: string | null
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

/** PlaybackTimeline は共有操作バーが描く固定尺・ライブ番組・ライブ画面の録画の時間軸記述。 */
export type PlaybackTimeline = FixedPlaybackTimeline | LiveRecordingPlaybackTimeline | LiveProgramPlaybackTimeline

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

/** LiveRecordingSource は録画軸が読む RecordingTimeline の欄。 */
export type LiveRecordingSource = Pick<RecordingTimeline, 'recordingHeadSeconds' | 'plannedSeconds' | 'recordedSeconds'>

/**
 * liveRecordingPlaybackTimeline は録画ファイル先頭を基準に、ライブ画面の録画軸の記述を作る。
 * ライブ位置は録画済みの先端の 1 秒前に置く。
 */
export function liveRecordingPlaybackTimeline(
  source: LiveRecordingSource,
  hoverSeconds: number | null,
): LiveRecordingPlaybackTimeline {
  const headSeconds = source.recordingHeadSeconds ?? 0
  const recordedEndSeconds = headSeconds + source.recordedSeconds
  const plannedEndSeconds = source.plannedSeconds
  const maxSeconds = Math.max(1, plannedEndSeconds, recordedEndSeconds)
  const liveEdgeSeconds = Math.max(0, recordedEndSeconds - 1)

  return {
    kind: 'live-recording',
    minSeconds: 0,
    maxSeconds,
    canSeek: true,
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
