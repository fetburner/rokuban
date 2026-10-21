import { describe, expect, it } from 'vitest'

import { playbackStrategy } from '@/lib/playback-mode'

describe('playbackStrategy', () => {
  it('live は番組軸・ライブ離脱先・同じ秒の起点を持ち、録画シークを持たない', () => {
    const strategy = playbackStrategy({
      mode: 'live',
      site: 'a b',
      networkId: 1,
      serviceId: 2,
      program: {
        startAt: '2026-10-17T10:00:00.000Z',
        endAt: '2026-10-17T11:00:00.000Z',
        nowMs: Date.parse('2026-10-17T10:30:00.000Z'),
        recordingId: 42,
        recordingStartedAt: '2026-10-17T10:05:00.000Z',
      },
    })

    expect(strategy.axis(900)).toMatchObject({
      kind: 'live-program',
      maxSeconds: 3600,
      plannedEndSeconds: 3600,
      liveEdgeSeconds: 1800,
      selectableRange: { startSeconds: 300, endSeconds: 1800 },
      hoverSeconds: 900,
      hoverLabel: 'ここから見る（録画中）',
    })
    expect(strategy.recording).toBeUndefined()
    expect(strategy.leaveURL(90)).toBe('/api/sites/a%20b/networks/1/services/2/live/leave')
    expect(strategy.originSeconds(10)).toBe(10)
  })

  it('chase は伸びる番組軸・録画先頭基準の seek・offset 離脱先を持つ', () => {
    const strategy = playbackStrategy({
      mode: 'chase',
      site: 'default',
      recordingId: 42,
      timeline: { chaseHeadOffsetSeconds: 1200, plannedSeconds: 3600, recordedSeconds: 1500 },
    })

    expect(strategy.axis(2800)).toMatchObject({
      kind: 'chase',
      minSeconds: 0,
      maxSeconds: 3600,
      headSeconds: 1200,
      recordedEndSeconds: 2700,
      plannedEndSeconds: 3600,
      liveEdgeSeconds: 2699,
      hoverSeconds: 2800,
      hoverLabel: 'まだ録画されていません',
    })
    expect(strategy.recording?.seekTarget(1320.6)).toBe(121)
    expect(strategy.recording?.seekTarget(4000)).toBe(1499)
    expect(strategy.recording?.seekTarget(100)).toBe(0)
    expect(strategy.leaveURL(90)).toBe('/api/sites/default/recordings/42/chase/offset/90/leave')
    expect(strategy.originSeconds(10)).toBe(10)
  })

  it('original-vod は固定尺・フレーム格子の起点・録画尺で clamp する seek を持つ', () => {
    const strategy = playbackStrategy({
      mode: 'original-vod',
      site: 'default',
      recordingId: 42,
      durationSeconds: 4321,
    })

    expect(strategy.axis(900)).toEqual({
      kind: 'fixed',
      minSeconds: 0,
      maxSeconds: 4321,
      canSeek: true,
      extended: false,
    })
    expect(strategy.recording?.seekTarget(23.5)).toBe(23.5)
    expect(strategy.recording?.seekTarget(5000)).toBe(4321)
    expect(strategy.recording?.seekTarget(-1)).toBe(0)
    expect(strategy.leaveURL(90)).toBe('/api/sites/default/recordings/42/original-vod/offset/90/leave')
    expect(strategy.originSeconds(10)).toBeCloseTo(9.976633333, 9)
  })
})
