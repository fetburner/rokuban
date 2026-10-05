import { describe, expect, it } from 'vitest'

import {
  chasePlaybackTimeline,
  fixedPlaybackTimeline,
  liveProgramPlaybackTimeline,
} from '@/lib/playback-timeline'

describe('playback timeline descriptions', () => {
  it('describes a fixed playback range', () => {
    expect(fixedPlaybackTimeline(3600)).toEqual({
      kind: 'fixed',
      minSeconds: 0,
      maxSeconds: 3600,
      canSeek: true,
      extended: false,
    })
  })

  it('uses the recording file start and puts the chase live edge one second before recorded end', () => {
    const timeline = chasePlaybackTimeline({
      chaseHeadOffsetSeconds: 180,
      plannedSeconds: 3600,
      recordedSeconds: 2520,
    }, 3000)

    expect(timeline).toEqual({
      kind: 'chase',
      minSeconds: 0,
      maxSeconds: 3600,
      canSeek: true,
      headSeconds: 180,
      recordedEndSeconds: 2700,
      plannedEndSeconds: 3600,
      liveEdgeSeconds: 2699,
      hoverSeconds: 3000,
      hoverLabel: 'まだ録画されていません',
      extended: false,
    })
  })

  it('describes the live program axis, recording range, and live edge from the programme clock', () => {
    const timeline = liveProgramPlaybackTimeline({
      startAt: '2026-10-02T10:00:00.000Z',
      endAt: '2026-10-02T11:00:00.000Z',
      nowMs: Date.parse('2026-10-02T11:30:00.000Z'),
      recordingId: 12,
      recordingStartedAt: '2026-10-02T10:03:00.000Z',
      hoverSeconds: 600,
    })

    expect(timeline).toEqual({
      kind: 'live-program',
      minSeconds: 0,
      maxSeconds: 5400,
      canSeek: true,
      canStartOver: true,
      plannedEndSeconds: 3600,
      recordingStartSeconds: 180,
      liveEdgeSeconds: 5400,
      hoverSeconds: 600,
      hoverLabel: 'ここから見る（録画中）',
      extended: true,
      ariaValueText: '90:00 / 60:00（番組表上の予定）',
      startClock: expect.any(String),
      endClock: expect.any(String),
    })
  })

  it('leaves a malformed programme without a timeline', () => {
    expect(liveProgramPlaybackTimeline({
      startAt: 'invalid',
      endAt: '2026-10-02T11:00:00.000Z',
      nowMs: Date.parse('2026-10-02T11:30:00.000Z'),
      hoverSeconds: null,
    })).toBeNull()
  })
})
