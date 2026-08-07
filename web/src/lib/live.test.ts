import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import {
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

describe('supportsNativeHls', () => {
  it('probably は true', () => {
    expect(supportsNativeHls(() => 'probably')).toBe(true)
  })

  it('maybe は true', () => {
    expect(supportsNativeHls(() => 'maybe')).toBe(true)
  })

  it('空文字は false（未対応。jsdom の既定値でもある）', () => {
    expect(supportsNativeHls(() => '')).toBe(false)
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
})
