import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LivePlayer, nativeStallTimeoutMs } from '@/components/live-player'
import type { LiveDiagnostics } from '@/lib/live'

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
  // 計器（issue #476）。既定値はライブ同期点が決まる前の実 hls.js の状態
  // ---`LatencyController.get latency()` は `this._latency || 0` を返すため
  // `NaN` ではなく `0`（`node_modules/hls.js` 1.7.1 で確認済み。レビュー
  // 指摘）。mainForwardBufferInfo はアタッチ直後は null
  latency: number
  mainForwardBufferInfo: { len: number } | null
}

vi.mock('hls.js', () => {
  class FakeHlsImpl {
    static Events = { ERROR: 'hlsError' }
    static isSupported = () => hlsMockState.supported
    on = vi.fn()
    loadSource = vi.fn()
    attachMedia = vi.fn()
    // 破棄後は `latency` / `mainForwardBufferInfo` を読むと例外にする ---
    // **実物より厳しい観測点**。実 hls.js は destroy 後に読んでも例外を
    // 投げない（`LatencyController.destroy()` は内部の `hls` 参照を `null`
    // にするだけで `_latency` は直前値のまま残る。`node_modules/hls.js`
    // 1.7.1 で確認済み）。ここでは canary として実物より厳しく throw させ、
    // 「破棄後は読み続けない」衛生を止め忘れたら `vi.advanceTimersByTime` から
    // 例外が漏れて落ちるようにしてある（issue #476 レビュー指摘）
    #destroyed = false
    #latency = 0
    #mainForwardBufferInfo: { len: number } | null = null
    destroy = vi.fn(() => {
      this.#destroyed = true
    })
    get latency() {
      if (this.#destroyed) throw new Error('destroy 後の hls インスタンスの latency を読んだ')
      return this.#latency
    }
    set latency(value: number) {
      this.#latency = value
    }
    get mainForwardBufferInfo() {
      if (this.#destroyed) {
        throw new Error('destroy 後の hls インスタンスの mainForwardBufferInfo を読んだ')
      }
      return this.#mainForwardBufferInfo
    }
    set mainForwardBufferInfo(value: { len: number } | null) {
      this.#mainForwardBufferInfo = value
    }
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
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  hlsMockState.instances.length = 0
  hlsMockState.supported = true
})

describe('LivePlayer の状態遷移', () => {
  it('読み込み中は "読み込み中…" を出し、video は invisible', async () => {
    deferredFetch()
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

    expect(screen.getByText('読み込み中…')).toBeInTheDocument()
    const video = document.querySelector('video')!
    expect(video.className).toContain('invisible')
  })

  it('fetch が reject すると streamer 不在の文言を出す（destructive にしない）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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

    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    await screen.findByText('live stream unavailable')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await user.click(screen.getByRole('button', { name: '再読み込み' }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })

  it('WebKit（Safari 相当）の実測値なら video.src に直接プレイリスト URL を渡し、hls.js を import しない', async () => {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

    const video = document.querySelector('video')!
    // probe が解決する前に canPlayType を差し替える。**値は WebKit の実測値**
    // （lib/live.ts の supportsNativeHls の表）。`'probably'` を返す実ブラウザは
    // 無いので、そこで模擬すると実在しない状況をテストすることになる
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )

    resolve(new Response('', { status: 200 }))

    await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    expect(video.src).toContain('/api/sites/default/networks/0/services/1024/live/playlist.m3u8')
    expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    // ネイティブ分岐では hls.js を import すらしない（約 520 KB を読ませない）
    expect(hlsMockState.instances).toHaveLength(0)
  })

  it('Chrome の実測値では hls.js 経路に入る（video.src に m3u8 を渡さない）', async () => {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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

  /**
   * ネイティブ HLS 経路（WebKit 相当）まで進めて `<video>` を返す。
   *
   * **probe は 200 で通る。壊れているのはメディア層だけ**という状況を作るための
   * 足場（`web/e2e/live.mjs` ⑦が実 WebKit で見ているのと同じ状況）。
   */
  async function renderNativePath(onDiagnostics?: (diagnostics: LiveDiagnostics | null) => void) {
    const { resolve } = deferredFetch()
    render(
      <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(video.src).toContain('playlist.m3u8'))
    // jsdom の `paused` は既定 true（再生が始まらないため）。ここで測りたいのは
    // **再生中に配信が途絶えた**ときの挙動なので、明示的に「再生中」にしておく。
    // 一時停止中の挙動は専用のテストが `true` に差し替えて確かめる
    Object.defineProperty(video, 'paused', { value: false, configurable: true })
    return video
  }

  describe('ネイティブ経路のメディア失敗（probe は 200 だが再生できない）', () => {
    // ここが無いと、ネイティブ経路の失敗は**永久に止まった黒いプレイヤー**に
    // なる（文言も読み込み表示も再読み込みボタンも出ない。レビュー #190 の
    // 3 回目の指摘で WebKit で実測された症状）。聴くイベントの選択は同じ実測に
    // 基づく: セグメント 404 は `error`、セグメントが応答しない / プレイリストの
    // 中身が壊れている場合は `error` が出ず `stalled` だけが出る

    it('error イベントでエラー表示と再読み込みボタンを出す', async () => {
      const video = await renderNativePath()

      await act(async () => {
        video.dispatchEvent(new Event('error'))
      })

      expect(await screen.findByText(/映像データを読み込めません/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '再読み込み' })).toBeInTheDocument()
    })

    it('stalled のまま猶予が過ぎるとエラー表示を出す（error が出ない壊れ方）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
      })
      // 猶予の間はまだエラーにしない（ライブは正常時にも一時的に止まる）
      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()

      await act(async () => {
        vi.advanceTimersByTime(nativeStallTimeoutMs)
      })

      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '再読み込み' })).toBeInTheDocument()
    })

    it('一時停止中の stalled はエラーにしない（WebKit は pause した瞬間に stalled を出す）', async () => {
      // WebKit は一時停止するとフェッチを止めるため `stalled` を出すが、配信は
      // 正常なまま。解除イベント（playing / canplay / timeupdate）も来ないので、
      // paused を見ないと猶予が必ず満了してエラー画面が出る --- しかも <video> が
      // invisible になり、ユーザーが一時停止した映像そのものが隠れる。
      // レビュー #190 の 4 回目の指摘（WebKit で実測）
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        // 一度は再生が始まっている。**ここが要る** --- 抑止したいのは「再生して
        // いたユーザーが自分で止めた」場合だけで、再生前の停止は抑止しない
        video.dispatchEvent(new Event('playing'))
        Object.defineProperty(video, 'paused', { value: true, configurable: true })
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(nativeStallTimeoutMs * 2)
      })

      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })

    it('再生前（まだ一度も再生していない）の stalled は猶予が過ぎたらエラーにする', async () => {
      // `<video>` に autoPlay は無いので、読み込み直後は常に paused === true。
      // 抑止条件を `paused` だけにすると**ここが塞がる** --- プレイリストは 200 だが
      // セグメントが無応答のとき、実 WebKit で届くイベントは
      // loadstart → progress → `paused=true` の stalled だけで、20 秒待っても
      // error も waiting も来ない。つまり猶予が一度も張られず永久に黒いままになる。
      // これは `web/e2e/live.mjs` ⑦（無応答側）が見ている条件そのもの ---
      // ⑦は play() を呼ばないため、この退行は e2e でも NG になる。
      // レビュー #190 の 5 回目の指摘（実 WebKit で測定）
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()
      Object.defineProperty(video, 'paused', { value: true, configurable: true })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(nativeStallTimeoutMs)
      })

      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '再読み込み' })).toBeInTheDocument()
    })

    it('再生中に一時停止されると猶予が満了してもエラーにしない（猶予中に pause した場合）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        // 再生中に stall（ここでタイマーが張られる）
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(nativeStallTimeoutMs / 2)
        // 猶予の途中でユーザーが一時停止した
        Object.defineProperty(video, 'paused', { value: true, configurable: true })
        video.dispatchEvent(new Event('pause'))
        vi.advanceTimersByTime(nativeStallTimeoutMs * 2)
      })

      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
    })

    it('再開後に配信が復帰していなければ再びエラーになる（一時停止で猶予を捨てた後の張り直し）', async () => {
      // 上 2 つの抑止が「以後ずっと検出しない」に化けていないことを見る。
      // ブラウザ側が再開時に `waiting` を再送することは実 WebKit で測った
      // （stall 中に pause@6.05s → play@12.05s → waiting@12.05s）
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        video.dispatchEvent(new Event('playing'))
        video.dispatchEvent(new Event('stalled'))
        Object.defineProperty(video, 'paused', { value: true, configurable: true })
        video.dispatchEvent(new Event('pause'))
        vi.advanceTimersByTime(nativeStallTimeoutMs * 2)
      })
      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()

      await act(async () => {
        // ユーザーが再開した。配信は死んだままなので再び waiting が来る
        Object.defineProperty(video, 'paused', { value: false, configurable: true })
        video.dispatchEvent(new Event('play'))
        video.dispatchEvent(new Event('waiting'))
        vi.advanceTimersByTime(nativeStallTimeoutMs)
      })

      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
    })

    it('猶予中に playing が来れば回復と見なしてエラーにしない（逆向き）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(nativeStallTimeoutMs / 2)
        video.dispatchEvent(new Event('playing'))
        vi.advanceTimersByTime(nativeStallTimeoutMs * 2)
      })

      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })
  })

  it('serviceId が変わると新しい URL で probe をやり直す', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    // probe だけを数える。jsdom には `navigator.sendBeacon` が無いので、離脱ヒント
    // （issue #191）はこの同じ fetch モックに POST として現れる --- 全呼び出しを
    // 数えると probe の数え上げに混ざる（ヒント自体の検証は下の describe）
    const probeURLs = () =>
      fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))

    const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    await waitFor(() => expect(probeURLs()).toHaveLength(1))
    expect(probeURLs()[0]).toContain('/services/1024/')

    rerender(<LivePlayer site="default" networkId={0} serviceId={2048} />)
    await waitFor(() => expect(probeURLs()).toHaveLength(2))
    expect(probeURLs()[1]).toContain('/services/2048/')
  })

  it('破棄すると probe の in-flight fetch を AbortController で中断する', async () => {
    let capturedSignal: AbortSignal | undefined
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string, init?: RequestInit) => {
        // probe（プレイリストの GET）の signal だけを見る。アンマウントでは
        // 離脱ヒント（issue #191）の POST も同じモックに来るが、あちらは signal を
        // 持たない（`keepalive` で投げっぱなしにする）ので上書きさせない
        if (String(url).includes('playlist.m3u8')) capturedSignal = init?.signal ?? undefined
        return new Promise<Response>(() => {
          /* 中断だけを見るテストなので解決しない */
        })
      }),
    )

    const { unmount } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
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

    const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    await waitFor(() => expect(signals).toHaveLength(1))
    expect(signals[0]?.aborted).toBe(false)

    rerender(<LivePlayer site="default" networkId={0} serviceId={2048} />)
    await waitFor(() => expect(signals).toHaveLength(2))

    // 古い（1024 向け）signal は中断済み、新しい（2048 向け）signal はまだ生きている
    expect(signals[0]?.aborted).toBe(true)
    expect(signals[1]?.aborted).toBe(false)
  })

  describe('hls.js 経路（ネイティブ HLS 非対応。Chrome / Firefox 相当）', () => {
    it('probe 成功後に動的 import → loadSource / attachMedia が呼ばれる', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      // jsdom の canPlayType は既定で '' を返すため、supportsNativeHls が false
      // になり hls.js 経路に入る（明示的な差し替えは不要）

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      expect(hls.loadSource).toHaveBeenCalledWith(
        expect.stringContaining('/api/sites/default/networks/0/services/1024/live/playlist.m3u8'),
      )
      expect(hls.attachMedia).toHaveBeenCalledTimes(1)
      await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    })

    it('fatal エラーで hls インスタンスを破棄し、エラー文言を出す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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
      const { unmount } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

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
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

      const video = document.querySelector('video')!
      vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
        type === 'application/vnd.apple.mpegurl' ? 'maybe' : '',
      )

      resolve(new Response('', { status: 200 }))

      await waitFor(() =>
        expect(video.src).toContain('/api/sites/default/networks/0/services/1024/live/playlist.m3u8'),
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
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

      expect(
        await screen.findByText('このブラウザはライブ視聴（HLS）に対応していません'),
      ).toBeInTheDocument()
      expect(document.querySelector('video')!.src).toBe('')
    })

    it('hls.js 経路では stalled を拾わない（MSE のバッファ制御で正常時にも出るため）', async () => {
      // ネイティブ経路のメディア監視を hls.js 経路にも張ると、正常な再生中の
      // バッファ待ちを「途絶えた」と誤検知する。**張らないこと**を固定する
      // （変異: `watchNativeMedia(video)` を hls.js 分岐にも足すとこのテストが落ちる）
      vi.useFakeTimers({ shouldAdvanceTime: true })
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(nativeStallTimeoutMs * 2)
      })

      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })

    it('serviceId が変わると古い hls インスタンスが destroy され、新しいインスタンスが作られる', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const first = hlsMockState.instances[0]!

      rerender(<LivePlayer site="default" networkId={0} serviceId={2048} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(first.destroy).toHaveBeenCalledTimes(1)
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        expect.stringContaining('/services/2048/'),
      )
    })
  })

  /**
   * 計器（issue #476）。「放送から n 秒 / 先読み n 秒」の値を 1 秒ごとに
   * `onDiagnostics` コールバック prop で親へ渡す。表示（テキストの組み立て・
   * DOM への描画）は `pages/live.tsx` 側が担うので、ここでは渡す値そのものを
   * 検査する。
   */
  describe('計器（issue #476）', () => {
    it('表示開始直後は「まだ計測していない」（latencySec / bufferSec が null）', async () => {
      const onDiagnostics = vi.fn()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      expect(onDiagnostics).toHaveBeenCalledWith({ source: 'hls', latencySec: null, bufferSec: null })
    })

    /**
     * **`hls.latency` は同期点が決まる前も `NaN` ではなく `0` を返す**
     * （`LatencyController.get latency()` が `this._latency || 0`。
     * `node_modules/hls.js` 1.7.1 で確認済み。レビュー指摘）。`0` を
     * そのまま「計測済みの遅延ゼロ」として渡すと、実ブラウザでは再生ボタンを
     * 押すまで「放送から約0秒」という偽の値が出続ける（修正前の実装の欠陥）。
     * フェイクの既定値は実物と同じ `0` のままにしてあるので、
     * `readHlsDiagnostics` の `hls.latency > 0` ガードを
     * `Number.isFinite(hls.latency)` に戻す変異でこのテストが落ちることを
     * 確認済み（`0` は finite なので通ってしまい、`latencySec` が `0` のまま
     * 報告される）。
     */
    it('latency が 0 のまま（同期点未確定）でも latencySec は null のまま報告する', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      await act(async () => {
        vi.advanceTimersByTime(3000)
      })

      // 1 回目は effect リセットの `onDiagnostics(null)`。以降は 1 秒ごとの
      // 計測値なので、それらすべてで latencySec が null であることを見る
      const measured = onDiagnostics.mock.calls.map(([d]) => d).filter((d) => d !== null)
      expect(measured.length).toBeGreaterThan(0)
      for (const diagnostics of measured) {
        expect(diagnostics.latencySec).toBeNull()
      }
    })

    it('1 秒ごとに hls.latency / mainForwardBufferInfo.len を読み、正の値を報告する', async () => {
      // 変異: watchLiveDiagnostics の setInterval を呼ばない（1 回しか読まない）
      // ようにするとこのテストが落ちる（呼び出しが 1 回のまま、正の値が来ない）
      // ことを確認済み
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      hls.latency = 3.4
      hls.mainForwardBufferInfo = { len: 5.6 }

      await act(async () => {
        vi.advanceTimersByTime(1000)
      })

      expect(onDiagnostics).toHaveBeenLastCalledWith({
        source: 'hls',
        latencySec: 3.4,
        bufferSec: 5.6,
      })
    })

    it('fatal エラーで hls を破棄した後は destroy 済みインスタンスの latency を読み続けない', async () => {
      // フェイクの latency / mainForwardBufferInfo は destroy 後に読むと例外を
      // 投げる --- **実物より厳しい観測点**（実 hls.js は destroy 後も例外を
      // 投げず直前値を返し続ける。上の FakeHlsImpl のコメント参照）。ここでの
      // 目的は例外対策の検証ではなく、意味の無くなった値を毎秒読み続けない
      // 衛生（stopDiagnostics の呼び出し）を canary で固定すること ---
      // 変異: `stopDiagnostics()` の呼び出しを削除するとこのテストが実際に
      // 落ちることを確認済み
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      hls.latency = 3
      hls.mainForwardBufferInfo = { len: 5 }
      await act(async () => {
        vi.advanceTimersByTime(1000)
      })
      expect(onDiagnostics).toHaveBeenLastCalledWith({ source: 'hls', latencySec: 3, bufferSec: 5 })

      const errorCall = hls.on.mock.calls.find(([event]) => event === 'hlsError')
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void
      await act(async () => {
        errorHandler('hlsError', { fatal: true })
      })
      expect(hls.destroy).toHaveBeenCalledTimes(1)

      // 破棄後に 3 秒分（3 回）タイマーを進めても例外にならない
      await expect(
        act(async () => {
          vi.advanceTimersByTime(3000)
        }),
      ).resolves.not.toThrow()
    })

    /**
     * 表示位置を `pages/live.tsx`（ON AIR バッジの隣）へ戻した際に入り込んだ
     * 回帰（レビュー指摘）。呼び出し側は `isPlaying && diagnostics` でしか
     * 出し分けておらず、エラー表示自体は知らない。`stopDiagnostics` が
     * ポーリングを止めるだけで最後の値を残したままだと、fatal エラーで
     * プレイヤーが「エラーが発生しました」を出している間も、ON AIR バッジの
     * 隣に直前の測定値（「放送から約3秒」等）が凍ったまま出続ける。
     *
     * 既存の「fatal エラーで hls を破棄した後は...latency を読み続けない」
     * テストは destroy 後に**例外が出ないか**しか見ていないため、この回帰は
     * 検出できない（`onDiagnostics` の最後の呼び出し引数までは見ていない）。
     *
     * 変異: `stop` から `onDiagnosticsRef.current?.(null)` を削除すると
     * このテストが実際に落ちることを確認済み（最後の呼び出しが
     * `{ source: 'hls', latencySec: 3, bufferSec: 5 }` のままになる）。
     */
    it('fatal エラーで hls を破棄すると計器を null で報告する（凍ったまま残さない）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onDiagnostics={onDiagnostics} />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const hls = hlsMockState.instances[0]!
      hls.latency = 3
      hls.mainForwardBufferInfo = { len: 5 }
      await act(async () => {
        vi.advanceTimersByTime(1000)
      })
      expect(onDiagnostics).toHaveBeenLastCalledWith({ source: 'hls', latencySec: 3, bufferSec: 5 })

      const errorCall = hls.on.mock.calls.find(([event]) => event === 'hlsError')
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void
      await act(async () => {
        errorHandler('hlsError', { fatal: true })
      })

      expect(onDiagnostics).toHaveBeenLastCalledWith(null)
    })

    it('ネイティブ経路では latencySec が常に null（latency は取得できない）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      const video = await renderNativePath(onDiagnostics)
      Object.defineProperty(video, 'buffered', {
        value: { length: 1, end: () => 10 },
        configurable: true,
      })
      video.currentTime = 4

      await act(async () => {
        vi.advanceTimersByTime(1000)
      })

      expect(onDiagnostics).toHaveBeenLastCalledWith({
        source: 'native',
        latencySec: null,
        bufferSec: 6,
      })
    })

    /**
     * ネイティブ経路の `failed()` も同じ回帰を持つ（レビュー指摘）。
     * 変異: `stop` から `onDiagnosticsRef.current?.(null)` を削除すると
     * このテストが実際に落ちることを確認済み。
     */
    it('ネイティブ経路のメディア失敗（error）に落ちると計器を null で報告する（凍ったまま残さない）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      const video = await renderNativePath(onDiagnostics)
      Object.defineProperty(video, 'buffered', {
        value: { length: 1, end: () => 8 },
        configurable: true,
      })
      video.currentTime = 0
      await act(async () => {
        vi.advanceTimersByTime(1000)
      })
      expect(onDiagnostics).toHaveBeenLastCalledWith({
        source: 'native',
        latencySec: null,
        bufferSec: 8,
      })

      await act(async () => {
        video.dispatchEvent(new Event('error'))
      })
      expect(await screen.findByText(/映像データを読み込めません/)).toBeInTheDocument()

      expect(onDiagnostics).toHaveBeenLastCalledWith(null)
    })

    it('ネイティブ経路のメディア失敗（error）に落ちると計器のポーリングが止まる', async () => {
      // nit: watchNativeMedia の failed() からも stopDiagnostics を呼ぶ
      // （issue #476 レビュー指摘）。呼ばなくてもリークはしないが、エラー中も
      // 毎秒報告し続ける理由が無い --- 変異: failed() の stopDiagnostics()
      // 呼び出しを削除するとこのテストが実際に落ちることを確認済み
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onDiagnostics = vi.fn()
      const video = await renderNativePath(onDiagnostics)

      await act(async () => {
        video.dispatchEvent(new Event('error'))
      })
      expect(await screen.findByText(/映像データを読み込めません/)).toBeInTheDocument()

      onDiagnostics.mockClear()
      await act(async () => {
        vi.advanceTimersByTime(5000)
      })
      expect(onDiagnostics).not.toHaveBeenCalled()
    })
  })

  /**
   * 離脱ヒント（issue #191）。**送ったかどうかは `navigator.sendBeacon` の
   * 呼び出しで見る** --- jsdom は `sendBeacon` を実装していないので、テスト側で
   * 差し替えたものが呼ばれれば「実ブラウザで beacon 経路に入る」配線の確認になる
   * （実 beacon が本当にサーバーへ届くことは jsdom では測れない。`web/e2e/live.mjs`
   * ⑧が実ブラウザで見る）。
   */
  describe('離脱ヒント（issue #191）', () => {
    /** stubBeacon は `navigator.sendBeacon` を差し替え、送信先 URL を記録する。 */
    function stubBeacon(): string[] {
      const sent: string[] = []
      vi.stubGlobal('navigator', {
        sendBeacon: (url: string) => {
          sent.push(url)
          return true
        },
      })
      return sent
    }

    /** playing は probe が通ってプレイヤーが立ち上がるまで待つ（空虚な成功を防ぐ）。 */
    async function waitForPlaying() {
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    }

    it('アンマウント（再生停止・画面遷移）でヒントを送る', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      const { unmount } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await waitForPlaying()
      expect(sent).toHaveLength(0)

      unmount()

      expect(sent).toEqual(['/api/sites/default/networks/0/services/1024/live/leave'])
    })

    it('チャンネル切り替えでは「離れた側」の serviceId にヒントを送る', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await waitForPlaying()

      rerender(<LivePlayer site="default" networkId={0} serviceId={2048} />)

      // 新しい方（2048）に送ってはならない --- それは今から見るチャンネルである
      expect(sent).toEqual(['/api/sites/default/networks/0/services/1024/live/leave'])
    })

    it('pagehide でヒントを送る（モバイル Safari では unload が発火しない）', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await waitForPlaying()

      window.dispatchEvent(new Event('pagehide'))

      expect(sent).toEqual(['/api/sites/default/networks/0/services/1024/live/leave'])
    })

    it('visibilitychange は hidden のときだけ送る（両方向）', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await waitForPlaying()

      const visibility = vi.spyOn(document, 'visibilityState', 'get')

      // 復帰（visible）では送らない。送ると、タブに戻るたびに自分の視聴の
      // idle 期限を詰めることになる
      visibility.mockReturnValue('visible')
      document.dispatchEvent(new Event('visibilitychange'))
      expect(sent).toHaveLength(0)

      visibility.mockReturnValue('hidden')
      document.dispatchEvent(new Event('visibilitychange'))
      expect(sent).toEqual(['/api/sites/default/networks/0/services/1024/live/leave'])
    })

    it('「再読み込み」では送らない（離脱ではないので、メトリクスに混ぜない）', async () => {
      const user = userEvent.setup()
      vi.stubGlobal(
        'fetch',
        vi
          .fn()
          .mockResolvedValueOnce(new Response('busy', { status: 503 }))
          .mockResolvedValue(new Response('', { status: 200 })),
      )
      const sent = stubBeacon()
      render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await screen.findByRole('button', { name: '再読み込み' })

      await user.click(screen.getByRole('button', { name: '再読み込み' }))
      await waitForPlaying()

      expect(sent).toEqual([])
    })

    it('アンマウント後のイベントでは送らない（リスナが外れている）', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      const { unmount } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
      await waitForPlaying()
      unmount()
      expect(sent).toHaveLength(1)

      window.dispatchEvent(new Event('pagehide'))
      document.dispatchEvent(new Event('visibilitychange'))

      expect(sent).toHaveLength(1)
    })
  })
})

describe('LivePlayer のキー操作', () => {
  it('M でミュートし、F でフルスクリーンにする', () => {
    deferredFetch()
    const { container } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    const video = container.querySelector('video')!
    const requestFullscreen = vi.fn(() => Promise.resolve())
    Object.defineProperty(video, 'requestFullscreen', { value: requestFullscreen })

    fireEvent.keyDown(window, { key: 'm' })
    fireEvent.keyDown(window, { key: 'F' })

    expect(video.muted).toBe(true)
    expect(requestFullscreen).toHaveBeenCalledOnce()
  })

  it('修飾キー付きのブラウザ・OS ショートカットを横取りしない', () => {
    deferredFetch()
    const { container } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    const video = container.querySelector('video')!
    const requestFullscreen = vi.fn(() => Promise.resolve())
    Object.defineProperty(video, 'requestFullscreen', { value: requestFullscreen })

    fireEvent.keyDown(window, { key: 'f', metaKey: true })
    fireEvent.keyDown(window, { key: 'm', ctrlKey: true })
    fireEvent.keyDown(window, { key: 'f', altKey: true })

    expect(requestFullscreen).not.toHaveBeenCalled()
    expect(video.muted).toBe(false)
  })

  it('入力欄からの M とライブ対象外のシークキーは無視する', () => {
    deferredFetch()
    const { container } = render(
      <div>
        <input aria-label="検索" />
        <LivePlayer site="default" networkId={0} serviceId={1024} />
      </div>,
    )
    const video = container.querySelector('video')!
    const input = container.querySelector('input')!
    Object.defineProperty(video, 'currentTime', { value: 50, writable: true })

    fireEvent.keyDown(input, { key: 'm' })
    fireEvent.keyDown(window, { key: 'ArrowRight' })

    expect(video.muted).toBe(false)
    expect(video.currentTime).toBe(50)
  })
})
