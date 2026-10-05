import type { Recording } from '@/api/generated'

type RecordingTimelineSource = Pick<Recording, 'startAt' | 'durationMs'> &
  Partial<Pick<Recording, 'startedAt' | 'endedAt' | 'status'>>

type RecordingTimelineOptions = {
  /** 現在の時刻。指定時は endedAt より優先して録画済み範囲を計算する。 */
  nowMs?: number
  /** 番組一覧の時刻。ライブ再生では録画作成時の番組 snapshot より新しい場合がある。 */
  programmeStartAt?: string
  programmeEndAt?: string
  /** 番組開始からの秒。指定時は録画先頭からの offset も返す。 */
  programSeconds?: number
}

export type RecordingTimeline = {
  programmeStartMs: number
  programmeEndMs: number
  /** 番組開始が解釈できないとき null。 */
  programmeEndAt: string | null
  recordingFileStartAtMs: number | null
  recordingEndAtMs: number | null
  recordedDurationMs: number | null
  recordingHeadSeconds: number | null
  chaseHeadOffsetSeconds: number
  plannedSeconds: number
  recordedSeconds: number
  availableChaseSeconds: number
  recordedEndSeconds: number
  maxSeconds: number
  programRecordingOffsetSeconds: number | null
  recordedAfterProgramStartPercent: number | null
}

function parseTime(value: string | null | undefined): number | null {
  if (value === null || value === undefined) return null
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : null
}

/** recordingTimeline は録画の番組軸とファイル軸の値をまとめて導出する。 */
export function recordingTimeline(
  recording: RecordingTimelineSource,
  options: RecordingTimelineOptions = {},
): RecordingTimeline {
  const programmeStartMs = Date.parse(options.programmeStartAt ?? recording.startAt)
  const programmeEndMs = options.programmeEndAt === undefined
    ? programmeStartMs + recording.durationMs
    : Date.parse(options.programmeEndAt)
  const programmeEndAt = Number.isFinite(programmeEndMs) ? new Date(programmeEndMs).toISOString() : null
  const startedAtMs = parseTime(recording.startedAt)
  const recordingFileStartAtMs = Number.isFinite(programmeStartMs) && startedAtMs !== null
    ? Math.max(startedAtMs, programmeStartMs)
    : null
  const recordingEndAtMs = parseTime(recording.endedAt)
  const recordedDurationMs = recordingFileStartAtMs !== null && recordingEndAtMs !== null
    ? recordingEndAtMs - recordingFileStartAtMs
    : null
  const recordingHeadSeconds = recordingFileStartAtMs !== null && Number.isFinite(programmeStartMs)
    ? Math.max(0, (recordingFileStartAtMs - programmeStartMs) / 1000)
    : null
  const chaseHeadOffsetSeconds = recordingHeadSeconds ?? 0
  // 規則は main のまま温存: ライブ画面は現在の EPG 幅（端数そのまま）、録画詳細は snapshot の durationMs（切り上げ）。
  const plannedSeconds = options.programmeEndAt === undefined
    ? Math.max(0, Math.ceil(recording.durationMs / 1000))
    : Math.max(1, (programmeEndMs - programmeStartMs) / 1000)
  const recordedThroughMs = options.nowMs ?? recordingEndAtMs ?? undefined
  const recordedSeconds = recordingFileStartAtMs !== null && recordedThroughMs !== undefined && Number.isFinite(recordedThroughMs)
    ? Math.max(0, (recordedThroughMs - recordingFileStartAtMs) / 1000)
    : 0
  const availableChaseSeconds = Math.floor(recordedSeconds)
  const recordedEndSeconds = chaseHeadOffsetSeconds + availableChaseSeconds
  const maxSeconds = Math.max(1, plannedSeconds, recordedEndSeconds)
  const programRecordingOffsetSeconds = recordingFileStartAtMs !== null &&
      Number.isFinite(programmeStartMs) &&
      options.programSeconds !== undefined &&
      Number.isFinite(options.programSeconds)
    ? Math.floor((programmeStartMs + options.programSeconds * 1000 - recordingFileStartAtMs) / 1000)
    : null
  const validProgramOffsetSeconds = programRecordingOffsetSeconds !== null && programRecordingOffsetSeconds >= 0
    ? programRecordingOffsetSeconds
    : null
  const recordedAfterProgramStartPercent = recordingFileStartAtMs !== null &&
      recordingEndAtMs !== null &&
      recordedDurationMs !== null &&
      recordedDurationMs > 0
    ? Math.min(100, Math.max(0, ((programmeEndMs - recordingFileStartAtMs) / recordedDurationMs) * 100))
    : null

  return {
    programmeStartMs,
    programmeEndMs,
    programmeEndAt,
    recordingFileStartAtMs,
    recordingEndAtMs,
    recordedDurationMs,
    recordingHeadSeconds,
    chaseHeadOffsetSeconds,
    plannedSeconds,
    recordedSeconds,
    availableChaseSeconds,
    recordedEndSeconds,
    maxSeconds,
    programRecordingOffsetSeconds: validProgramOffsetSeconds,
    recordedAfterProgramStartPercent,
  }
}
