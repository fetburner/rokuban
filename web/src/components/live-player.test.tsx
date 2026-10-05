import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { LivePlayer } from '@/components/live-player'
import { FRAME_SECONDS } from '@/lib/chapters'
import { liveStallTimeoutMs, originalVODSessionOriginSeconds } from '@/lib/live'
import type { LiveDiagnostics, StallHandling } from '@/lib/live'
import { saveChapterEditPlaybackRate, savePlaybackRate } from '@/lib/playback-position'

function openPlaybackSettingsSubmenu(label: '画質' | '音声') {
  if (!screen.queryByRole('menu', { name: '再生設定' })) {
    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
  }
  const settings = screen.getByRole('menu', { name: '再生設定' })
  fireEvent.click(within(settings).getByRole('menuitem', { name: label }))
  return screen.getByRole('menu', { name: label })
}

function choosePlaybackOption(label: '画質' | '音声', option: RegExp) {
  const submenu = openPlaybackSettingsSubmenu(label)
  fireEvent.click(within(submenu).getByRole('menuitemradio', { name: option }))
}

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
  constructorArgs: [] as unknown[][],
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
  audioTrack: number
  audioTracks: unknown[]
}

vi.mock('hls.js', () => {
  class FakeHlsImpl {
    static Events = {
      ERROR: 'hlsError',
      AUDIO_TRACKS_UPDATED: 'hlsAudioTracksUpdated',
      LEVEL_LOADED: 'hlsLevelLoaded',
    }
    static isSupported = () => hlsMockState.supported
    on = vi.fn()
    // 音声トラック（issue #870）。実 hls.js は master の音声グループを読むまで空
    audioTrack = 0
    audioTracks: unknown[] = []
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
    constructor(...args: unknown[]) {
      hlsMockState.instances.push(this as unknown as FakeHls)
      hlsMockState.constructorArgs.push(args)
    }
  }
  return { default: FakeHlsImpl }
})

/**
 * PROFILE_MASTER は captions 無効時に streamer が返すプロファイルごとの master
 * （音声レンディション入り。issue #870）の形で、video variant は 1 本だけ。
 * ffmpeg 9.0 の `-var_stream_map "v:0,agroup:aud a:0,agroup:aud ..."` の出力を写した。
 * BUNDLED_MASTER は `live.captions: true` の全プロファイルを束ねた master。
 */
const PROFILE_MASTER =
  '#EXTM3U\n#EXT-X-VERSION:3\n' +
  '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="group_aud",NAME="audio_1",DEFAULT=YES,URI="hd.1.m3u8"\n' +
  '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="group_aud",NAME="audio_2",DEFAULT=NO,URI="hd.2.m3u8"\n' +
  '#EXT-X-STREAM-INF:BANDWIDTH=2000000,AUDIO="group_aud"\nhd.0.m3u8\n'
const BUNDLED_MASTER =
  '#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nplaylist_0.m3u8\n' +
  '#EXT-X-STREAM-INF:BANDWIDTH=800000\nplaylist_1.m3u8\n'

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
  localStorage.clear()
  hlsMockState.instances.length = 0
  hlsMockState.constructorArgs.length = 0
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

  it('追っかけ入力の502は容量不足と表示せず、同じ失敗を自動再試行しない', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(
        new Response(
          '追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。',
          { status: 502, headers: { 'Retry-After': '5' } },
        ),
      ),
    )
    vi.stubGlobal('fetch', fetchMock)
    render(<LivePlayer mode="chase" site="default" recordingId={42} />)

    expect(
      await screen.findByText('追っかけ再生の入力に失敗したため、一時停止しています。'),
    ).toBeInTheDocument()
    expect(
      screen.getByText(
        '追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText(/チューナー不足または同時視聴数の上限/)).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('Retry-After の無い追っかけの502（プロキシ由来）は入力失敗と表示しない', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('Bad Gateway from proxy', { status: 502 }))),
    )
    render(<LivePlayer mode="chase" site="default" recordingId={42} />)

    expect(await screen.findByText('Bad Gateway from proxy')).toBeInTheDocument()
    expect(
      screen.queryByText('追っかけ再生の入力に失敗したため、一時停止しています。'),
    ).not.toBeInTheDocument()
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
  async function renderNativePath(
    options: {
      onDiagnostics?: (diagnostics: LiveDiagnostics | null) => void
      onStalled?: () => StallHandling
      /** probe が返す本文。既定は variant playlist（master ではない）。 */
      body?: string
    } = {},
  ) {
    const { resolve } = deferredFetch()
    render(
      <LivePlayer
        site="default"
        networkId={0}
        serviceId={1024}
        onDiagnostics={options.onDiagnostics}
        onStalled={options.onStalled}
      />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response(options.body ?? '', { status: 200 }))
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
        vi.advanceTimersByTime(liveStallTimeoutMs)
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
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
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
        vi.advanceTimersByTime(liveStallTimeoutMs)
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
        vi.advanceTimersByTime(liveStallTimeoutMs / 2)
        // 猶予の途中でユーザーが一時停止した
        Object.defineProperty(video, 'paused', { value: true, configurable: true })
        video.dispatchEvent(new Event('pause'))
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
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
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
      })
      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()

      await act(async () => {
        // ユーザーが再開した。配信は死んだままなので再び waiting が来る
        Object.defineProperty(video, 'paused', { value: false, configurable: true })
        video.dispatchEvent(new Event('play'))
        video.dispatchEvent(new Event('waiting'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
    })

    it('猶予中に playing が来れば回復と見なしてエラーにしない（逆向き）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const video = await renderNativePath()

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs / 2)
        video.dispatchEvent(new Event('playing'))
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
      })

      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })
  })

  describe('停滞したときの画質の自動降格（issue #871）', () => {
    /**
     * ネイティブ経路: 猶予が満了したときに**まず降格を試す**。
     *
     * 変異: `onStalledRef.current?.() === true` の分岐を外す（常に `failed()` へ
     * 落とす）と、このテストの「エラーが出ない」が落ちる。
     */
    it('ネイティブ経路: 猶予が満了したら呼び出し側に降格を試させ、引き取られたらエラーにしない', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      const video = await renderNativePath({ onStalled })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
      expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })

    /**
     * ネイティブ経路: **段が尽きたときは現行と同一の挙動**（新しい文言・新しい
     * 経路を作らない）。`false` は「呼び出し側が下げられなかった」の意味である。
     */
    it('ネイティブ経路: 引き取られなければ従来どおりエラー表示にする（段が尽きたとき）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => false)
      const video = await renderNativePath({ onStalled })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: '再読み込み' })).toBeInTheDocument()
    })

    /**
     * ネイティブ経路の `'wait'`（一覧がまだ届いていない）は**現行どおりの
     * エラー文言に落ちる**。
     *
     * **ここで `return` してはいけない。** このタイマーは `stalled` / `waiting`
     * でしか張り直せず、実 WebKit の無応答の配信ではそのイベントが再発火しない
     * （実測: loadstart → progress → `stalled` のあと 20 秒待っても来ない）。
     * `return` すると**その再生では降格もエラー表示も起きず黒いまま何も出ない**。
     * hls.js 経路は 1 秒ごとの刻みがあるので `'wait'` で待ち続けられる（そちらは
     * 別のテストが見る）。
     */
    it('ネイティブ経路: 一覧が未着（wait）なら現行どおりエラーに落ちる', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => 'wait' as const)
      const video = await renderNativePath({ onStalled })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
    })

    /**
     * **降格のあと `canplay` が来ない（降格先も死んでいる）とき、停滞の検出を
     * 止めない。**
     *
     * 切替の cleanup が `load()` で要素を paused にするので、「利用者が自分で
     * 一時停止した」の抑止をそのまま効かせると、**降格もエラー表示も起きず
     * 黒いまま永久に何も出ない**（`startedOnceRef` を effect を跨いで持つように
     * したことと、`canplay` で再開するようにしたことが組み合わせて作る穴）。
     *
     * 変異: 抑止の条件から `!isResumePending()` を外すと、このテストが落ちる
     * （`stalled` が無視されて `onStalled` が 2 回目に呼ばれない）。
     */
    it('ネイティブ経路: 降格のあと再開できなくても、停滞の検出を続ける', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      const { resolve } = deferredFetch()
      const { rerender } = render(
        <LivePlayer site="default" networkId={0} serviceId={1024} onStalled={onStalled} />,
      )
      const video = document.querySelector('video')!
      vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
        type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
      )
      resolve(new Response('', { status: 200 }))
      await waitFor(() => expect(video.src).toContain('playlist.m3u8'))
      // 再生中にする（`preserved.playing` に載せる）
      await act(async () => {
        video.dispatchEvent(new Event('playing'))
        Object.defineProperty(video, 'paused', { value: false, configurable: true })
      })

      // 画質の切替（= 降格と同じ形）。cleanup が `paused` を読み、新しい effect が
      // `canplay` を待つ状態になる
      rerender(<LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" onStalled={onStalled} />)
      await waitFor(() => expect(video.src).toContain('profile=sd'))
      // load() が paused に戻した状態（`canplay` は来ない = 降格先も死んでいる）
      Object.defineProperty(video, 'paused', { value: true, configurable: true })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      // **ここが要点**: paused でも「利用者が止めた」とは見なさない
      expect(onStalled).toHaveBeenCalledTimes(1)
    })

    /**
     * **全プロファイルを束ねた master（`live.captions: true`）では降格を試さない。**
     * そのとき streamer は `?profile=` に関わらず同じ master を返すので、下げても
     * 何も変わらないのに「下げました」と表示することになる。
     *
     * 変異: `canDowngrade` の `!probe.bundlesProfiles` を外すと、このテストの
     * 「呼ばれない」が落ちる。
     */
    it('ネイティブ経路: 全プロファイルを束ねた master では降格を試さず、従来どおりエラーにする', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      const video = await renderNativePath({
        onStalled,
        body: BUNDLED_MASTER,
      })

      await act(async () => {
        video.dispatchEvent(new Event('stalled'))
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })

      expect(onStalled).not.toHaveBeenCalled()
      expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
    })

    /**
     * hls.js 経路まで進めて `<video>` を返す（フェイクの hls.js は既定で
     * `isSupported() === true`、jsdom の `canPlayType` は `''` なので、
     * 何も差し替えなければ hls.js 経路に入る）。
     */
    async function renderHlsPath(options: {
      onStalled?: () => StallHandling
      body?: string
    } = {}) {
      vi.stubGlobal(
        'fetch',
        vi.fn(() => Promise.resolve(new Response(options.body ?? '', { status: 200 }))),
      )
      render(<LivePlayer site="default" networkId={0} serviceId={1024} onStalled={options.onStalled} />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      // 「再生を押した」状態にする。`paused` は再生が始まるまで true で、
      // その間 `currentTime` が進まないのは正常である
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      Object.defineProperty(video, 'paused', { value: false, configurable: true })
      return video
    }

    /**
     * hls.js 経路: 計器の 1 秒の刻みに相乗りして「`currentTime` が進まない」を
     * 見る（`stalled` / `waiting` は MSE では正常時にも出るので聴けない）。
     *
     * 変異: `tickProgress` の `stalledForMs(...) < liveStallTimeoutMs` の比較を
     * `liveStallTimeoutMs` を無視する形（常に false）にすると落ちる。
     */
    it('hls.js 経路: 映像が猶予ぶん進まなければ降格を試す（引き取られたらエラーにしない）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      await renderHlsPath({ onStalled })

      await act(async () => {
        // 最初の刻みは観測の基準を作るだけなので、猶予ちょうどでは足りない
        // （`liveStallTimeoutMs` + 1 刻みで初めて満了する）
        vi.advanceTimersByTime(liveStallTimeoutMs + 1000)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
      // **hls.js 経路に停滞の失敗経路は作らない**（エラー文言を出さない）
      expect(screen.queryByText('ライブ再生中にエラーが発生しました。')).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
    })

    /**
     * **降格のあと `canplay` が来ないとき、hls.js 経路も検出を続ける。**
     *
     * `observeStall` に渡す `paused` は「切替の cleanup が `load()` で止めた」
     * ぶんを含む。それを「利用者が止めた」と読むと、降格先も死んでいる場合に
     * 観測の基準を捨て続けて**次の降格もエラーも起きない**。
     *
     * 変異: `paused: media.paused && !resumePending` から `!resumePending` を
     * 外すと、このテストが落ちる（`onStalled` が 1 回目から呼ばれない）。
     */
    it('hls.js 経路: 降格のあと再開できなくても、停滞の検出を続ける', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
      vi.stubGlobal('fetch', fetchMock)
      const onStalled = vi.fn(() => true)
      const probeURLs = () =>
        fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))
      const { rerender } = render(
        <LivePlayer site="default" networkId={0} serviceId={1024} profile="hd" onStalled={onStalled} />,
      )
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      Object.defineProperty(video, 'paused', { value: false, configurable: true })
      video.dispatchEvent(new Event('playing'))
      await waitFor(() => expect(probeURLs()).toHaveLength(1))

      // 画質の切替（= 降格と同じ形）。cleanup が `paused` を読み、新しい effect が
      // `canplay` を待つ状態（= `resumePending`）になる
      rerender(
        <LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" onStalled={onStalled} />,
      )
      await waitFor(() => expect(probeURLs()).toHaveLength(2))
      // load() が paused に戻した状態（`canplay` は来ない = 降格先も死んでいる）
      Object.defineProperty(video, 'paused', { value: true, configurable: true })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs + 1000)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
    })

    /** 逆向き: 再生を押していない（`paused`）間は、進まないのが正常である。 */
    it('hls.js 経路: 再生前（paused）は数えない', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      const video = await renderHlsPath({ onStalled })
      Object.defineProperty(video, 'paused', { value: true, configurable: true })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
      })

      expect(onStalled).not.toHaveBeenCalled()
    })

    /**
     * 逆向き: 非表示タブでは観測の基準を捨てる（issue #871）。
     *
     * 非表示タブではブラウザが `setInterval` を間引くので、基準を捨てないと
     * **復帰した瞬間の差分が閾値を超えて誤発火する**（進んでいないのは
     * タイマーが動いていなかったからで、映像が止まっていたからではない）。
     */
    it('hls.js 経路: 非表示タブでは数えず、基準も捨てる', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      await renderHlsPath({ onStalled })
      // **先に 1 刻み進めて観測の基準を作る。** 基準が無いまま非表示に入ると、
      // 「捨てる」実装と「捨てない」実装の差がこのテストに現れない
      // （捨てない実装でも、基準が一度も作られていなければ何も溜まらない）。
      // **`currentTime` は動かさない** --- 動かすと復帰後の最初の刻みが
      // 「進んだ」と見なして基準を作り直すので、これも差を消してしまう
      await act(async () => {
        vi.advanceTimersByTime(1000)
      })
      const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs * 3)
      })
      expect(onStalled).not.toHaveBeenCalled()

      // 見えるようになった直後は「進んでいない」を数え直す（捨てていないと
      // 3 窓ぶんの無進捗が溜まっていて、復帰の 1 秒後に発火する）
      hidden.mockReturnValue(false)
      await act(async () => {
        vi.advanceTimersByTime(1000)
      })
      expect(onStalled).not.toHaveBeenCalled()
      // ただし無進捗が実際に続けば発火する（判定そのものは生きている）
      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs)
      })
      expect(onStalled).toHaveBeenCalledTimes(1)
    })

    /** 逆向き: 進んでいる間は数えない（一時的なバッファ枯れを停滞と読まない）。 */
    it('hls.js 経路: currentTime が進んでいれば数えない', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      const video = await renderHlsPath({ onStalled })

      await act(async () => {
        for (let i = 0; i < 20; i++) {
          vi.advanceTimersByTime(1000)
          video.currentTime += 0.5
        }
      })

      expect(onStalled).not.toHaveBeenCalled()
    })

    /**
     * **プロファイルごとの master（captions 無効時の実際の応答）では降格を試す。**
     * 音声レンディションを載せるため、captions に関わらず master が返る。
     * 「master なら止める」と判定すると、全デプロイで自動降格が一度も動かない。
     *
     * 変異: `bundlesProfiles` を `#EXT-X-STREAM-INF` の有無で判定すると落ちる。
     */
    it('hls.js 経路: プロファイルごとの master（video variant 1 本）では降格を試す', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      await renderHlsPath({ onStalled, body: PROFILE_MASTER })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs + 1000)
      })

      expect(onStalled).toHaveBeenCalledTimes(1)
    })

    /** 全プロファイルを束ねた master では、hls.js 経路でも降格を試さない。 */
    it('hls.js 経路: 全プロファイルを束ねた master では降格を試さない', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => true)
      await renderHlsPath({
        onStalled,
        body: BUNDLED_MASTER,
      })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
      })

      expect(onStalled).not.toHaveBeenCalled()
    })

    /**
     * `'wait'`（一覧がまだ届いていない）は「下げられない」と違う。
     *
     * **観測を初期化して次の刻みで再判定する。** `false` と同じ扱いにすると、
     * 一覧が遅れて届いた場合にその再生では二度と試さない（下のテストが
     * その 1 回目→2 回目を見る）。
     */
    it('hls.js 経路: 一覧が未着（wait）なら次の刻みで再判定する', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => 'wait' as const)
      await renderHlsPath({ onStalled })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs + 1000)
      })
      expect(onStalled).toHaveBeenCalledTimes(1)

      // 観測を初期化しているので、次の窓で再び判定される（初期化の直後の
      // 1 刻みは基準を作るだけなので、猶予 + 1 刻みが要る）
      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs + 1000)
      })
      expect(onStalled).toHaveBeenCalledTimes(2)
      // **エラーにはしない**（判断できないだけで、失敗ではない）
      expect(screen.queryByText('ライブ再生中にエラーが発生しました。')).not.toBeInTheDocument()
    })

    /** 下げられないときも、hls.js 経路はエラーにしない（現行どおり黙って待つ）。 */
    it('hls.js 経路: 引き取られなくてもエラーにしない（現行どおり）', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const onStalled = vi.fn(() => false)
      await renderHlsPath({ onStalled })

      await act(async () => {
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
      })

      // 1 度判定したら、下げられないと分かっているので繰り返さない
      expect(onStalled).toHaveBeenCalledTimes(1)
      expect(screen.queryByText('ライブ再生中にエラーが発生しました。')).not.toBeInTheDocument()
    })
  })

  it('ネイティブHLSでも明示した追っかけ開始位置から再生する', async () => {
    const { resolve } = deferredFetch()
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={10}
        offsetSessionStart={{ type: 'offset', offsetSeconds: 30 }}
      />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))

    await waitFor(() => expect(video.src).toContain('/chase/offset/30/playlist.m3u8'))
    Object.defineProperty(video, 'currentTime', { value: 7, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    expect(video.currentTime).toBe(0)

    Object.defineProperty(video, 'currentTime', { value: 7, writable: true, configurable: true })
    fireEvent.canPlay(video)
    expect(video.currentTime).toBe(0)
  })

  it('ネイティブHLSでも画質の切替で位置を持ち越し、canplay で最新端への飛びを戻す', async () => {
    // WebKit は EVENT playlist を付けるとき、loadedmetadata で受け付けた位置を
    // 捨てて最新端を選ぶことがある。canplay での再表明が無いとここで落ちる。
    const { resolve } = deferredFetch()
    const { rerender } = render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={83}
        profile="live-720p"
      />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(video.src).toContain('profile=live-720p'))
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    fireEvent.canPlay(video)
    video.currentTime = 12

    rerender(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={83}
        profile="live-480p"
      />,
    )
    await waitFor(() => expect(video.src).toContain('profile=live-480p'))
    video.currentTime = 0
    fireEvent.loadedMetadata(video)
    expect(video.currentTime).toBe(12)
    video.currentTime = 95
    fireEvent.canPlay(video)
    expect(video.currentTime).toBe(12)
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

    it('原本 VOD は保存位置を秒に切り下げた offset のセッションで開き、端数だけを startPosition に渡す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      render(
        <LivePlayer
          mode="original-vod"
          site="default"
          recordingId={42}
          offsetSessionStart={{ type: 'saved-position', positionMs: 23_500 }}
          profile="hd"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      // offset 0 のセッションに 23.5 を渡すと、変換が 23.5 秒へ追いつくまで始まらない。
      expect(hlsMockState.instances[0]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/42/original-vod/offset/23/playlist.m3u8?profile=hd',
      )
      // offset 23 の起点は 689 フレーム目（22.98963 秒）。
      expect((hlsMockState.constructorArgs[0]![0] as { startPosition: number }).startPosition).toBeCloseTo(0.51037, 4)

      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBeCloseTo(0.51037, 4)
    })

    it('原本 VOD の開始 offset は最初の保存位置で固定し、保存位置が更新されても張り直さない', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      const props = { mode: 'original-vod', site: 'default', profile: 'hd' } as const
      const { rerender } = render(<LivePlayer {...props} recordingId={42} offsetSessionStart={{ type: 'saved-position', positionMs: 23_000 }} />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      rerender(<LivePlayer {...props} recordingId={42} offsetSessionStart={{ type: 'saved-position', positionMs: 31_000 }} />)
      await new Promise((resolve) => setTimeout(resolve, 20))
      expect(hlsMockState.instances).toHaveLength(1)
      // 次のエピソードへ移った後の保存（同じ部品に別の録画が来る）。
      rerender(<LivePlayer {...props} recordingId={43} offsetSessionStart={{ type: 'saved-position', positionMs: 50_000 }} />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      rerender(<LivePlayer {...props} recordingId={43} offsetSessionStart={{ type: 'saved-position', positionMs: 57_000 }} />)
      await new Promise((resolve) => setTimeout(resolve, 20))
      expect(hlsMockState.instances.map((instance) => instance.loadSource.mock.calls[0]![0])).toEqual([
        '/api/sites/default/recordings/42/original-vod/offset/23/playlist.m3u8?profile=hd',
        '/api/sites/default/recordings/43/original-vod/offset/50/playlist.m3u8?profile=hd',
      ])
    })

    it('サーバーで共有する原本時間軸の位置を原本 VOD の hls.js startPosition に渡す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      const chase = render(
        <LivePlayer mode="chase" site="default" recordingId={90} profile="hd" />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const chaseVideo = document.querySelector('video')!
      Object.defineProperty(chaseVideo, 'currentTime', { value: 42, writable: true, configurable: true })
      fireEvent.timeUpdate(chaseVideo)
      expect(localStorage.getItem('rokuban:playback:90:original')).toBeNull()
      chase.unmount()

      render(<LivePlayer mode="original-vod" site="default" recordingId={90} offsetSessionStart={{ type: 'saved-position', positionMs: 42_000 }} profile="hd" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/90/original-vod/offset/42/playlist.m3u8?profile=hd',
      )
      // offset 42 の起点は 1258 フレーム目（41.97527 秒）。
      expect((hlsMockState.constructorArgs[1]![0] as { startPosition: number }).startPosition).toBeCloseTo(0.02473, 4)
    })

    it('原本 VOD は ENDLIST を見るまで duration を完了判定に使わない', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      render(<LivePlayer mode="original-vod" site="default" recordingId={44} profile="hd" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      // 変換中の EVENT playlist: duration は変換の先端（120 秒）でしかない
      Object.defineProperty(video, 'duration', { value: 300, configurable: true })
      fireEvent.loadedMetadata(video)

      video.currentTime = 118
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:44:vod-h264')).toBeNull()

      // ENDLIST を読んだ（live === false）後は、終端 5 秒以内の位置を残さない
      const levelLoaded = hlsMockState.instances[0]!.on.mock.calls.find((c) => c[0] === 'hlsLevelLoaded')![1]
      levelLoaded('hlsLevelLoaded', { fatal: false, details: { live: true } })
      video.currentTime = 119
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:44:vod-h264')).toBeNull()
      levelLoaded('hlsLevelLoaded', { fatal: false, details: { live: false } })
      video.currentTime = 120
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:44:vod-h264')).toBeNull()
    })

    it('原本 VOD の ended で保存位置を消す（ネイティブ経路も同じイベント）', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      render(<LivePlayer mode="original-vod" site="default" recordingId={45} profile="hd" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'duration', { value: 120, configurable: true })
      fireEvent.ended(video)
      expect(localStorage.getItem('rokuban:playback:45:vod-h264')).toBeNull()
    })

    it('原本 VOD の profile 切替で再生位置を持ち越す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      const props = {
        mode: 'original-vod',
        site: 'default',
        recordingId: 43,
      } as const
      const { rerender } = render(<LivePlayer {...props} profile="hd" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      // 開始位置（0）を canplay で明示し終えるまでは、要素の位置を持ち越さない。
      fireEvent.canPlay(video)
      video.currentTime = 37

      rerender(<LivePlayer {...props} profile="sd" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      video.currentTime = 0
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(37)
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/43/original-vod/playlist.m3u8?profile=sd',
      )
    })

    it('追っかけは VOD と端末共通の速度を読み書きする', async () => {
      savePlaybackRate(1.5)
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(<LivePlayer mode="chase" site="default" recordingId={7} />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      expect(video.defaultPlaybackRate).toBe(1.5)
      expect(video.playbackRate).toBe(1.5)

      video.playbackRate = 1.25
      fireEvent.rateChange(video)

      expect(localStorage.getItem('rokuban:playback-rate')).toBe('1.25')
      expect(video.defaultPlaybackRate).toBe(1.25)
    })

    it('追っかけのチャプター編集中は別速度を使い、終了すると通常速度へ戻す', async () => {
      savePlaybackRate(1.5)
      saveChapterEditPlaybackRate(1.75)
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const props = { mode: 'chase', site: 'default', recordingId: 7 } as const
      const { rerender } = render(<LivePlayer {...props} chapterEditing />)

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      expect(video.defaultPlaybackRate).toBe(1.75)
      expect(video.playbackRate).toBe(1.75)

      fireEvent.click(screen.getByTestId('chapter-edit-playback-rate'))
      expect(video.playbackRate).toBe(2)
      fireEvent.rateChange(video)
      expect(localStorage.getItem('rokuban:chapter-edit-playback-rate')).toBe('2')
      expect(localStorage.getItem('rokuban:playback-rate')).toBe('1.5')

      rerender(<LivePlayer {...props} chapterEditing={false} />)
      expect(video.defaultPlaybackRate).toBe(1.5)
      expect(video.playbackRate).toBe(1.5)
      rerender(<LivePlayer {...props} chapterEditing />)
      expect(video.defaultPlaybackRate).toBe(2)
      expect(video.playbackRate).toBe(2)
    })

    it('追っかけは配信プロファイルと別のVODプロファイルで位置を復元する', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={7}
          offsetSessionStart={{ type: 'saved-position', positionMs: 12_000 }}
          profile="live-720p"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      expect(hlsMockState.constructorArgs[0]).toEqual([{ startPosition: 0 }])
      expect(hlsMockState.instances[0]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/7/chase/offset/12/playlist.m3u8?profile=live-720p',
      )
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })

      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(0)
    })

    it('画質を切り替えても追っかけの再生位置を持ち越す（offset 付き）', async () => {
      // offset 付きで見るのは、**復元 effect が profile で立ち直る変異が
      // 決定的に落ちる**ようにするためである。offset 付きの追っかけは先頭が
      // 「録画の 30 秒」なので、復元がやり直されると `onCanPlay` の 0 秒への
      // 再表明（`explicitStartSeekPending`）が走って位置が巻き戻る。
      //
      // 録画 id は他のテストと共有しない（id を借りると保存位置が残っていて
      // 偽陽性で通る）。
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const { rerender } = render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={81}
          offsetSessionStart={{ type: 'offset', offsetSeconds: 30 }}
          profile="live-720p"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      // 初回の読み込みで 1 度は canplay が来る（その 1 回で 0 秒への再表明は
      // 使い切られる）。以降は画質の切替で復元が立ち直ったときだけ再び走る。
      // 実ブラウザと同じく loadedmetadata が先に来る（来ていない読み込みの位置は
      // 持ち越さない）。
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      video.currentTime = 12

      rerender(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={81}
          offsetSessionStart={{ type: 'offset', offsetSeconds: 30 }}
          profile="live-480p"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(hlsMockState.constructorArgs[1]).toEqual([{ startPosition: 12 }])
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/81/chase/offset/30/playlist.m3u8?profile=live-480p',
      )
      // 切替の前後で再生位置が連続する（先頭へ巻き戻らない）
      fireEvent.canPlay(video)
      expect(video.currentTime).toBe(12)
      // 位置のキーは VOD 側のプロファイルのまま（画質ごとに分かれない）。
      // offset 付きは録画全体の秒数（12 + 30）で保存する
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:81:vod-h264')).toBeNull()
      expect(localStorage.getItem('rokuban:playback:81:live-480p')).toBeNull()
    })

    it('画質の切替で位置を戻し終えるまで「続きから」を上書きしない', async () => {
      // cleanup の load() で位置が 0 に戻り、その timeupdate が新しい
      // プレイリストの loadedmetadata より先に届く。ここで保存すると、切替の
      // 直後に離れたとき「続きから」が先頭になる。
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const { rerender } = render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={82}
          profile="live-720p"
        />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      video.currentTime = 12
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:82:vod-h264')).toBeNull()

      rerender(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={82}
          profile="live-480p"
        />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      video.currentTime = 0
      fireEvent.timeUpdate(video)
      fireEvent.pause(video)
      expect(localStorage.getItem('rokuban:playback:82:vod-h264')).toBeNull()

      // 戻し終える前にもう一度切り替えても、要素の位置（0）ではなく持ち越し中の
      // 位置を引き継ぐ
      rerender(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={82}
          profile="live-360p"
        />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(3))
      expect(hlsMockState.constructorArgs[2]).toEqual([{ startPosition: 12 }])

      // 戻し終えたら保存を再開する
      fireEvent.canPlay(video)
      expect(video.currentTime).toBe(12)
      video.currentTime = 14
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:82:vod-h264')).toBeNull()
    })

    it('サーバー再開位置が更新された後の画質切替も、切替直前の位置を持ち越す', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const props = { mode: 'chase', site: 'default', recordingId: 84 } as const
      const { rerender } = render(
        <LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 40_000 }} profile="live-720p" />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      expect(video.currentTime).toBe(0)
      video.currentTime = 12

      rerender(<LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 30_000 }} profile="live-720p" />)
      expect(hlsMockState.instances).toHaveLength(1)
      rerender(<LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 30_000 }} profile="live-480p" />)
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(hlsMockState.constructorArgs[1]).toEqual([{ startPosition: 12 }])
      // 最終値だけでなく、途中で更新後のサーバー位置（30）へ seek しないことも見る
      // （最後に 12 へ戻るのはリスナの登録順に依存しているだけ）。
      let position = 0
      const assigned: number[] = []
      Object.defineProperty(video, 'currentTime', {
        configurable: true,
        get: () => position,
        set: (value: number) => {
          position = value
          assigned.push(value)
        },
      })
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      expect(video.currentTime).toBe(12)
      expect(assigned).not.toContain(30)
    })

    it('最初の読み込みが終わる前に画質を切り替えても、保存位置から再生する', async () => {
      // probe 中（ffmpeg の起動待ち）の切替。読み込まれていない要素の位置 0 を
      // 持ち越すと、復元が潰れて保存位置が消える。
      const { resolve } = deferredFetch()
      const props = { mode: 'chase', site: 'default', recordingId: 85 } as const
      const { rerender } = render(
        <LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 40_000 }} profile="live-720p" />,
      )
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      rerender(<LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 40_000 }} profile="live-480p" />)
      resolve(new Response('', { status: 200 }))

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      expect(hlsMockState.constructorArgs[0]).toEqual([{ startPosition: 0 }])
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      expect(video.currentTime).toBe(0)
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:85:vod-h264')).toBeNull()
    })

    it('指定した開始オフセットから読み、保存位置を上書きせず録画全体の秒数で扱う', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={8}
          offsetSessionStart={{ type: 'offset', offsetSeconds: 30 }}
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      expect(hlsMockState.instances[0]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/8/chase/offset/30/playlist.m3u8',
      )
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(0)

      video.currentTime = 13
      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:8:vod-h264')).toBeNull()
    })

    it('録画先頭を明示したときも保存位置を復元しない', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={9}
          offsetSessionStart={{ type: 'offset', offsetSeconds: 0 }}
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      expect(hlsMockState.instances[0]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/9/chase/playlist.m3u8',
      )
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(0)
    })

    it('成長中の追っかけプレイリストでは最新付近の再生位置を完了扱いで消さない', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={70}
          profile="live-720p"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'duration', { value: 120, configurable: true })
      Object.defineProperty(video, 'currentTime', { value: 119.9, writable: true, configurable: true })

      fireEvent.timeUpdate(video)
      expect(localStorage.getItem('rokuban:playback:70:vod-h264')).toBeNull()

      localStorage.clear()
      fireEvent.pause(video)
      expect(localStorage.getItem('rokuban:playback:70:vod-h264')).toBeNull()
    })

    it('fatal エラー後の再読み込みでも追っかけの再生位置を復元する', async () => {
      const user = userEvent.setup()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      render(
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={71}
          offsetSessionStart={{ type: 'saved-position', positionMs: 12_000 }}
          profile="live-720p"
        />,
      )

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
      const video = document.querySelector('video')!
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(0)

      const firstHls = hlsMockState.instances[0]!
      const errorCall = firstHls.on.mock.calls.find(([event]) => event === 'hlsError')
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void
      await act(async () => {
        errorHandler('hlsError', { fatal: true })
      })

      await user.click(await screen.findByRole('button', { name: '再読み込み' }))
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      video.currentTime = 0
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBe(0)
    })

    it('fatal エラー後の原本 VOD retry は最新の保存位置とフレーム端数を保つ', async () => {
      const user = userEvent.setup()
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
      const props = { mode: 'original-vod' as const, site: 'default', recordingId: 72, profile: 'hd' }
      const { rerender } = render(
        <LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 23_500 }} />,
      )
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

      const video = document.querySelector('video')!
      const origin = originalVODSessionOriginSeconds(23)
      const firstStartPosition = 23.5 - origin
      expect((hlsMockState.constructorArgs[0]![0] as { startPosition: number }).startPosition)
        .toBeCloseTo(firstStartPosition, 5)
      Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
      fireEvent.loadedMetadata(video)
      fireEvent.canPlay(video)
      expect(video.currentTime).toBeCloseTo(firstStartPosition, 5)

      // 同じ録画の保存位置更新は既存セッションを動かさないが、明示 retry には使う。
      rerender(
        <LivePlayer {...props} offsetSessionStart={{ type: 'saved-position', positionMs: 31_500 }} />,
      )
      expect(hlsMockState.instances).toHaveLength(1)
      const errorCall = hlsMockState.instances[0]!.on.mock.calls.find(([event]) => event === 'hlsError')
      const errorHandler = errorCall![1] as (event: string, data: { fatal: boolean }) => void
      await act(async () => errorHandler('hlsError', { fatal: true }))

      await user.click(await screen.findByRole('button', { name: '再読み込み' }))
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      const retryStartPosition = 31.5 - origin
      expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
        '/api/sites/default/recordings/72/original-vod/offset/23/playlist.m3u8?profile=hd',
      )
      expect((hlsMockState.constructorArgs[1]![0] as { startPosition: number }).startPosition)
        .toBeCloseTo(retryStartPosition, 5)

      video.currentTime = 0
      fireEvent.loadedMetadata(video)
      expect(video.currentTime).toBeCloseTo(retryStartPosition, 5)
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
        vi.advanceTimersByTime(liveStallTimeoutMs * 2)
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
      const video = await renderNativePath({ onDiagnostics })
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
      const video = await renderNativePath({ onDiagnostics })
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
      const video = await renderNativePath({ onDiagnostics })

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

    it('追っかけのアンマウントでは recording id の leave ヒントを送る', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      const { unmount } = render(<LivePlayer mode="chase" site="default" recordingId={42} />)
      await waitForPlaying()

      unmount()

      expect(sent).toEqual(['/api/sites/default/recordings/42/chase/leave'])
    })

    it('オフセット付き追っかけのアンマウントでは同じセッションへ leave する', async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
      const sent = stubBeacon()
      const { unmount } = render(
        <LivePlayer mode="chase" site="default" recordingId={42} offsetSessionStart={{ type: 'offset', offsetSeconds: 90 }} />,
      )
      await waitForPlaying()

      unmount()

      expect(sent).toEqual(['/api/sites/default/recordings/42/chase/offset/90/leave'])
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

/**
 * 画質（プロファイル）切替（issue #869 M4-21）。
 *
 * **切替はセッションを作り直さない。** 1 サービスの ffmpeg 1 本が全プロファイルを
 * 同時に出力しており、替わるのはプレイリストの URL だけである（docs/api/media.md
 * §資源同定）。したがって `profile` が変わっても離脱ヒントを送らない ---
 * ヒントは「このチャンネルを見るのをやめた」の合図で、画質の切替ではない。
 */
describe('LivePlayer / 画質（プロファイル）切替（issue #869）', () => {
  it('profile が変わると新しい URL で probe をやり直し、離脱ヒントは送らない', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const probeURLs = () =>
      fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))
    const leavePosts = () =>
      fetchMock.mock.calls
        .map(([url]) => String(url))
        .filter((u) => u.includes('/live/leave'))

    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} profile="hd" />,
    )
    await waitFor(() => expect(probeURLs()).toHaveLength(1))
    expect(probeURLs()[0]).toContain('profile=hd')

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" />)
    await waitFor(() => expect(probeURLs()).toHaveLength(2))
    expect(probeURLs()[1]).toContain('profile=sd')
    // 同じセッションの別プレイリストを取るだけ --- 手放す合図は送らない
    expect(leavePosts()).toEqual([])
  })

  /**
   * 切替の cleanup は `video.load()` を呼ぶので、再生中だった要素は paused に戻る。
   * **`canplay` で再開する**（`src` の代入や `attachMedia` の直後に `play()` を
   * 呼んでも、その後の load algorithm が `paused` を true に戻すので競争に負ける
   * --- 実ブラウザで実測: 呼んでも `paused=true` のままで `currentTime` が 0 だった。
   * `web/e2e/live.mjs` の ⑪ が同じことを実ブラウザで見る）。
   */
  it('再生中の profile 切替では、canplay で再生を再開する', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const probeURLs = () =>
      fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))
    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} profile="hd" />,
    )
    const video = document.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue(undefined)
    await waitFor(() => expect(probeURLs()).toHaveLength(1))

    // 再生中の状態を作る（cleanup が読む `paused` を false にしておく）
    Object.defineProperty(video, 'paused', { value: false, configurable: true })
    video.dispatchEvent(new Event('playing'))

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" />)
    await waitFor(() => expect(probeURLs()).toHaveLength(2))
    expect(probeURLs()[1]).toContain('profile=sd')
    // **切替の直後には呼ばない**（load algorithm に上書きされるので意味が無い）
    expect(play).not.toHaveBeenCalled()

    await act(async () => {
      video.dispatchEvent(new Event('canplay'))
    })
    expect(play).toHaveBeenCalledTimes(1)
  })

  /**
   * ライブページの「最初から」・軸のシークは同じ要素のまま再生元を追っかけへ替える。
   * 再生中のライブから替えたなら canplay で再開し、止めたライブから替えたなら再開しない
   * （実ブラウザは `web/e2e/live.mjs` の「frame c)」）。
   */
  it.each([
    [true, 1],
    [false, 0],
  ])('ライブ（再生中=%s）から追っかけへ替えると、canplay で play を %i 回呼ぶ', async (playing, calls) => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const probeURLs = () =>
      fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))
    const { rerender } = render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    const video = document.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue(undefined)
    await waitFor(() => expect(probeURLs()).toHaveLength(1))
    Object.defineProperty(video, 'paused', { value: !playing, configurable: true })
    if (playing) video.dispatchEvent(new Event('playing'))

    rerender(
      <LivePlayer mode="chase" site="default" networkId={0} serviceId={1024} recordingId={501} offsetSessionStart={{ type: 'offset', offsetSeconds: 180 }} />,
    )
    await waitFor(() => expect(probeURLs()).toHaveLength(2))
    expect(probeURLs()[1]).toContain('/recordings/501/chase/offset/180/')
    await act(async () => {
      video.dispatchEvent(new Event('canplay'))
    })
    expect(play).toHaveBeenCalledTimes(calls)
  })

  /** ネイティブ経路（Safari 相当）でも同じく `canplay` で再開する。 */
  it('ネイティブ経路でも canplay で再生を再開する', async () => {
    const { resolve } = deferredFetch()
    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} profile="hd" />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(video.src).toContain('profile=hd'))

    Object.defineProperty(video, 'paused', { value: false, configurable: true })
    video.dispatchEvent(new Event('playing'))
    const play = vi.spyOn(video, 'play').mockResolvedValue(undefined)

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" />)
    await waitFor(() => expect(video.src).toContain('profile=sd'))

    fireEvent(video, new Event('canplay'))

    expect(play).toHaveBeenCalledTimes(1)
  })

  /**
   * **再読み込み（エラー表示のボタン）でも同じく再開する。** 持ち越しは cleanup の
   * 時点の `paused` を読むだけで、切替の理由（自動降格 / 手動の画質選択 / 再読み込み）を
   * 区別しない。ネイティブ経路の停滞は `waiting` のまま `paused=false` なので、
   * 利用者がボタンを押したら再生に戻る --- 押し直しを 2 回求めない。
   * **hls.js 経路では起きない**（fatal エラーで `hls.destroy()` が `load()` を呼び
   * `paused=true` にするので、cleanup が読む時点で再生中ではない）。
   */
  it('ネイティブ経路: エラー後の再読み込みでも canplay で再生を再開する', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { resolve } = deferredFetch()
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(video.src).toContain('playlist.m3u8'))
    Object.defineProperty(video, 'paused', { value: false, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue(undefined)

    await act(async () => {
      video.dispatchEvent(new Event('playing'))
      video.dispatchEvent(new Event('stalled'))
      vi.advanceTimersByTime(liveStallTimeoutMs)
    })
    fireEvent.click(await screen.findByRole('button', { name: '再読み込み' }))
    await waitFor(() => expect(video.src).toContain('playlist.m3u8'))
    expect(play).not.toHaveBeenCalled()

    fireEvent(video, new Event('canplay'))

    expect(play).toHaveBeenCalledTimes(1)
  })

  /**
   * **「一度でも再生が始まったか」は切替を跨いで持つ**（`startedOnceRef`）ので、
   * 利用者が一時停止したまま画質を切り替えた先の `stalled` はエラーにしない
   * （一時停止中の抑止と同じ扱い）。**再生を押した後の `waiting` では出す** ---
   * 抑止が「以後ずっと検出しない」に化けていないことを同じテストで見る。
   *
   * 変異: effect の先頭で `startedOnceRef.current = false` に戻すと、前半の
   * 「エラーにしない」が落ちる。
   */
  it('ネイティブ経路: 一時停止中に切り替えた先の stalled ではエラーにせず、再生後の waiting で出す', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { resolve } = deferredFetch()
    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} profile="hd" />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(video.src).toContain('profile=hd'))
    video.dispatchEvent(new Event('playing'))
    // 利用者が一時停止してから画質を切り替える
    Object.defineProperty(video, 'paused', { value: true, configurable: true })
    video.dispatchEvent(new Event('pause'))

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} profile="sd" />)
    await waitFor(() => expect(video.src).toContain('profile=sd'))
    await act(async () => {
      video.dispatchEvent(new Event('stalled'))
      vi.advanceTimersByTime(liveStallTimeoutMs * 2)
    })
    expect(screen.queryByText(/映像データが途絶えました/)).not.toBeInTheDocument()

    await act(async () => {
      Object.defineProperty(video, 'paused', { value: false, configurable: true })
      video.dispatchEvent(new Event('play'))
      video.dispatchEvent(new Event('waiting'))
      vi.advanceTimersByTime(liveStallTimeoutMs)
    })
    expect(screen.getByText(/映像データが途絶えました/)).toBeInTheDocument()
  })

  /**
   * 逆向き: **初回のマウントでは `canplay` でも再生しない。** 「再生」ボタンで
   * マウントしただけで再生を始めると、同意の分離（issue #234）が壊れる。
   */
  it('初回のマウントでは canplay でも再生しない', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    const video = document.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue(undefined)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    await act(async () => {
      video.dispatchEvent(new Event('canplay'))
    })

    expect(play).not.toHaveBeenCalled()
  })

  it('profile を省略すると ?profile を付けない（サーバー側の既定に任せる）', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)

    render(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8')),
      ).toHaveLength(1),
    )
    const url = fetchMock.mock.calls
      .map(([u]) => String(u))
      .find((u) => u.includes('playlist.m3u8'))!
    expect(url).not.toContain('profile=')
  })
})

/**
 * 音声（二重音声の主 / 副。issue #870）。
 *
 * **切替はプレイヤーが取るトラックを替えるだけ**で、プレイリストの取り直しも
 * hls.js の作り直しも起こさない（streamer が標準 / 主 / 副の 3 本を常に出している）。
 * トラックの位置（0 = 標準 / 1 = 主 / 2 = 副）が streamer との契約で、期待値は
 * リテラルで書く。実ブラウザで音が替わることは jsdom では測れない
 * （`internal/streamer/live.go` の hlsFlags に書いた Playwright の実測が担う）。
 */
describe('LivePlayer / 音声（issue #870）', () => {
  const playlistFetches = (fetchMock: ReturnType<typeof vi.fn>) =>
    fetchMock.mock.calls.map(([url]) => String(url)).filter((u) => u.includes('playlist.m3u8'))

  it('hls.js: トラック一覧が届くと選択を適用し、切替はプレイリストを取り直さない', async () => {
    const fetchMock = vi.fn((_url: string) => Promise.resolve(new Response('', { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} audio="sub" />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const hls = hlsMockState.instances[0]!
    const onTracks = hls.on.mock.calls.find(([event]) => event === 'hlsAudioTracksUpdated')
    expect(onTracks).toBeDefined()

    hls.audioTracks = [{}, {}, {}]
    act(() => onTracks![1]('hlsAudioTracksUpdated', { fatal: false }))
    expect(hls.audioTrack).toBe(2)

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} audio="main" />)
    expect(hls.audioTrack).toBe(1)
    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    expect(hls.audioTrack).toBe(0)

    // 作り直さない・取り直さない・URL に音声を載せない（サーバーは選択を知らない）
    expect(hlsMockState.instances).toHaveLength(1)
    expect(playlistFetches(fetchMock)).toHaveLength(1)
    expect(playlistFetches(fetchMock)[0]).not.toContain('audio')
    expect(hls.loadSource).toHaveBeenCalledTimes(1)
  })

  it('hls.js: 音声レンディションを持たない master（トラック 0 本）では触らない', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('', { status: 200 }))),
    )
    render(<LivePlayer site="default" networkId={0} serviceId={1024} audio="sub" />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const hls = hlsMockState.instances[0]!
    const onTracks = hls.on.mock.calls.find(([event]) => event === 'hlsAudioTracksUpdated')!
    act(() => onTracks[1]('hlsAudioTracksUpdated', { fatal: false }))
    expect(hls.audioTrack).toBe(0)
  })

  it('ネイティブ（WebKit）: トラックが出来たら選択を適用し、切替で src を差し替えない', async () => {
    const { resolve } = deferredFetch()
    const { rerender } = render(
      <LivePlayer site="default" networkId={0} serviceId={1024} audio="sub" />,
    )
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    // jsdom の <video> は audioTracks を持たない。WebKit と同じく後から作られる形にする
    const list: Array<{ enabled: boolean }> = []
    const tracks = Object.assign(list, {
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })
    Object.defineProperty(video, 'audioTracks', { value: tracks, configurable: true })

    resolve(new Response('', { status: 200 }))
    await waitFor(() => expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument())
    const src = video.src

    // トラックが 1 本ずつ届く（addtrack）。揃った時点で副だけが有効になる
    const onAddTrack = tracks.addEventListener.mock.calls.find(([type]) => type === 'addtrack')![1]
    list.push({ enabled: true }, { enabled: false }, { enabled: false })
    act(() => onAddTrack())
    expect(list.map((t) => t.enabled)).toEqual([false, false, true])

    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} audio="main" />)
    expect(list.map((t) => t.enabled)).toEqual([false, true, false])
    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    expect(list.map((t) => t.enabled)).toEqual([true, false, false])
    expect(video.src).toBe(src)
  })
})

describe('LivePlayer / 追っかけ共通シークバー（issue #1015）', () => {
  const chaseTimeline = {
    chaseHeadOffsetSeconds: 0,
    plannedSeconds: 60,
    recordedSeconds: 20,
  }

  const playlistRequests = (fetchMock: ReturnType<typeof vi.fn>) =>
    fetchMock.mock.calls.map(([url]) => String(url)).filter((url) => url.includes('playlist.m3u8'))

  // ライブページの追っかけ（onReturnLive あり）だけ「録画から再生中」を出す。録画詳細の追っかけには出さない。
  it.each([
    ['ライブページ', true],
    ['録画詳細', false],
  ])('%s の追っかけで「録画から再生中」を出すか: %s', async (_page, fromLive) => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={510}
        chaseTimeline={chaseTimeline}
        onReturnLive={fromLive ? () => {} : undefined}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    expect(screen.queryByText('● 録画から再生中') !== null).toBe(fromLive)
    expect(screen.getByRole('button', { name: fromLive ? 'ライブへ戻る' : '録画の先端へ' })).toBeInTheDocument()
  })

  it('native controlsを外し、画質を設定メニューに置き、範囲内シークは再要求しない', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={510}
        chaseTimeline={chaseTimeline}
        profile="hd"
        availableProfiles={[{ name: 'hd', height: 720 }, { name: 'sd', height: 480 }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const video = document.querySelector('video')!
    expect(video.controls).toBe(false)
    const sliders = screen.getAllByRole('slider', { name: 'シークバー' })
    expect(sliders).toHaveLength(1)
    const slider = sliders[0]!
    expect(slider).toHaveAttribute('aria-valuemin', '0')
    expect(slider).toHaveAttribute('aria-valuemax', '60')
    expect(screen.queryByRole('slider', { name: '追っかけ再生の位置' })).not.toBeInTheDocument()
    expect(document.querySelector('[data-testid="chase-timeline-track"]')?.closest('[data-testid="player-controls"]'))
      .not.toBeNull()

    const seekable = { length: 1, start: () => 0, end: () => 10 }
    Object.defineProperty(video, 'seekable', { value: seekable, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    Object.defineProperty(slider, 'getBoundingClientRect', {
      value: () => ({ x: 0, y: 0, left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24, toJSON: () => ({}) }),
    })

    const beforeSeek = playlistRequests(fetchMock).length
    fireEvent.pointerDown(slider, { pointerId: 1, pointerType: 'mouse', clientX: 50, clientY: 12 })
    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'mouse', clientX: 50, clientY: 12 })
    expect(video.currentTime).toBe(0)
    expect(playlistRequests(fetchMock)).toHaveLength(beforeSeek)
    expect(slider).toHaveAttribute('aria-valuenow', '5')
    fireEvent.pointerUp(slider, { pointerId: 1, pointerType: 'mouse', clientX: 50, clientY: 12 })
    expect(video.currentTime).toBe(5)
    expect(playlistRequests(fetchMock)).toHaveLength(beforeSeek)

    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const settings = screen.getByRole('menu', { name: '再生設定' })
    expect(within(settings).getByRole('menuitem', { name: '画質' })).toHaveAccessibleDescription('hd（720p）')
    const quality = openPlaybackSettingsSubmenu('画質')
    expect(within(quality).getByRole('menuitemradio', { name: 'hd（720p）' })).toHaveAttribute('aria-checked', 'true')
    expect(within(quality).getByRole('menuitemradio', { name: 'sd（480p）' })).toHaveAttribute('aria-checked', 'false')
    expect(settings).toHaveAttribute('aria-label', '画質')
  })

    it('現在のoffsetより前への確定は内部でoffset付きplaylistに張り直す', async () => {
      const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
      vi.stubGlobal('fetch', fetchMock)
      const props = {
        mode: 'chase' as const,
        site: 'default',
        recordingId: 511,
        chaseTimeline,
      }
      render(<LivePlayer {...props} offsetSessionStart={{ type: 'offset', offsetSeconds: 5 }} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    expect(playlistRequests(fetchMock)[0]).toContain('/chase/offset/5/playlist.m3u8')

    const slider = screen.getByRole('slider', { name: 'シークバー' })
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 10 },
      configurable: true,
    })
    Object.defineProperty(slider, 'getBoundingClientRect', {
      value: () => ({ x: 0, y: 0, left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24, toJSON: () => ({}) }),
    })
      // Axis second 3 is earlier than session offset 5. It is committed only on pointerup.
      fireEvent.pointerDown(slider, { pointerId: 2, pointerType: 'mouse', clientX: 30, clientY: 12 })
      fireEvent.pointerMove(slider, { pointerId: 2, pointerType: 'mouse', clientX: 30, clientY: 12 })
      expect(playlistRequests(fetchMock)).toHaveLength(1)
      fireEvent.pointerUp(slider, { pointerId: 2, pointerType: 'mouse', clientX: 30, clientY: 12 })
      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(playlistRequests(fetchMock)[1]).toContain('/chase/offset/3/playlist.m3u8')
    })

    it('変換済みの端より先で録画済み範囲内なら、内部で選んだ秒へ張り直す', async () => {
      const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
      vi.stubGlobal('fetch', fetchMock)
      const props = {
        mode: 'chase' as const,
        site: 'default',
        recordingId: 512,
        chaseTimeline,
      }
      render(<LivePlayer {...props} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const slider = screen.getByRole('slider', { name: 'シークバー' })
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 10 },
      configurable: true,
    })
    Object.defineProperty(slider, 'getBoundingClientRect', {
      value: () => ({ x: 0, y: 0, left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24, toJSON: () => ({}) }),
    })

      fireEvent.pointerDown(slider, { pointerId: 3, pointerType: 'mouse', clientX: 150, clientY: 12 })
      fireEvent.pointerMove(slider, { pointerId: 3, pointerType: 'mouse', clientX: 150, clientY: 12 })
      expect(slider).toHaveAttribute('aria-valuenow', '15')
      expect(playlistRequests(fetchMock)).toHaveLength(1)
      fireEvent.pointerUp(slider, { pointerId: 3, pointerType: 'mouse', clientX: 150, clientY: 12 })

      await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
      expect(playlistRequests(fetchMock)[1]).toContain('/chase/offset/15/playlist.m3u8')
    })

  const rect600 = () => ({ x: 0, y: 0, left: 0, top: 0, right: 600, bottom: 24, width: 600, height: 24, toJSON: () => ({}) })
  const percent = (el: Element | null, prop: 'left' | 'width') =>
    Number.parseFloat((el as HTMLElement | null)?.style[prop] ?? 'NaN')

  it('マウスのホバーは吹き出しだけを出し、つまみ・再生済みの塗り・時刻・aria-valuenow は再生位置のまま', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={513}
        chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 3600, recordedSeconds: 1501 }}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    Object.defineProperty(slider, 'getBoundingClientRect', { value: rect600 })
    const thumb = screen.getByTestId('seek-thumb')
    const played = screen.getByTestId('chase-timeline-played')
    expect(slider).toHaveAttribute('aria-valuenow', '0')
    expect(screen.getByTestId('playback-time')).toHaveTextContent('0:00 / 録画済み 25:01')

    // 37:00 は録画の先端（25:01）より先 = まだ録画されていない。
    fireEvent.pointerMove(slider, { pointerId: 4, pointerType: 'mouse', clientX: 370, clientY: 12 })
    expect(screen.getByTestId('chase-hover-label')).toHaveTextContent('37:00 · まだ録画されていません')
    expect(slider).toHaveAttribute('aria-valuenow', '0')
    expect(slider.getAttribute('aria-valuetext')).toBe('0:00 / 録画済み 25:01')
    expect(percent(thumb, 'left')).toBe(0)
    expect(percent(played, 'width')).toBe(0)
    expect(screen.getByTestId('playback-time')).toHaveTextContent('0:00 / 録画済み 25:01')

    // 録画済みの中なら時刻だけ。
    fireEvent.pointerMove(slider, { pointerId: 4, pointerType: 'mouse', clientX: 120, clientY: 12 })
    expect(screen.getByTestId('chase-hover-label').textContent).toBe('12:00')
    expect(slider).toHaveAttribute('aria-valuenow', '0')

    fireEvent.pointerLeave(slider, { pointerId: 4, pointerType: 'mouse' })
    expect(screen.queryByTestId('chase-hover-label')).not.toBeInTheDocument()
  })

  it('チューナーが番組開始前に開いていても追っかけ軸は番組開始を 0 秒にする', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={514}
        chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 3600, recordedSeconds: 1500 }}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    expect(slider).toHaveAttribute('aria-valuemin', '0')
    expect(slider).toHaveAttribute('aria-valuenow', '0')
    expect(slider.getAttribute('aria-valuetext')).toBe('0:00 / 録画済み 25:00')
    expect(screen.getByTestId('playback-time')).toHaveTextContent('0:00 / 録画済み 25:00')
    expect(screen.getByTestId('chase-timeline-labels').firstElementChild).toHaveTextContent('0:00')
  })

  it('番組開始より遅れて録り始めた録画は、録画済みと再生済みの塗りを録画開始の位置から始める', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="chase"
        site="default"
        recordingId={515}
        // 15 秒遅れて開始、録画済み 15 秒 → 先端は番組開始から 30 秒。軸は 0〜60 秒。
        chaseTimeline={{ chaseHeadOffsetSeconds: 15, plannedSeconds: 60, recordedSeconds: 15 }}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const recorded = screen.getByTestId('chase-timeline-recorded')
    expect(percent(recorded, 'left')).toBe(25)
    expect(percent(recorded, 'width')).toBe(25)
    const played = screen.getByTestId('chase-timeline-played')
    expect(percent(played, 'left')).toBe(25)
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    expect(slider).toHaveAttribute('aria-valuenow', '15')
    // 録画開始より前（6 秒）も録っていない部分なので、ホバーの吹き出しに添える。
    Object.defineProperty(slider, 'getBoundingClientRect', { value: rect600 })
    fireEvent.pointerMove(slider, { pointerId: 7, pointerType: 'mouse', clientX: 60, clientY: 12 })
    expect(screen.getByTestId('chase-hover-label')).toHaveTextContent('0:06 · まだ録画されていません')
    fireEvent.pointerMove(slider, { pointerId: 7, pointerType: 'mouse', clientX: 200, clientY: 12 })
    expect(screen.getByTestId('chase-hover-label').textContent).toBe('0:20')
  })

  it('再生中にセッション外へシークしたら、張り直したセッションで再生を続ける（原本 HLS と同じ）', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    function Harness() {
      return (
        <LivePlayer
          mode="chase"
          site="default"
          recordingId={517}
          chaseTimeline={chaseTimeline}
        />
      )
    }
    render(<Harness />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 4 }, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.canPlay(video)
    fireEvent.play(video)
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    Object.defineProperty(slider, 'getBoundingClientRect', { value: rect600 })
    // 軸 60 秒の 15 秒（変換済みの端 4 秒より先、録画の先端 19 秒より手前）。
    fireEvent.pointerDown(slider, { pointerId: 5, pointerType: 'mouse', clientX: 150 })
    fireEvent.pointerUp(slider, { pointerId: 5, pointerType: 'mouse', clientX: 150 })
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/517/chase/offset/15/playlist.m3u8',
    )
    expect(screen.getAllByRole('button', { name: '一時停止' }).length).toBeGreaterThan(0)
    expect(play).not.toHaveBeenCalled()
    fireEvent.canPlay(video)
    expect(play).toHaveBeenCalledOnce()
  })

  it('延長中に軸が伸びても、離したときはドラッグ中に見せた位置を確定する', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    const props = { mode: 'chase' as const, site: 'default', recordingId: 518 }
    const { rerender } = render(
      <LivePlayer {...props} chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 60, recordedSeconds: 120 }} />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    Object.defineProperty(slider, 'getBoundingClientRect', { value: rect600 })
    fireEvent.pointerDown(slider, { pointerId: 6, pointerType: 'mouse', clientX: 300 })
    fireEvent.pointerMove(slider, { pointerId: 6, pointerType: 'mouse', clientX: 300 })
    expect(slider).toHaveAttribute('aria-valuenow', '60')
    // 1 秒ごとに録画の先端が伸び、同じ座標の秒が変わる。
    rerender(
      <LivePlayer {...props} chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 60, recordedSeconds: 122 }} />,
    )
    fireEvent.pointerUp(slider, { pointerId: 6, pointerType: 'mouse', clientX: 300 })
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/518/chase/offset/60/playlist.m3u8',
    )
  })

  it('予定を超えて録画が続いたら、軸を先端まで広げ、予定終端と延長中の先端を分で出す', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    const props = {
      mode: 'chase' as const,
      site: 'default',
      recordingId: 516,
    }
    const { rerender } = render(
      <LivePlayer {...props} chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 3600, recordedSeconds: 1501 }} />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    // 先端の印の下は時刻だけ。押すと先端へ移る説明は印のアクセシブル名と title に置く。
    expect(screen.getByTestId('chase-live-edge-label').textContent).toBe('25:01')
    const edgeButton = screen.getByRole('button', { name: '録画の先端へ' })
    expect(edgeButton).toHaveAttribute('title', '録画の先端 25:01（押すと先端へ）')
    expect(screen.getByTestId('chase-timeline-end')).toHaveTextContent('60:00 まで（予定）')
    expect(percent(screen.getByTestId('chase-timeline-planned-end'), 'left')).toBe(100)

    rerender(
      <LivePlayer {...props} chaseTimeline={{ chaseHeadOffsetSeconds: 0, plannedSeconds: 3600, recordedSeconds: 4212 }} />,
    )
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    expect(slider).toHaveAttribute('aria-valuemax', '4212')
    expect(screen.queryByTestId('chase-live-edge-label')).not.toBeInTheDocument()
    expect(screen.getByTestId('chase-timeline-planned-label')).toHaveTextContent('予定 60:00 ¦')
    expect(screen.getByTestId('chase-timeline-end')).toHaveTextContent('延長中 · 先端 70:12')
    expect(screen.getByTestId('playback-time')).toHaveTextContent('0:00 / 録画済み 70:12')
    expect(percent(screen.getByTestId('chase-timeline-planned-end'), 'left')).toBeCloseTo((3600 / 4212) * 100, 5)
    expect(percent(screen.getByTestId('chase-timeline-recorded'), 'width')).toBe(100)
  })
})

describe('LivePlayer / 原本 VOD 操作バー（issue #1014）', () => {
  const playlistRequests = (fetchMock: ReturnType<typeof vi.fn>) =>
    fetchMock.mock.calls.map(([url]) => String(url)).filter((url) => url.includes('playlist.m3u8'))

  it('原本は native controls を外して共通バーを使い、音声・視聴状態を操作できる', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const putWatched = vi.fn()
    const deleteWatched = vi.fn()
    const { rerender } = render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={410}
        recordingDurationMs={30_000}
        profile="hd"
        availableProfiles={[{ name: 'hd', height: 720 }, { name: 'sd', height: 360 }]}
        onPutWatched={putWatched}
        onDeleteWatched={deleteWatched}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const video = document.querySelector('video')!
    expect(video.controls).toBe(false)
    expect(screen.getAllByRole('slider', { name: 'シークバー' })).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const settings = screen.getByRole('menu', { name: '再生設定' })
    expect(within(settings).getByRole('menuitem', { name: '画質' })).toHaveAccessibleDescription('hd（720p）')
    expect(screen.queryByRole('link', { name: 'encoded 動画をダウンロード' })).not.toBeInTheDocument()

    const hls = hlsMockState.instances[0]!
    hls.audioTracks = [{}, {}, {}]
    expect(within(settings).getByRole('menuitem', { name: '音声' })).toHaveAccessibleDescription('標準')
    const audio = openPlaybackSettingsSubmenu('音声')
    expect(within(audio).getAllByRole('menuitemradio').map((item) => item.textContent?.trim())).toEqual([
      '標準',
      '主音声',
      '副音声',
    ])
    fireEvent.click(within(audio).getByRole('menuitemradio', { name: '主音声' }))
    expect(hls.audioTrack).toBe(1)
    expect(hlsMockState.instances).toHaveLength(1)
    expect(playlistRequests(fetchMock)).toHaveLength(1)

    fireEvent.click(screen.getByTitle('視聴済みにする'))
    expect(putWatched).toHaveBeenCalledOnce()
    rerender(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={410}
        recordingDurationMs={30_000}
        profile="hd"
        availableProfiles={[{ name: 'hd', height: 720 }, { name: 'sd', height: 360 }]}
        watched
        onPutWatched={putWatched}
        onDeleteWatched={deleteWatched}
      />,
    )
    fireEvent.click(screen.getByTitle('未視聴に戻す'))
    expect(deleteWatched).toHaveBeenCalledOnce()
    rerender(<LivePlayer site="default" networkId={0} serviceId={1024} />)
    expect(document.querySelector('video')!.controls).toBe(false)
  })

  it('mode=live はネイティブ controls を外してプレイヤー操作バーを使う', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))
    render(<LivePlayer mode="live" site="default" networkId={1} serviceId={2} />)
    expect(document.querySelector('video')!.controls).toBe(false)
    expect(screen.getByTestId('player-controls')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '再生' })).toBeInTheDocument()
  })

  // 枠の振る舞いは encoded の RecordingPlayer と同じフック（use-player-frame）を使う。
  // 実ブラウザの判定は web/e2e/recording-original-vod.mjs ⑤-b / ⑤-g。
  it('原本 VOD: マウスでバーのボタンを押して再生した後も、3 秒で操作が隠れる', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(<LivePlayer mode="original-vod" site="default" recordingId={418} recordingDurationMs={30_000} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    vi.useFakeTimers()
    const video = document.querySelector('video')!
    const controls = screen.getByTestId('player-controls')
    const play = within(controls).getAllByRole('button', { name: '再生' })[0]!
    // マウスで押したボタンに残るフォーカス（:focus-visible ではない）
    act(() => play.focus())
    fireEvent.play(video)
    act(() => vi.advanceTimersByTime(3100))
    expect(controls).toHaveAttribute('aria-hidden', 'true')
  })

  it('原本 VOD: タッチで映像を叩くと操作を出すだけで、再生 / 一時停止は切り替えない', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(<LivePlayer mode="original-vod" site="default" recordingId={420} recordingDurationMs={30_000} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    expect(video).toHaveAttribute('tabindex', '0')
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.pointerDown(video, { pointerType: 'touch' })
    fireEvent.click(video)
    expect(play).not.toHaveBeenCalled()
    fireEvent.pointerDown(video, { pointerType: 'mouse' })
    fireEvent.click(video)
    expect(play).toHaveBeenCalledOnce()
  })

  it('原本 VOD: 再生中にセッション外へシークしたら、張り直したセッションで再生を続ける', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(<LivePlayer mode="original-vod" site="default" recordingId={421} recordingDurationMs={60_000} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 8 }, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.canPlay(video)
    fireEvent.play(video)
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    vi.spyOn(slider, 'getBoundingClientRect').mockReturnValue({
      x: 0, y: 0, left: 0, top: 0, right: 100, bottom: 16, width: 100, height: 16, toJSON: () => ({}),
    } as DOMRect)
    fireEvent.pointerDown(slider, { pointerId: 1, pointerType: 'mouse', clientX: 50 })
    fireEvent.pointerUp(slider, { pointerId: 1, pointerType: 'mouse', clientX: 50 })
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/421/original-vod/offset/30/playlist.m3u8',
    )
    // load() は pause を発火しないので、再開するまでバーは再生中のまま
    expect(screen.getAllByRole('button', { name: '一時停止' }).length).toBeGreaterThan(0)
    expect(play).not.toHaveBeenCalled()
    fireEvent.canPlay(video)
    expect(play).toHaveBeenCalledOnce()
  })

  it('原本 VOD: playAround は offset セッションが再生を始めてから停止時計を動かす', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={427}
        recordingDurationMs={60_000}
        chapters={[{ startMs: 20_000, endMs: 25_000, label: 'CM', cut: true }]}
        chapterSource="auto"
        chapterVersion="chapters-v1"
        chapterEditing
        onSaveChapters={async () => true}
        onResetChapters={async () => true}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 5 }, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    const pause = vi.spyOn(video, 'pause').mockImplementation(() => undefined)
    fireEvent.playing(video)
    vi.useFakeTimers({ shouldAdvanceTime: true })

    fireEvent.click(screen.getByRole('button', { name: /前後3秒を再生/ }))
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/427/original-vod/offset/17/playlist.m3u8',
    )

    // offset セッションが canplay に至るまで 8 秒を超えても、未再生の時間は停止区間に含めない。
    act(() => vi.advanceTimersByTime(8_500))
    expect(pause).not.toHaveBeenCalled()
    const callsBeforeCanPlay = play.mock.calls.length
    fireEvent.canPlay(video)
    expect(play).toHaveBeenCalledTimes(callsBeforeCanPlay + 1)

    fireEvent.playing(video)
    video.currentTime = 6.1
    fireEvent.timeUpdate(video)
    expect(pause).toHaveBeenCalledOnce()
  })

  it('原本 VOD: offset セッションの canplay 待ちに境界を選ぶと保留中の再開を取り消す', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={428}
        recordingDurationMs={60_000}
        chapters={[{ startMs: 20_000, endMs: 25_000, label: 'CM', cut: true }]}
        chapterSource="auto"
        chapterVersion="chapters-v1"
        chapterEditing
        onSaveChapters={async () => true}
        onResetChapters={async () => true}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 5 }, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.playing(video)

    fireEvent.click(screen.getByRole('button', { name: /前後3秒を再生/ }))
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/428/original-vod/offset/17/playlist.m3u8',
    )

    // load() が停止させた再読み込み待ち中に、別の境界へ移動する。
    Object.defineProperty(video, 'paused', { value: true, writable: true, configurable: true })
    const boundary = document.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="25000"]',
    )!
    fireEvent.click(boundary)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(3))
    expect(hlsMockState.instances[2]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/428/original-vod/offset/25/playlist.m3u8',
    )

    const callsBeforeCanPlay = play.mock.calls.length
    fireEvent.canPlay(video)
    expect(play).toHaveBeenCalledTimes(callsBeforeCanPlay)
    expect(video.paused).toBe(true)
  })

  it('原本 VOD: 再生中に CM の自動スキップで変換の先端より先へ飛んでも再生を続ける', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={422}
        recordingDurationMs={60_000}
        chapters={[{ startMs: 6_000, endMs: 40_000, label: 'CM', cut: true }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 8 }, configurable: true })
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.canPlay(video)
    video.currentTime = 5
    fireEvent.timeUpdate(video)
    video.currentTime = 6.5
    fireEvent.timeUpdate(video)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/422/original-vod/offset/40/playlist.m3u8',
    )
    fireEvent.canPlay(video)
    expect(play).toHaveBeenCalledOnce()
  })

  it('原本 VOD: 末尾付近の 416 は、最後に取れた offset を下限に手前へ丸めて張り直す', async () => {
    // 映像は 57.5 秒。streamer は「映像の終端 - 0.5 秒」より後ろの offset を 416 にする。
    const fetchMock = vi.fn((url: string) => {
      const offset = Number(/\/offset\/(\d+)\//.exec(String(url))?.[1] ?? 0)
      return Promise.resolve(
        offset > 57
          ? new Response('offset is outside the original', { status: 416 })
          : new Response(PROFILE_MASTER, { status: 200 }),
      )
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<LivePlayer mode="original-vod" site="default" recordingId={423} recordingDurationMs={63_000} />)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 8 }, configurable: true })
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    vi.spyOn(slider, 'getBoundingClientRect').mockReturnValue({
      x: 0, y: 0, left: 0, top: 0, right: 100, bottom: 16, width: 100, height: 16, toJSON: () => ({}),
    } as DOMRect)
    fireEvent.pointerDown(slider, { pointerId: 1, pointerType: 'mouse', clientX: 99 })
    fireEvent.pointerUp(slider, { pointerId: 1, pointerType: 'mouse', clientX: 99 })
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    const requested = fetchMock.mock.calls
      .map(([url]) => String(url))
      .filter((url) => url.endsWith('playlist.m3u8'))
      .map((url) => /\/offset\/(\d+)\//.exec(url)?.[1])
      .filter(Boolean)
    // 62（416）→ 61（416）→ 59（416）→ 55
    expect(requested).toEqual(['62', '61', '59', '55'])
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/423/original-vod/offset/55/playlist.m3u8',
    )
    expect(screen.queryByText(/エラー/)).not.toBeInTheDocument()
  })

  it('原本 VOD（ネイティブ HLS）: 先頭・続きから位置なしでも canplay で 0 を明示し直す', async () => {
    // WebKit は ENDLIST の無い EVENT playlist をライブ端の近くから始める（e2e ⑤-a で 17.4 秒）。
    const { resolve } = deferredFetch()
    render(<LivePlayer mode="original-vod" site="default" recordingId={425} recordingDurationMs={60_000} />)
    const video = document.querySelector('video')!
    vi.spyOn(video, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' || type === 'video/mp2t' ? 'maybe' : '',
    )
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    resolve(new Response(PROFILE_MASTER, { status: 200 }))
    await waitFor(() => expect(video.getAttribute('src')).toContain('/original-vod/playlist.m3u8'))
    fireEvent.loadedMetadata(video)
    video.currentTime = 15.2
    fireEvent.canPlay(video)
    expect(video.currentTime).toBe(0)
  })

  it('原本 VOD: 前後チャプターのボタンで境界へ飛ぶ', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={424}
        recordingDurationMs={60_000}
        chapters={[{ startMs: 10_000, endMs: 15_000, label: '気象情報', cut: false }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', { value: { length: 1, start: () => 0, end: () => 20 }, configurable: true })
    fireEvent.canPlay(video)
    fireEvent.click(screen.getAllByRole('button', { name: '次のチャプター' })[0]!)
    expect(video.currentTime).toBeCloseTo(300.5 * FRAME_SECONDS, 12)
    fireEvent.click(screen.getAllByRole('button', { name: '次のチャプター' })[0]!)
    expect(video.currentTime).toBeCloseTo(450.5 * FRAME_SECONDS, 12)
    fireEvent.click(screen.getAllByRole('button', { name: '前のチャプター' })[0]!)
    expect(video.currentTime).toBeCloseTo(300.5 * FRAME_SECONDS, 12)
  })

  it('原本の設定メニューはエンコード版と同じ順で、音声を画質の隣に置く', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={426}
        recordingDurationMs={30_000}
        chapters={[{ startMs: 6_000, endMs: 8_000, label: 'CM', cut: true }]}
        availableProfiles={[{ name: 'hd', height: 720 }, { name: 'sd', height: 360 }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const settings = screen.getByRole('menu', { name: '再生設定' })
    // DOM 順 = デスクトップの見た目（画質が歯車に近い最下段）。スマホは CSS で逆順（画質が先頭）。
    const rows = Array.from(settings.querySelectorAll('[role^="menuitem"]')).map((item) => item.getAttribute('aria-label'))
    expect(rows.slice(0, 5)).toEqual(['CM を飛ばす', '字幕', '再生速度', '音声', '画質'])
  })

  it('原本の画質メニューは profile が 1 件なら表示しない', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={411}
        recordingDurationMs={30_000}
        availableProfiles={[{ name: 'hd', height: 720 }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const settings = screen.getByRole('menu', { name: '再生設定' })
    expect(within(settings).queryByRole('menuitem', { name: '画質' })).not.toBeInTheDocument()
    expect(within(settings).getByRole('menuitem', { name: '音声' })).toHaveAccessibleDescription('標準')
  })

  it('原本 HLS はチャプターを描き、再生中に cut 区間を自動で飛ばす', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    const chapters = [{ startMs: 6_000, endMs: 8_000, label: 'CM', cut: true }]
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={417}
        recordingDurationMs={30_000}
        chapters={chapters}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'duration', { value: 30, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 30 },
      configurable: true,
    })
    fireEvent.loadedMetadata(video)
    fireEvent.canPlay(video)
    expect(screen.getByTestId('chapter-marker')).toHaveAttribute('data-cut', 'true')

    video.currentTime = 5
    fireEvent.timeUpdate(video)
    video.currentTime = 6.5
    fireEvent.timeUpdate(video)
    expect(video.currentTime).toBeCloseTo(240.5 * FRAME_SECONDS, 12)

    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const skip = screen.getByRole('menuitemcheckbox', { name: 'CM を飛ばす' })
    expect(skip).toHaveAttribute('aria-checked', 'true')
    fireEvent.click(skip)
    expect(skip).toHaveAttribute('aria-checked', 'false')
    expect(localStorage.getItem('rokuban:chapter-skip')).toBe('off')
  })

  it('保存位置から cut 区間内へ再開したときは自動スキップしない', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))))
    const chapters = [{ startMs: 6_000, endMs: 8_000, label: 'CM', cut: true }]
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={419}
        offsetSessionStart={{ type: 'saved-position', positionMs: 6_500 }}
        recordingDurationMs={30_000}
        chapters={chapters}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))

    const video = document.querySelector('video')!
    Object.defineProperty(video, 'paused', { value: false, writable: true, configurable: true })
    Object.defineProperty(video, 'duration', { value: 30, writable: true, configurable: true })
    // 6.5 秒は offset 6 のセッション（起点 5.97263 秒）の 0.52737 秒。cut の終端（8 秒）は 2.02737 秒。
    Object.defineProperty(video, 'currentTime', { value: 0.52737, writable: true, configurable: true })
    fireEvent.canPlay(video)
    fireEvent.seeking(video)
    fireEvent.timeUpdate(video)
    expect(video.currentTime).toBeCloseTo(0.52737, 4)
  })

  it('シークはキーを離すまで確定せず、セッション範囲内なら playlist を取り直さない', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer mode="original-vod" site="default" recordingId={412} recordingDurationMs={30_000} />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    fireEvent.canPlay(video)
    Object.defineProperty(video, 'currentTime', { value: 5, writable: true, configurable: true })
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 20 },
      configurable: true,
    })
    fireEvent.timeUpdate(video)
    const slider = screen.getByRole('slider', { name: 'シークバー' })

    fireEvent.keyDown(slider, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(5)
    expect(playlistRequests(fetchMock)).toHaveLength(1)
    fireEvent.keyUp(slider, { key: 'ArrowRight' })

    expect(video.currentTime).toBe(15)
    expect(hlsMockState.instances).toHaveLength(1)
    expect(playlistRequests(fetchMock)).toHaveLength(1)
  })

  it('ドラッグ中は preview だけを更新し、pointerup で一度だけ offset playlist を取る', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer mode="original-vod" site="default" recordingId={414} recordingDurationMs={30_000} />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 8 },
      configurable: true,
    })
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    vi.spyOn(slider, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 100,
      bottom: 16,
      width: 100,
      height: 16,
      toJSON: () => ({}),
    } as DOMRect)

    fireEvent.pointerDown(slider, { pointerId: 1, pointerType: 'touch', clientX: 30 })
    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'touch', clientX: 45 })
    fireEvent.pointerMove(slider, { pointerId: 1, pointerType: 'touch', clientX: 60 })
    expect(playlistRequests(fetchMock)).toHaveLength(1)
    expect(hlsMockState.instances).toHaveLength(1)
    fireEvent.pointerUp(slider, { pointerId: 1, pointerType: 'touch', clientX: 60 })

    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/414/original-vod/offset/18/playlist.m3u8',
    )
    expect(playlistRequests(fetchMock)).toHaveLength(2)
  })

  it('画質メニュー切替では原本時間の再生位置を持ち越す', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={415}
        recordingDurationMs={30_000}
        profile="hd"
        availableProfiles={[{ name: 'hd', height: 720 }, { name: 'sd', height: 360 }]}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    fireEvent.canPlay(video)
    video.currentTime = 12
    fireEvent.timeUpdate(video)

    choosePlaybackOption('画質', /sd（360p）/)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    video.currentTime = 0
    fireEvent.loadedMetadata(video)
    expect(video.currentTime).toBe(12)
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/415/original-vod/playlist.m3u8?profile=sd',
    )
    expect(playlistRequests(fetchMock)).toHaveLength(2)
  })

  it('変換端より後ろはキーを離した時だけ offset URL で張り直す', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={413}
        recordingDurationMs={30_000}
        profile="hd"
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 8 },
      configurable: true,
    })
    const slider = screen.getByRole('slider', { name: 'シークバー' })

    fireEvent.keyDown(slider, { key: 'ArrowRight' })
    fireEvent.keyDown(slider, { key: 'ArrowRight', repeat: true })
    expect(playlistRequests(fetchMock)).toHaveLength(1)
    expect(hlsMockState.instances).toHaveLength(1)
    fireEvent.keyUp(slider, { key: 'ArrowRight' })

    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/413/original-vod/offset/20/playlist.m3u8?profile=hd',
    )
    expect(playlistRequests(fetchMock).some((url) => url.includes('/original-vod/offset/20/playlist.m3u8')))
      .toBe(true)
  })

  it('offset セッションの再開位置はフレーム境界の起点 + currentTime の原本時刻で送る', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method && init.method !== 'GET') return Promise.resolve(new Response(null, { status: 204 }))
      return Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))
    })
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={418}
        recordingDurationMs={120_000}
        offsetSessionStart={{ type: 'offset', offsetSeconds: 15 }}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    fireEvent.canPlay(video)
    video.currentTime = 5
    fireEvent.timeUpdate(video)
    expect(Number(screen.getByRole('slider', { name: 'シークバー' }).getAttribute('aria-valuenow'))).toBeCloseTo(19.981633333, 6)
    fireEvent.pause(video)

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/recordings/418/playback-position')).toBe(true)
    })
    const positionWrite = fetchMock.mock.calls.find(([url]) => String(url) === '/api/recordings/418/playback-position')
    expect(JSON.parse(String(positionWrite?.[1]?.body))).toEqual({ positionMs: 19_981 })
  })

  it('ENDLIST 後だけ実尺の 90% で watched を送り、成功後に親へ通知する', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method) return Promise.resolve(new Response(null, { status: 204 }))
      return Promise.resolve(new Response(PROFILE_MASTER, { status: 200 }))
    })
    vi.stubGlobal('fetch', fetchMock)
    const onWatched = vi.fn()
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={417}
        recordingDurationMs={120_000}
        onWatched={onWatched}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    // The actual recording lasts 120s, while EVENT duration is merely the
    // current conversion edge and intentionally differs.
    Object.defineProperty(video, 'duration', { value: 300, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    fireEvent.canPlay(video)
    const levelLoaded = hlsMockState.instances[0]!.on.mock.calls.find((call) => call[0] === 'hlsLevelLoaded')![1]

    video.currentTime = 118
    fireEvent.timeUpdate(video)
    fireEvent.pause(video)
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/recordings/417/playback-position')).toBe(true))
    const requests = () => fetchMock.mock.calls.filter(([url]) => String(url).includes('/api/recordings/417/'))
    expect(requests().some(([url]) => String(url).endsWith('/watched'))).toBe(false)
    expect(onWatched).not.toHaveBeenCalled()

    levelLoaded('hlsLevelLoaded', { fatal: false, details: { live: true } })
    video.currentTime = 119
    fireEvent.timeUpdate(video)
    expect(requests().some(([url]) => String(url).endsWith('/watched'))).toBe(false)

    levelLoaded('hlsLevelLoaded', { fatal: false, details: { live: false } })
    fireEvent.timeUpdate(video)
    await waitFor(() => expect(requests().filter(([url]) => String(url).endsWith('/watched'))).toHaveLength(1))
    await waitFor(() => expect(onWatched).toHaveBeenCalledOnce())
  })

  it('現在の offset より前はその秒数の offset セッションへ戻す', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={416}
        recordingDurationMs={30_000}
        offsetSessionStart={{ type: 'offset', offsetSeconds: 15 }}
        profile="hd"
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    expect(hlsMockState.instances[0]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/416/original-vod/offset/15/playlist.m3u8?profile=hd',
    )
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 20 },
      configurable: true,
    })
    fireEvent.loadedMetadata(video)
    const slider = screen.getByRole('slider', { name: 'シークバー' })

    fireEvent.keyDown(slider, { key: 'ArrowLeft' })
    expect(hlsMockState.instances).toHaveLength(1)
    fireEvent.keyUp(slider, { key: 'ArrowLeft' })

    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/416/original-vod/offset/4/playlist.m3u8?profile=hd',
    )
  })

  it('同じ player instance で録画 ID を替えると offset・画質・音声を新しい録画の props に戻す', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(new Response(PROFILE_MASTER, { status: 200 })))
    vi.stubGlobal('fetch', fetchMock)
    const profiles = [{ name: 'hd', height: 720 }, { name: 'sd', height: 360 }] as const
    const { rerender } = render(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={419}
        recordingDurationMs={30_000}
        profile="hd"
        availableProfiles={profiles}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(1))
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'seekable', {
      value: { length: 1, start: () => 0, end: () => 8 },
      configurable: true,
    })
    const slider = screen.getByRole('slider', { name: 'シークバー' })
    fireEvent.keyDown(slider, { key: 'ArrowRight' })
    fireEvent.keyUp(slider, { key: 'ArrowRight' })
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(2))
    expect(hlsMockState.instances[1]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/419/original-vod/offset/10/playlist.m3u8?profile=hd',
    )

    choosePlaybackOption('画質', /sd（360p）/)
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(3))
    choosePlaybackOption('音声', /副音声/)

    rerender(
      <LivePlayer
        mode="original-vod"
        site="default"
        recordingId={420}
        recordingDurationMs={30_000}
        profile="hd"
        availableProfiles={profiles}
      />,
    )
    await waitFor(() => expect(hlsMockState.instances).toHaveLength(4))
    expect(hlsMockState.instances[3]!.loadSource).toHaveBeenCalledWith(
      '/api/sites/default/recordings/420/original-vod/playlist.m3u8?profile=hd',
    )
    fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
    const settings = screen.getByRole('menu', { name: '再生設定' })
    expect(within(settings).getByRole('menuitem', { name: '画質' })).toHaveAccessibleDescription('hd（720p）')
    expect(within(settings).getByRole('menuitem', { name: '音声' })).toHaveAccessibleDescription('標準')
  })
})

describe('LivePlayer / 再生前のエラー（再生元の選び直し）', () => {
  // 押した ▶（autoPlay）の意図は、始まる前のエラーでも親へ渡す（選び直した先で再生を続ける）。
  // 止めたまま移ったセッションのエラーは「再生していた」にしない。位置はどちらも作らない。
  it.each([
    [true, true],
    [false, false],
  ])('autoPlay=%s のセッションが再生前に 404 なら wasPlaying=%s・位置 undefined で親へ渡す', async (autoPlay, expected) => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('gone\n', { status: 404 }))))
    const onError = vi.fn(() => true)
    render(
      <LivePlayer mode="chase" site="default" recordingId={7} autoPlay={autoPlay} onRecordingPlaybackError={onError} />,
    )
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1))
    expect(onError).toHaveBeenCalledWith(undefined, expected)
    // 親が選び直したのでエラー表示に落ちない。
    expect(screen.queryByRole('button', { name: '再読み込み' })).not.toBeInTheDocument()
  })
})
