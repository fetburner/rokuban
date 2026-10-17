import { describe, expect, it } from 'vitest'

import {
  playbackLeaveURL,
  playbackModeAxis,
  playbackSeekTarget,
  playbackSessionOriginSeconds,
} from '@/lib/playback-mode'

describe('playbackModeAxis', () => {
  it('chase は録画済み範囲・ライブ端・予定終端・hover 印を作る', () => {
    const axis = playbackModeAxis({
      mode: 'chase',
      timeline: { chaseHeadOffsetSeconds: 1200, plannedSeconds: 3600, recordedSeconds: 1500 },
      hoverSeconds: 2800,
    })

    expect(axis).toMatchObject({
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
  })

  it('live は番組予定・録画可能範囲・hover 印を作る', () => {
    const axis = playbackModeAxis({
      mode: 'live',
      program: {
        startAt: '2026-10-17T10:00:00.000Z',
        endAt: '2026-10-17T11:00:00.000Z',
        nowMs: Date.parse('2026-10-17T10:30:00.000Z'),
        recordingId: 42,
        recordingStartedAt: '2026-10-17T10:05:00.000Z',
      },
      hoverSeconds: 900,
    })

    expect(axis).toMatchObject({
      kind: 'live-program',
      maxSeconds: 3600,
      plannedEndSeconds: 3600,
      liveEdgeSeconds: 1800,
      selectableRange: { startSeconds: 300, endSeconds: 1800 },
      hoverSeconds: 900,
      hoverLabel: 'ここから見る（録画中）',
    })
  })

  it('original-vod は実尺を固定軸にする', () => {
    expect(playbackModeAxis({ mode: 'original-vod', durationSeconds: 4321 })).toEqual({
      kind: 'fixed',
      minSeconds: 0,
      maxSeconds: 4321,
      canSeek: true,
      extended: false,
    })
  })
})

describe('playback mode calculations', () => {
  it('original-vod の session 起点をフレーム格子へ写し、他の mode は秒を保つ', () => {
    expect(playbackSessionOriginSeconds('chase', 10)).toBe(10)
    expect(playbackSessionOriginSeconds('original-vod', 10)).toBeCloseTo(9.976633333, 9)
    expect(playbackSessionOriginSeconds('live', 10)).toBe(10)
  })

  it('chase seek は録画先端で止めて整数秒へ丸める', () => {
    expect(playbackSeekTarget({
      mode: 'chase',
      axisSeconds: 1320.6,
      headSeconds: 1200,
      liveEdgeSeconds: 2699,
    })).toBe(121)
    expect(playbackSeekTarget({
      mode: 'chase',
      axisSeconds: 4000,
      headSeconds: 1200,
      liveEdgeSeconds: 2699,
    })).toBe(1499)
  })

  it('original-vod seek は実尺で clamp し、フレーム端数を保つ', () => {
    expect(playbackSeekTarget({ mode: 'original-vod', axisSeconds: 23.5, durationSeconds: 60 })).toBe(23.5)
    expect(playbackSeekTarget({ mode: 'original-vod', axisSeconds: 70, durationSeconds: 60 })).toBe(60)
  })

  it('各 mode の離脱先を現在の session URL と同じ識別子で作る', () => {
    expect(playbackLeaveURL({ mode: 'live', site: 'a b', networkId: 1, serviceId: 2 }))
      .toBe('/api/sites/a%20b/networks/1/services/2/live/leave')
    expect(playbackLeaveURL({ mode: 'chase', site: 'default', recordingId: 42 }, 90))
      .toBe('/api/sites/default/recordings/42/chase/offset/90/leave')
    expect(playbackLeaveURL({ mode: 'original-vod', site: 'default', recordingId: 42 }, 90))
      .toBe('/api/sites/default/recordings/42/original-vod/offset/90/leave')
  })
})
