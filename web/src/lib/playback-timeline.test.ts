import { describe, expect, it } from 'vitest'

import { fixedPlaybackTimeline, liveProgramPlaybackTimeline, liveRecordingPlaybackTimeline } from '@/lib/playback-timeline'

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

  it('録画ファイルの先頭を基準に、ライブ位置を録画済み終端の 1 秒前に置く', () => {
    const timeline = liveRecordingPlaybackTimeline({
      recordingHeadSeconds: 180,
      plannedSeconds: 3600,
      recordedSeconds: 2520,
    }, 3000)

    expect(timeline).toEqual({
      kind: 'live-recording',
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
    expect(liveRecordingPlaybackTimeline({ recordingHeadSeconds: 180, plannedSeconds: 3600, recordedSeconds: 2520 }, 1000).hoverLabel)
      .toBeNull()
    expect(liveRecordingPlaybackTimeline({ recordingHeadSeconds: null, plannedSeconds: 60, recordedSeconds: 90 }, null))
      .toMatchObject({ headSeconds: 0, maxSeconds: 90, extended: true, hoverLabel: null })
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
      selectableRange: { startSeconds: 180, endSeconds: 5400 },
      hoverSeconds: 600,
      hoverLabel: 'ここから見る（録画中）',
      extended: true,
      ariaValueText: '90:00 / 60:00（番組表上の予定）',
      startClock: expect.any(String),
      endClock: expect.any(String),
    })
  })

  it('only describes a selectable range when a recording can be sought', () => {
    const timeline = liveProgramPlaybackTimeline({
      startAt: '2026-10-02T10:00:00.000Z',
      endAt: '2026-10-02T11:00:00.000Z',
      nowMs: Date.parse('2026-10-02T11:30:00.000Z'),
      hoverSeconds: 600,
    })

    expect(timeline?.canSeek).toBe(false)
    expect(timeline?.selectableRange).toBeNull()
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
