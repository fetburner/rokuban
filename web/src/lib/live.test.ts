import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import {
  claimsHlsPlaylistSupport,
  classifyLiveLoadError,
  currentProgramWindow,
  livePlaylistURL,
  pickInitialServiceId,
  probeLivePlaylist,
  supportsNativeHls,
} from '@/lib/live'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('livePlaylistURL', () => {
  it('profile 無しは ?profile を付けない', () => {
    expect(livePlaylistURL('default', 1024)).toBe(
      '/api/sites/default/services/1024/live/playlist.m3u8',
    )
  })

  it('profile を渡すと ?profile= が付く', () => {
    expect(livePlaylistURL('default', 1024, 'h264-720p')).toBe(
      '/api/sites/default/services/1024/live/playlist.m3u8?profile=h264-720p',
    )
  })

  it('site をエスケープする', () => {
    expect(livePlaylistURL('a b', 1)).toBe('/api/sites/a%20b/services/1/live/playlist.m3u8')
  })
})

/**
 * measuredCanPlayType は Playwright の各エンジンで**実測した** `canPlayType` の
 * 戻り値（`web/e2e/live.mjs` の⑥が実ブラウザ上で同じ値を突き合わせ続ける）。
 *
 * **実ブラウザが実際に返す値だけでテストする。** レビュー #190 の 1 回目の修正は
 * `canPlayType('application/vnd.apple.mpegurl') === 'probably'` を要求していたが、
 * codecs 無しでこの値を返す実ブラウザは 1 つも無い（3 エンジンとも `'maybe'`）ので、
 * 「`'probably'` なら true」というテストは**どのブラウザでも起こらない入力**に
 * ついての主張であり、ネイティブ分岐が全ブラウザで到達不能になったことを
 * 通してしまった。ここでは表そのものを入力にする。
 */
const measuredCanPlayType: Record<string, Record<string, string>> = {
  // WebKit 605.1.15（UA は Version/26.5 Safari/605.1.15）
  webkit: { 'application/vnd.apple.mpegurl': 'maybe', 'video/mp2t': 'maybe' },
  // Playwright 同梱 Chromium 151
  chromium: { 'application/vnd.apple.mpegurl': 'maybe', 'video/mp2t': '' },
  // channel: 'chrome' の実 Google Chrome 151
  chrome: { 'application/vnd.apple.mpegurl': 'maybe', 'video/mp2t': '' },
  // Firefox 153
  firefox: { 'application/vnd.apple.mpegurl': '', 'video/mp2t': '' },
  // jsdom（`HTMLMediaElement.canPlayType` 未実装。常に空文字）
  jsdom: {},
}

function canPlayTypeOf(engine: string): (type: string) => string {
  return (type) => measuredCanPlayType[engine]?.[type] ?? ''
}

describe('supportsNativeHls', () => {
  it('WebKit だけが true（セグメントの video/mp2t を demux できるのは WebKit だけ）', () => {
    expect(supportsNativeHls(canPlayTypeOf('webkit'))).toBe(true)
  })

  it.each(['chromium', 'chrome', 'firefox', 'jsdom'])(
    '%s は false（true にすると video.src に m3u8 を直接渡され、沈黙して再生できない）',
    (engine) => {
      expect(supportsNativeHls(canPlayTypeOf(engine))).toBe(false)
    },
  )

  it('プレイリストの MIME だけでは Chrome と区別できない（実測値。判定に使わない根拠）', () => {
    const playlistOnly = (engine: string) =>
      canPlayTypeOf(engine)('application/vnd.apple.mpegurl')
    expect(playlistOnly('webkit')).toBe(playlistOnly('chrome'))
    expect(playlistOnly('webkit')).toBe(playlistOnly('chromium'))
  })
})

describe('claimsHlsPlaylistSupport', () => {
  it.each(['webkit', 'chromium', 'chrome'])(
    '%s は true（この弱い問いだけでネイティブ分岐を選んではならない）',
    (engine) => {
      expect(claimsHlsPlaylistSupport(canPlayTypeOf(engine))).toBe(true)
    },
  )

  it.each(['firefox', 'jsdom'])('%s は false', (engine) => {
    expect(claimsHlsPlaylistSupport(canPlayTypeOf(engine))).toBe(false)
  })
})

function makeService(overrides: Partial<Service>): Service {
  return {
    networkId: 1,
    serviceId: 1,
    name: 'test',
    channelType: 'GR',
    channel: '1',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
    ...overrides,
  }
}

describe('pickInitialServiceId', () => {
  it('要求した id が一覧に存在すればそれを使う', () => {
    const services = [makeService({ serviceId: 1 }), makeService({ serviceId: 2 })]
    expect(pickInitialServiceId(services, 2)).toBe(2)
  })

  it('要求した id が存在しなければ番組を持つ先頭にフォールバックする', () => {
    const services = [
      makeService({ serviceId: 1, hasPrograms: false }),
      makeService({ serviceId: 2, hasPrograms: true }),
    ]
    expect(pickInitialServiceId(services, 999)).toBe(2)
  })

  it('未指定でも番組を持つ先頭にフォールバックする', () => {
    const services = [
      makeService({ serviceId: 1, hasPrograms: false }),
      makeService({ serviceId: 2, hasPrograms: true }),
    ]
    expect(pickInitialServiceId(services, undefined)).toBe(2)
  })

  it('番組を持つサービスが無ければ先頭を使う', () => {
    const services = [
      makeService({ serviceId: 5, hasPrograms: false }),
      makeService({ serviceId: 6, hasPrograms: false }),
    ]
    expect(pickInitialServiceId(services, undefined)).toBe(5)
  })

  it('サービスが 1 件も無ければ undefined', () => {
    expect(pickInitialServiceId([], 1)).toBeUndefined()
  })
})

describe('currentProgramWindow', () => {
  it('start は now、end は windowMs 後', () => {
    const nowMs = new Date('2026-08-08T10:00:00.000Z').getTime()
    expect(currentProgramWindow(nowMs, 60_000)).toEqual({
      start: '2026-08-08T10:00:00.000Z',
      end: '2026-08-08T10:01:00.000Z',
    })
  })

  it('windowMs を省略すると 60 秒幅になる', () => {
    const nowMs = new Date('2026-08-08T10:00:00.000Z').getTime()
    const { start, end } = currentProgramWindow(nowMs)
    expect(new Date(end).getTime() - new Date(start).getTime()).toBe(60_000)
  })
})

describe('classifyLiveLoadError', () => {
  it('network は unreachable になる', () => {
    expect(classifyLiveLoadError({ kind: 'network' })).toEqual({ kind: 'unreachable' })
  })

  it('503 は capacity になり、本文をそのまま運ぶ', () => {
    expect(
      classifyLiveLoadError({
        kind: 'http',
        status: 503,
        body: 'too many concurrent live sessions on this process\n',
      }),
    ).toEqual({ kind: 'capacity', message: 'too many concurrent live sessions on this process' })
  })

  it('503 以外は other になり、status と本文を運ぶ', () => {
    expect(
      classifyLiveLoadError({ kind: 'http', status: 500, body: 'boom' }),
    ).toEqual({ kind: 'other', status: 500, message: 'boom' })
  })
})

describe('probeLivePlaylist', () => {
  it('200 なら ok', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('', { status: 200 }))),
    )
    expect(await probeLivePlaylist('/x')).toEqual({ ok: true })
  })

  it('503 なら capacity エラーとして本文を運ぶ', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('live stream unavailable', { status: 503 }))),
    )
    expect(await probeLivePlaylist('/x')).toEqual({
      ok: false,
      error: { kind: 'capacity', message: 'live stream unavailable' },
    })
  })

  it('fetch が reject すると unreachable になる（streamer 不在）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )
    expect(await probeLivePlaylist('/x')).toEqual({
      ok: false,
      error: { kind: 'unreachable' },
    })
  })

  it('signal を渡すと fetch に伝わり、中断は unreachable に丸めず再 throw する', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      const signal = init?.signal
      return new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener('abort', () => {
          reject(new DOMException('aborted', 'AbortError'))
        })
      })
    })
    vi.stubGlobal('fetch', fetchMock)

    const controller = new AbortController()
    const promise = probeLivePlaylist('/x', controller.signal)
    controller.abort()

    await expect(promise).rejects.toThrow(
      expect.objectContaining({ name: 'AbortError' }),
    )
    // fetch 自身にも signal が渡っている（呼び出し側だけが中断を握っているのではない）
    expect(fetchMock).toHaveBeenCalledWith('/x', { signal: controller.signal })
  })
})
