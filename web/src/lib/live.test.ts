import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import {
  claimsHlsPlaylistSupport,
  classifyLiveLoadError,
  currentProgramWindow,
  liveLeaveURL,
  livePlaylistURL,
  pickInitialService,
  probeLivePlaylist,
  sendLiveLeaveHint,
  supportsNativeHls,
} from '@/lib/live'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('livePlaylistURL', () => {
  it('profile 無しは ?profile を付けない', () => {
    expect(livePlaylistURL('default', 0, 1024)).toBe(
      '/api/sites/default/networks/0/services/1024/live/playlist.m3u8',
    )
  })

  it('profile を渡すと ?profile= が付く', () => {
    expect(livePlaylistURL('default', 0, 1024, 'h264-720p')).toBe(
      '/api/sites/default/networks/0/services/1024/live/playlist.m3u8?profile=h264-720p',
    )
  })

  it('site をエスケープする', () => {
    expect(livePlaylistURL('a b', 0, 1)).toBe(
      '/api/sites/a%20b/networks/0/services/1/live/playlist.m3u8',
    )
  })

  // パスに載るのは SI の (networkId, serviceId) そのもの。合成 id
  // （networkId * 100000 + serviceId = 3192053248）を作るのは streamer 側で、
  // web はその規則を知らない（issue #217。合成をここで行っていたのが issue #208）。
  it('SI の networkId / serviceId をそのまま別セグメントに載せる', () => {
    const url = livePlaylistURL('default', 31920, 53248)
    expect(url).toBe('/api/sites/default/networks/31920/services/53248/live/playlist.m3u8')
    expect(url).not.toContain('3192053248')
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

describe('liveLeaveURL', () => {
  it('プレイリストと同じ (site, networkId, serviceId) の固定深さで、セッション ID を持たない', () => {
    // 期待値はリテラル（実装の式と比較すると何も主張しない）。
    expect(liveLeaveURL('default', 32736, 1024)).toBe(
      '/api/sites/default/networks/32736/services/1024/live/leave',
    )
    // プレイリストと同じ接頭辞（前段の consistent hash 鍵の取り出しが同じ
    // 正規表現で効く。docs/operations.md §5）
    expect(
      liveLeaveURL('default', 32736, 1024).startsWith(
        '/api/sites/default/networks/32736/services/1024/live/',
      ),
    ).toBe(true)
  })

  it('site をエスケープする', () => {
    expect(liveLeaveURL('a/b', 1, 2)).toBe('/api/sites/a%2Fb/networks/1/services/2/live/leave')
  })
})

describe('sendLiveLeaveHint', () => {
  it('sendBeacon があれば sendBeacon で送る（fetch は使わない）', () => {
    const beacon = vi.fn(() => true)
    vi.stubGlobal('navigator', { sendBeacon: beacon })
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    sendLiveLeaveHint('default', 32736, 1024)

    expect(beacon).toHaveBeenCalledWith('/api/sites/default/networks/32736/services/1024/live/leave')
    // ページ離脱の瞬間に投げるので、ドキュメント破棄で中断されうる fetch は使わない
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('sendBeacon が無い環境では keepalive つきの POST に落とす', async () => {
    vi.stubGlobal('navigator', {})
    const fetchMock = vi.fn(() => Promise.resolve(new Response(null, { status: 204 })))
    vi.stubGlobal('fetch', fetchMock)

    sendLiveLeaveHint('default', 32736, 1024)

    expect(fetchMock).toHaveBeenCalledWith('/api/sites/default/networks/32736/services/1024/live/leave', {
      method: 'POST',
      keepalive: true,
    })
  })

  it('送信に失敗しても投げない（離脱時の失敗は見せる画面がもう無い）', async () => {
    vi.stubGlobal('navigator', {})
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )

    expect(() => sendLiveLeaveHint('default', 1, 2)).not.toThrow()
    // 未処理の rejection にもしない（catch が付いていること）
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
})

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
    id: (overrides.networkId ?? 1) * 100_000 + (overrides.serviceId ?? 1),
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


describe('pickInitialService', () => {
  it('networkId + serviceId が両方一致するサービスを使う', () => {
    const services = [
      makeService({ networkId: 1, serviceId: 100 }),
      makeService({ networkId: 2, serviceId: 200 }),
    ]
    expect(pickInitialService(services, { networkId: 2, serviceId: 200 })).toBe(services[1])
  })

  /**
   * issue #291: SI の `serviceId` は network をまたぐと一意でない
   * （Mirakurun が合成 id を発明した理由そのもの）。同じ `serviceId` を 2 つの
   * network が持つ構成で、`networkId` まで指定すればその network のサービスが
   * 選ばれることを固定する --- `services.find((s) => s.serviceId === ...)` に
   * 戻す変異（`networkId` を無視する）だとこのテストは落ちる（先頭の network 1
   * が選ばれてしまう）。
   */
  it('同じ serviceId を 2 network が持つとき、networkId で正しい方を選ぶ', () => {
    const services = [
      makeService({ networkId: 1, serviceId: 100, name: 'network 1 の局' }),
      makeService({ networkId: 2, serviceId: 100, name: 'network 2 の局' }),
    ]
    expect(pickInitialService(services, { networkId: 2, serviceId: 100 })?.name).toBe(
      'network 2 の局',
    )
    expect(pickInitialService(services, { networkId: 1, serviceId: 100 })?.name).toBe(
      'network 1 の局',
    )
  })

  it('networkId が一致しない組では番組を持つ先頭にフォールバックする（無効な指定と同じ扱い）', () => {
    const services = [
      makeService({ networkId: 1, serviceId: 100, hasPrograms: false }),
      makeService({ networkId: 2, serviceId: 200, hasPrograms: true }),
    ]
    // network 9 に serviceId 100 は存在しない（network 1 のものと取り違えない）
    expect(pickInitialService(services, { networkId: 9, serviceId: 100 })).toBe(services[1])
  })

  // **networkId 単独では同定できないので serviceId ごと無視する。**
  // 「その serviceId を持つ最初のサービス」を返す実装だと、同じ id を持つ
  // 別 network のチャンネルを黙って選ぶ（issue #291 の根）。番組を持つ
  // 先頭という既定へ落ちることを、その serviceId の持ち主が先頭ではない
  // 並びで固定する。
  it('networkId 未指定なら serviceId は使わず、番組を持つ先頭へ落ちる', () => {
    const services = [
      makeService({ networkId: 1, serviceId: 100, name: 'serviceId 100 の持ち主', hasPrograms: false }),
      makeService({ networkId: 2, serviceId: 200, name: '番組を持つ先頭', hasPrograms: true }),
    ]
    expect(
      pickInitialService(services, { networkId: undefined, serviceId: 100 })?.name,
    ).toBe('番組を持つ先頭')
  })

  /**
   * `?networkId=` 単独（`serviceId` 無し）は `networkId` を無視する。この挙動を
   * 固定しておくと、将来「その network 内で選ぶ」に変えるのが意識的な変更になる
   * （レビューでの指摘）。実装が `services.find((s) => s.networkId === networkId)`
   * を先に試す形に変わればこのテストは落ちる。
   */
  it('networkId だけの指定は無視して番組を持つ先頭にフォールバックする', () => {
    const services = [
      makeService({ networkId: 1, serviceId: 100, hasPrograms: false, name: 'network 1 の局' }),
      makeService({ networkId: 2, serviceId: 200, hasPrograms: true, name: 'network 2 の局' }),
    ]
    // network 2 を指定しても「番組を持つ先頭」で決まる（たまたま network 2 が
    // 選ばれるのを合格と誤認しないよう、network 1 を指定した場合も同じ結果に
    // なることを見る）
    expect(pickInitialService(services, { networkId: 2, serviceId: undefined })?.name).toBe(
      'network 2 の局',
    )
    expect(pickInitialService(services, { networkId: 1, serviceId: undefined })?.name).toBe(
      'network 2 の局',
    )
  })

  it('要求した id が存在しなければ番組を持つ先頭にフォールバックする', () => {
    const services = [
      makeService({ serviceId: 1, hasPrograms: false }),
      makeService({ serviceId: 2, hasPrograms: true }),
    ]
    expect(
      pickInitialService(services, { networkId: undefined, serviceId: 999 }),
    ).toBe(services[1])
  })

  it('未指定でも番組を持つ先頭にフォールバックする', () => {
    const services = [
      makeService({ serviceId: 1, hasPrograms: false }),
      makeService({ serviceId: 2, hasPrograms: true }),
    ]
    expect(
      pickInitialService(services, { networkId: undefined, serviceId: undefined }),
    ).toBe(services[1])
  })

  it('番組を持つサービスが無ければ先頭を使う', () => {
    const services = [
      makeService({ serviceId: 5, hasPrograms: false }),
      makeService({ serviceId: 6, hasPrograms: false }),
    ]
    expect(
      pickInitialService(services, { networkId: undefined, serviceId: undefined }),
    ).toBe(services[0])
  })

  it('サービスが 1 件も無ければ undefined', () => {
    expect(pickInitialService([], { networkId: undefined, serviceId: 1 })).toBeUndefined()
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
