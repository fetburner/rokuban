import { describe, expect, it } from 'vitest'

import { playbackStrategy } from '@/lib/playback-mode'

describe('playbackStrategy', () => {
  const strategies = {
    live: playbackStrategy({ mode: 'live', site: 'default', networkId: 0, serviceId: 1024 }),
    chase: playbackStrategy({
      mode: 'chase',
      site: 'default',
      recordingId: 42,
      chaseTimeline: { chaseHeadOffsetSeconds: 0, plannedSeconds: 3600, recordedSeconds: 1800 },
    }),
    'original-vod': playbackStrategy({
      mode: 'original-vod',
      site: 'default',
      recordingId: 42,
      recordingDurationMs: 3_600_000,
    }),
  }

  const expectedErrorMessages = {
    live: {
      capacity: {
        title: 'いま視聴できません（チューナー不足または同時視聴数の上限）。',
        guidance: 'チャンネルを切り替えた直後は、前のチャンネルの解放待ちの可能性があります。30 秒ほど待って再読み込みしてください。',
      },
      otherTitle: 'ライブ視聴でエラーが発生しました。',
      fatalTitle: 'ライブ再生中にエラーが発生しました',
      mediaError: 'ライブ映像を再生できませんでした（映像データを読み込めません）',
    },
    chase: {
      capacity: {
        title: 'いま追っかけ再生を始められません。',
        guidance: 'しばらく待ってから再読み込みしてください。',
      },
      chaseInputTitle: '追っかけ再生の入力に失敗したため、一時停止しています。',
      otherTitle: '追っかけ再生でエラーが発生しました。',
      fatalTitle: '追っかけ再生中にエラーが発生しました',
      mediaError: '追っかけ映像を再生できませんでした（映像データを読み込めません）',
    },
    'original-vod': {
      capacity: {
        title: 'いま再生を始められません（同時再生数の上限、または streamer の再起動中）。',
        guidance: 'しばらく待ってから再読み込みしてください。',
      },
      otherTitle: '原本 TS の再生でエラーが発生しました。',
      fatalTitle: '原本 TS の再生中にエラーが発生しました',
      mediaError: '録画映像を再生できませんでした（映像データを読み込めません）',
    },
  }

  for (const mode of Object.keys(strategies) as Array<keyof typeof strategies>) {
    it(`${mode} はプレイリストエラーと方式別の文言を持つ`, () => {
      const strategy = strategies[mode]
      expect(strategy.classifyLoadError({
        kind: 'http',
        status: 503,
        body: 'too many concurrent live sessions on this process',
      })).toEqual({ kind: 'capacity', message: 'too many concurrent live sessions on this process' })

      const retryAfterError = strategy.classifyLoadError({
        kind: 'http',
        status: 502,
        body: 'cooldown',
        retryAfter: '5',
      })
      if (mode === 'chase') {
        expect(retryAfterError).toEqual({ kind: 'chase-input', message: 'cooldown' })
      } else {
        expect(retryAfterError).toEqual({ kind: 'other', status: 502, message: 'cooldown' })
      }

      expect(strategy.errorMessages).toEqual(expectedErrorMessages[mode])
    })
  }

  it('live は番組の時間軸・ライブ離脱先・同じ秒の起点を持ち、録画シークを持たない', () => {
    const strategy = playbackStrategy({
      mode: 'live',
      site: 'a b',
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
      hoverLabel: 'ここから見る（録画中）',
    })
    expect(strategy.recording).toBeUndefined()
    expect(strategy.leaveURL(90)).toBe('/api/sites/a%20b/networks/1/services/2/live/leave')
    expect(strategy.originSeconds(10)).toBe(10)
  })

  it('chase は伸びる番組の時間軸・録画先頭基準の seek・offset 離脱先を持つ', () => {
    const strategy = playbackStrategy({
      mode: 'chase',
      site: 'default',
      recordingId: 42,
      chaseTimeline: { chaseHeadOffsetSeconds: 1200, plannedSeconds: 3600, recordedSeconds: 1500 },
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
      recordingDurationMs: 4_321_000,
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
    expect(playbackStrategy({ mode: 'original-vod', site: 'default', recordingId: 42, recordingDurationMs: 0 }).recording?.seekTarget(10)).toBeUndefined()
    expect(strategy.leaveURL(90)).toBe('/api/sites/default/recordings/42/original-vod/offset/90/leave')
    expect(strategy.originSeconds(10)).toBeCloseTo(9.976633333, 9)
  })
})
