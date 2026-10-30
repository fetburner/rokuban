import { describe, expect, it } from 'vitest'

import { playbackStrategy } from '@/lib/playback-mode'

describe('playbackStrategy', () => {
  it('live は番組の時間軸と live capacity error message を持つ', () => {
    const strategy = playbackStrategy({
      mode: 'live',
      site: 'default',
      networkId: 1,
      serviceId: 2,
      liveProgram: {
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
    })
    expect(strategy.recording).toBeUndefined()
    expect(strategy.errorMessages.capacity.title).toContain('チューナー不足')
  })

  it('recording HLS uses recording time and clamps seeks to both duration and playlist end', () => {
    const strategy = playbackStrategy({ mode: 'recording-hls', recordingDurationMs: 45_000 })

    expect(strategy.axis(null)).toEqual({
      kind: 'fixed',
      minSeconds: 0,
      maxSeconds: 45,
      canSeek: true,
      extended: false,
    })
    expect(strategy.recording?.seekTarget(12.75)).toBe(12.75)
    expect(strategy.recording?.seekTarget(80)).toBe(45)
    expect(strategy.recording?.seekTarget(80, 20)).toBe(20)
    expect(strategy.recording?.seekTarget(-2)).toBe(0)
    expect(strategy.errorMessages.otherTitle).toBe('録画 HLS の再生でエラーが発生しました。')
  })

  it('0 以下または不明の長さでは録画時間軸を作らない', () => {
    expect(playbackStrategy({ mode: 'recording-hls', recordingDurationMs: 0 }).recording?.seekTarget(10)).toBeUndefined()
    expect(playbackStrategy({ mode: 'recording-hls', recordingDurationMs: Number.NaN }).recording?.seekTarget(10)).toBeUndefined()
  })
})
