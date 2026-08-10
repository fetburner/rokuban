import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LivePlayer, nativeStallTimeoutMs } from '@/components/live-player'

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
    expect(video.src).toContain('/api/sites/default/services/1024/live/playlist.m3u8')
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
  async function renderNativePath() {
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
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

    const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0]?.[0]).toContain('/services/1024/')

    rerender(<LivePlayer site="default" networkId={0} serviceId={2048} />)
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
        expect.stringContaining('/api/sites/default/services/1024/live/playlist.m3u8'),
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
})
