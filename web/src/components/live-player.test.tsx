import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LivePlayer } from '@/components/live-player'

/**
 * hls.js 経路（Safari 以外のネイティブ HLS 非対応ブラウザ）の内部呼び出しを
 * jsdom で検査するためのフェイク。jsdom には MediaSource が無く、実 hls.js の
 * `Hls.isSupported()` は常に false を返すため、実物を動的 import すると
 * 「HLS 非対応」に落ちてしまい `attachMedia` 以降が一度も走らない。
 *
 * `vi.hoisted` で保持するインスタンス配列はテスト側からも参照できるので、
 * 「実際に `loadSource` / `attachMedia` が呼ばれたか」「fatal エラーで
 * `destroy` が呼ばれたか」を検査できる。ここで検査できるのは呼び出しの配線
 * だけで、実際のバンドル分割（動的 import が本当に別チャンクを読むか）と
 * 実再生（MSE への実データ投入）は `web/e2e/live.mjs` の役目（jsdom では
 * 原理的に測れない。CLAUDE.md「jsdom が測れないものは実装より先に判定手段を
 * 作る」）。
 */
const hlsMockState = vi.hoisted(() => ({
  instances: [] as FakeHls[],
  // supported はフェイクの `Hls.isSupported()` の戻り値。false にすると
  // 「MSE も ManagedMediaSource も無いブラウザ」（iOS 17.1 未満の iPhone Safari）を
  // 模擬できる。実 hls.js は jsdom でも常に false を返すが、それだと hls.js 経路
  // 自体が一度も走らないので既定は true にしておく
  supported: true,
}))

type FakeHls = {
  on: ReturnType<typeof vi.fn>
  loadSource: ReturnType<typeof vi.fn>
  attachMedia: ReturnType<typeof vi.fn>
  destroy: ReturnType<typeof vi.fn>
}

vi.mock('hls.js', () => {
  class FakeHlsImpl {
    static Events = { ERROR: 'hlsError' }
    static isSupported = () => hlsMockState.supported
    on = vi.fn()
    loadSource = vi.fn()
    attachMedia = vi.fn()
    destroy = vi.fn()
    constructor() {
      hlsMockState.instances.push(this as unknown as FakeHls)
    }
  }
  return { default: FakeHlsImpl }
})

/**
 * jsdom は `HTMLMediaElement.canPlayType` を実装していない（常に `''`）ため、
 * ネイティブ HLS 対応（Safari）を明示的に模擬するにはテスト側から差し替える必要がある。
 * 差し替えのタイミングは「effect が `probeLivePlaylist` の `await` に入った後・
 * 継続処理で読むより前」でなければならない --- コンポーネントは probe 成功後に
 * `canPlayType` を読むので、fetch を deferred にして制御する。
 */
function deferredFetch() {
  let resolve!: (response: Response) => void
  const promise = new Promise<Response>((r) => {
    resolve = r
  })
  vi.stubGlobal('fetch', vi.fn(() => promise))
  return { resolve }
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  hlsMockState.instances.length = 0
  hlsMockState.supported = true
})

describe('LivePlayer の状態遷移', () => {
  it('読み込み中は "読み込み中…" を出し、video は invisible', async () => {
    deferredFetch()
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(screen.getByText('読み込み中…')).toBeInTheDocument()
    const video = document.querySelector('video')!
    expect(video.className).toContain('invisible')
  })

  it('fetch が reject すると streamer 不在の文言を出す（destructive にしない）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    const message = await screen.findByText(/自宅サーバーが起動しているか/)
    expect(message).toBeInTheDocument()
    expect(message.className).not.toContain('text-destructive')
    // 読み込み中の表示は消えている
    expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument()
  })

  it('503 は本文をそのまま見せる（同時セッション上限 / チューナー枯渇）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(new Response('too many concurrent live sessions on this process', { status: 503 })),
      ),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(
      await screen.findByText('too many concurrent live sessions on this process'),
    ).toBeInTheDocument()
    expect(screen.getByText(/チューナー不足または同時視聴数の上限/)).toBeInTheDocument()
  })

  it('想定外のステータスも本文をそのまま見せる', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('internal error', { status: 500 }))),
    )
    render(<LivePlayer site="default" serviceId={1024} />)

    expect(await screen.findByText('internal error')).toBeInTheDocument()
    expect(screen.getByText('ライブ視聴でエラーが発生しました。')).toBeInTheDocument()
  })

  it('再読み込みボタンで probe をやり直す', async () => {
    const user = userEvent.setup()
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response('live stream unavailable', { status: 503 }))
      .mockResolvedValueOnce(new Response('live stream unavailable', { status: 503 }))
    vi.stubGlobal('fetch', fetchMock)

    render(<LivePlayer site="default" serviceId={1024} />)
    await screen.findByText('live stream unavailable')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await user.click(screen.getByRole('button', { name: '再読み込み' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })

  it('WebKit（Safari 相当）の実測値なら video.src に直接プレイリスト URL を渡し、hls.js を import しない', async () => {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" serviceId={1024} />)

    const video = document.querySelector('video')!
    // probe が解決する前に canPlayType を差し替える。**値は WebKit の実測値**
    // （lib/live.ts の supportsNativeHls の表）。`'probably'` を返す実ブラウザは
    // 無いので、そこで模擬すると実在しない状況をテストすることになる
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )

    resolve(new Response('', { status: 200 }))

    await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    expect(video.src).toContain('/api/sites/default/services/1024/live/playlist.m3u8')
    expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    // ネイティブ分岐では hls.js を import すらしない（約 520 KB を読ませない）
    expect(hlsMockState.instances).toHaveLength(0)
  })

  it('Chrome の実測値では hls.js 経路に入る（video.src に m3u8 を渡さない）', async () => {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" serviceId={1024} />)

    const video = document.querySelector('video')!
    // Chrome / Chromium はプレイリストの MIME に WebKit と同じ 'maybe' を返すが、
    // セグメント（video/mp2t）は空文字。ここが両者を分ける唯一の点
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' ? 'maybe' : '',
    )

    resolve(new Response('', { status: 200 }))

    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    expect(video.src).toBe('')
  })

  it('serviceId が変わると新しい URL で probe をやり直す', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    const { rerender } = render(<LivePlayer site="default" serviceId={1024} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0]?.[0]).toContain('/services/1024/')

    rerender(<LivePlayer site="default" serviceId={2048} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(fetchMock.mock.calls[1]?.[0]).toContain('/services/2048/')
  })

  it('破棄すると probe の in-flight fetch を AbortController で中断する', async () => {
    let capturedSignal: AbortSignal | undefined
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit) => {
        capturedSignal = init?.signal ?? undefined
        return new Promise<Response>(() => {
          /* 中断だけを見るテストなので解決しない */
        })
      }),
    )

    const { unmount } = render(<LivePlayer site="default" serviceId={1024} />)
    await waitFor(() => expect(capturedSignal).toBeDefined())
    expect(capturedSignal?.aborted).toBe(false)

    unmount()

    expect(capturedSignal?.aborted).toBe(true)
  })

  it('serviceId が変わると古い probe の in-flight fetch を中断する', async () => {
    const signals: AbortSignal[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn((_url: string, init?: RequestInit) => {
        if (init?.signal) signals.push(init.signal)
        return new Promise<Response>(() => {})
      }),
    )

    const { rerender } = render(<LivePlayer site="default" serviceId={1024} />)
    await waitFor(() => expect(signals).toHaveLength(1))
    expect(signals[0]?.aborted).toBe(false)

    rerender(<LivePlayer site="default" serviceId={2048} />)
    await waitFor(() => expect(signals).toHaveLength(2))

    // 古い（1024 向け）signal は中断済み、新しい（2048 向け）signal はまだ生きている
    expect(signals[0]?.aborted).toBe(true)
    expect(signals[1]?.aborted).toBe(false)
  })

  describe('hls.js 経路（ネイティブ HLS 非対応。Chrome / Firefox 相当）', () => {
    it('probe 成功後に動的 import → loadSource / attachMedia が呼ばれる', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" serviceId={1024} />)
      // jsdom の canPlayType は既定で '' を返すため、supportsNativeHls が false
      // になり hls.js 経路に入る（明示的な差し替えは不要）

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      expect(hls.loadSource).toHaveBeenCalledWith(
        expect.stringContaining('/api/sites/default/services/1024/live/playlist.m3u8'),
      )
      expect(hls.attachMedia).toHaveBeenCalledTimes(1)
      await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    })

    it('fatal エラーで hls インスタンスを破棄し、エラー文言を出す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      const errorCall = hls.on.mock.calls.find(([event]) => event === 'hlsError')
      expect(errorCall).toBeDefined()
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void

      errorHandler('hlsError', { fatal: true })

      expect(hls.destroy).toHaveBeenCalledTimes(1)
      expect(await screen.findByText('ライブ再生中にエラーが発生しました')).toBeInTheDocument()
    })

    it('non-fatal エラーは破棄もエラー表示もしない', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      const errorCall = hls.on.mock.calls.find(([event]) => event === 'hlsError')
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void

      errorHandler('hlsError', { fatal: false })

      expect(hls.destroy).not.toHaveBeenCalled()
      expect(screen.queryByText('ライブ再生中にエラーが発生しました')).not.toBeInTheDocument()
    })

    it('破棄（アンマウント）すると hls インスタンスが destroy される', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const { unmount } = render(<LivePlayer site="default" serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!

      unmount()

      expect(hls.destroy).toHaveBeenCalledTimes(1)
    })

    it('Hls.isSupported() が false でも m3u8 に支持があれば video.src へ渡す（MSE の無い iPhone Safari 相当）', async () => {
      // iOS 17.1 未満の iPhone Safari は `window.MediaSource` を持たない
      // （ManagedMediaSource も無い）ので `Hls.isSupported()` が false になる。
      // ここで「非対応」と断じると、**ネイティブなら完璧に再生できる端末**に
      // エラーを出すことになる（レビュー #190 の 2 回目の指摘）。
      // canPlayType は m3u8 にだけ支持を表明する形にして、rung 1（ネイティブ）を
      // 通り抜けて rung 3（最後の砦）に落ちる経路を作る
      hlsMockState.supported = false
      const { resolve } = deferredFetch()
      render(<LivePlayer site="default" serviceId={1024} />)

      const video = document.querySelector('video')!
      vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
        type === 'application/vnd.apple.mpegurl' ? 'maybe' : '',
      )

      resolve(new Response('', { status: 200 }))

      await waitFor(() =>
        expect(video.src).toContain('/api/sites/default/services/1024/live/playlist.m3u8'),
      )
      expect(
        screen.queryByText('このブラウザはライブ視聴（HLS）に対応していません'),
      ).not.toBeInTheDocument()
      expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument()
    })

    it('Hls.isSupported() が false で m3u8 にも支持が無ければ非対応を表示する', async () => {
      hlsMockState.supported = false
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      // jsdom の canPlayType は既定で '' を返す（= どの MIME にも支持が無い）
      render(<LivePlayer site="default" serviceId={1024} />)

      expect(
        await screen.findByText('このブラウザはライブ視聴（HLS）に対応していません'),
      ).toBeInTheDocument()
      expect(document.querySelector('video')!.src).toBe('')
    })

    it('serviceId が変わると古い hls インスタンスが destroy され、新しいインスタンスが作られる', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const { rerender } = render(<LivePlayer site="default" serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const first = hlsMockState.instances[0]!

      rerender(<LivePlayer site="default" serviceId={2048} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(first.destroy).toHaveBeenCalledTimes(1)
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        expect.stringContaining('/services/2048/'),
      )
    })
  })
})
