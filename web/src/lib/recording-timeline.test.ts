import { describe, expect, it } from 'vitest'

import { recordingTimeline } from '@/lib/recording-timeline'

const programmeStartAt = '2026-10-02T10:00:00.000Z'
const recordingBefore = '2026-10-02T09:57:00.000Z'
const recordingAtStart = programmeStartAt
const recordingLate = '2026-10-02T10:03:00.000Z'
const nowAt45Minutes = Date.parse('2026-10-02T10:45:00.000Z')

type RecordingTimelineInput = Parameters<typeof recordingTimeline>[0]

function recording(overrides: Partial<RecordingTimelineInput> = {}): RecordingTimelineInput {
  return {
    startAt: programmeStartAt,
    durationMs: 60 * 60_000,
    status: 'recording',
    startedAt: recordingBefore,
    ...overrides,
  }
}

describe('recordingTimeline', () => {
  it('uses the programme start when the tuner opened early', () => {
    const timeline = recordingTimeline(recording(), { nowMs: nowAt45Minutes, programSeconds: 540 })

    expect(timeline.recordingFileStartAtMs).toBe(Date.parse(programmeStartAt))
    expect(timeline.recordingHeadSeconds).toBe(0)
    expect(timeline.programRecordingOffsetSeconds).toBe(540)
  })

  it('uses the actual recording start when the tuner opened late', () => {
    const timeline = recordingTimeline(recording({ startedAt: recordingLate }), {
      nowMs: nowAt45Minutes,
      programSeconds: 540,
    })

    expect(timeline.recordingFileStartAtMs).toBe(Date.parse(recordingLate))
    expect(timeline.recordingHeadSeconds).toBe(180)
    expect(timeline.chaseHeadOffsetSeconds).toBe(180)
    expect(timeline.recordedSeconds).toBe(2520)
    expect(timeline.recordedEndSeconds).toBe(2700)
    expect(timeline.programRecordingOffsetSeconds).toBe(360)
    expect(recordingTimeline(recording({ startedAt: recordingLate }), { programSeconds: 180 }).programRecordingOffsetSeconds)
      .toBe(0)
    expect(recordingTimeline(recording({ startedAt: recordingLate }), { programSeconds: 179 }).programRecordingOffsetSeconds)
      .toBeNull()
  })

  it('uses the shared timestamp when recording and programme start together', () => {
    const timeline = recordingTimeline(recording({ startedAt: recordingAtStart }), {
      nowMs: nowAt45Minutes,
      programSeconds: 540,
    })

    expect(timeline.recordingFileStartAtMs).toBe(Date.parse(programmeStartAt))
    expect(timeline.recordingHeadSeconds).toBe(0)
    expect(timeline.programRecordingOffsetSeconds).toBe(540)
  })

  it('keeps a future scheduled end while extending the recorded edge to now', () => {
    const timeline = recordingTimeline(recording(), { nowMs: nowAt45Minutes })

    expect(timeline.programmeEndAt).toBe('2026-10-02T11:00:00.000Z')
    expect(timeline.plannedSeconds).toBe(3600)
    expect(timeline.recordedSeconds).toBe(2700)
    expect(timeline.recordedEndSeconds).toBe(2700)
    expect(timeline.maxSeconds).toBe(3600)
    expect(timeline.recordedDurationMs).toBeNull()
  })

  it('uses the final recording timestamp for a completed recording', () => {
    const timeline = recordingTimeline(recording({
      status: 'finished',
      endedAt: '2026-10-02T11:06:00.000Z',
    }))

    expect(timeline.recordedSeconds).toBe(3960)
    expect(timeline.recordedDurationMs).toBe(3_960_000)
    expect(timeline.recordedAfterProgramStartPercent).toBeCloseTo(90.9091, 3)
  })

  it('extends maxSeconds to the recorded edge once it passes the planned end', () => {
    const timeline = recordingTimeline(recording(), { nowMs: Date.parse('2026-10-02T11:10:00.000Z') })

    expect(timeline.plannedSeconds).toBe(3600)
    expect(timeline.recordedEndSeconds).toBe(4200)
    expect(timeline.maxSeconds).toBe(4200)
  })

  it('returns no file axis when either timestamp cannot be parsed', () => {
    const badStarted = recordingTimeline(recording({ startedAt: 'invalid' }), { programSeconds: 540 })
    expect(badStarted.recordingFileStartAtMs).toBeNull()
    expect(badStarted.recordingHeadSeconds).toBeNull()
    expect(badStarted.programRecordingOffsetSeconds).toBeNull()
    expect(badStarted.recordedSeconds).toBe(0)

    for (const startAt of ['', 'invalid']) {
      const badStart = recordingTimeline(recording({ startAt }), { programSeconds: 540 })
      expect(badStart.programmeEndAt).toBeNull()
      expect(badStart.recordingFileStartAtMs).toBeNull()
      expect(badStart.recordingHeadSeconds).toBeNull()
      expect(badStart.programRecordingOffsetSeconds).toBeNull()
    }
  })
})
