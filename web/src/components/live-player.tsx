import { useEffect, useRef, useState } from 'react'

import type { LiveLoadError } from '@/lib/live'
import {
  claimsHlsPlaylistSupport,
  livePlaylistURL,
  probeLivePlaylist,
  sendLiveLeaveHint,
  supportsNativeHls,
} from '@/lib/live'
import { cn } from '@/lib/utils'

/** HlsLike は hls.js の型を静的 import せずに使うための最小限の形。 */
type HlsLike = {
  destroy(): void
  loadSource(url: string): void
  attachMedia(media: HTMLMediaElement): void
  on(event: string, callback: (event: string, data: { fatal: boolean }) => void): void
}

type LivePlayerProps = {
  site: string
  /** SI の networkId。mirakc 合成 service id の組み立てに使う（issue #208）。 */
  networkId: number
  /** SI の serviceId。パスに載る前に networkId と合成する（issue #208）。 */
  serviceId: number
  className?: string
}

/**
 * nativeStallTimeoutMs はネイティブ HLS 経路で「止まったまま」と見なすまでの猶予
 * （テストから参照するので export する）。
 *
 * `stalled` / `waiting` が来ただけでは失敗ではない --- ライブ配信は正常時にも
 * バッファ枯れで一時的に止まる。この猶予の間に `playing` / `canplay` /
 * `timeupdate` のいずれかが来れば回復と見なしてタイマーを捨てる。
 *
 * 12 秒にしたのは、WebKit が `stalled` を出すのがデータ途絶から 3 秒後
 * （HTML 仕様の「3 秒以上データが来ない」規定。実測でも 3.6 秒）で、
 * streamer 側のセグメント長が 2 秒（`internal/streamer/live.go` の
 * `-hls_time 2`）だから --- 正常なら 3 セグメント以上落ちないと到達しない。
 */
export const nativeStallTimeoutMs = 12_000

/**
 * LivePlayer はライブ視聴の HLS プレイリストを再生する（M4-4）。
 *
 * 再生に先立ち `probeLivePlaylist` で 1 回プレイリストを取得し、成功したときだけ
 * `<video>` / hls.js に URL を渡す（`<video>` の `error` イベントは HTTP
 * ステータス・本文を運ばないため、事前確認でしか区別できない）。ネイティブ HLS
 * 対応（Safari）はそちらを使い、hls.js は動的 import で読み込む（バンドルサイズ、
 * issue #92 の着手時コメント参照）。
 *
 * チャンネル切り替えは呼び出し側が `serviceId` を変えて渡す（`key` での再マウントを
 * 前提にしない --- effect の cleanup で確実に破棄する）。破棄すると即座にセグメント
 * 要求が止まり、あわせて**離脱のヒント**を送る（下の effect）。ヒントはセッションを
 * 止めるのではなく idle 期限を短い猶予まで詰めるだけなので、同じチャンネルを見て
 * いる別の視聴者がいれば何も起きない（`lib/live.ts` の `sendLiveLeaveHint`）。
 */
export function LivePlayer({ site, networkId, serviceId, className }: LivePlayerProps) {
  const videoRef = useRef<HTMLVideoElement>(null)
  const hlsRef = useRef<HlsLike | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<LiveLoadError | null>(null)
  // retryNonce を変えると effect が再実行される（依存配列に入れる）
  const [retryNonce, setRetryNonce] = useState(0)

  // ライブのページキー操作は M / F だけ。録画向けの速度変更は出さない。
  // ネイティブ HLS の playbackRate が実 Safari で有効かは未検証。
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const video = videoRef.current
      if (!video || event.ctrlKey || event.metaKey || event.altKey) return
      if (
        event.target instanceof Element &&
        event.target.closest('input, textarea, select, button, a, video, [contenteditable]')
      ) {
        return
      }

      switch (event.key.toLowerCase()) {
        case 'm':
          video.muted = !video.muted
          event.preventDefault()
          break
        case 'f':
          void video.requestFullscreen?.()
          event.preventDefault()
      }
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  useEffect(() => {
    let cancelled = false
    // 切り替え・破棄が起きたら probe の fetch 自体を中断する。
    // `playlistStartupTimeout`（streamer 側、15 秒）ぶん in-flight のまま
    // 残さないため（レビュー #190 の指摘）。
    const controller = new AbortController()
    // effect の設定時点で 1 回だけ読む。cleanup で `videoRef.current` を直接
    // 読むと「クリーンアップが走る時点でもまだ同じノードを指しているか」が
    // 保証できない（react-hooks/exhaustive-deps が指摘する形）ため、同じ
    // effect の中で捕まえた変数を setup・cleanup の両方から使う。
    const video = videoRef.current
    setLoading(true)
    setError(null)

    const url = livePlaylistURL(site, networkId, serviceId)

    // teardown はこの effect が張ったものを外す手続き（メディアイベントの
    // リスナと stall 監視のタイマー）。cleanup から呼ぶ
    const teardown: Array<() => void> = []

    /**
     * watchNativeMedia はネイティブ HLS 経路の失敗を表面化する。
     *
     * **probe が通ってもメディア層は死にうる**（プレイリストは 200 で返るが
     * セグメントが 404 / 応答しない / 中身が壊れている）。probe は HTTP 層しか
     * 見ないので、ここを聴かないと**永久に止まった黒いプレイヤー**になる ---
     * 文言も読み込み表示も再読み込みボタンも出ない（レビュー #190 の 3 回目の
     * 指摘。WebKit で実測された症状）。
     *
     * 聴く 2 種は WebKit での実測に基づく（`E2E_URL` のスタブに対して
     * プレイリスト 200 + セグメント 404 / 応答しない / 壊れた中身の 3 通り）:
     *
     * | 壊し方 | 出るもの | `video.error` |
     * |---|---|---|
     * | セグメント 404 | `error`（+ 再生中なら `waiting`） | code 3 `Media failed to decode` |
     * | セグメントが応答しない | `progress` → `stalled`（3.6 秒後） | null |
     * | プレイリストの中身が壊れている | `progress` → `stalled`（3.6 秒後） | null |
     *
     * **`error` だけでは足りない**（下 2 つは error を出さない）し、`stalled` /
     * `waiting` を即座に失敗と見なすのも誤り（正常なライブでも一時的に出る）。
     * だから `error` は即時、`stalled` / `waiting` は
     * `nativeStallTimeoutMs` の猶予つきにする。
     *
     * hls.js 経路には張らない --- あちらは `Hls.Events.ERROR` が同じ役目を持ち、
     * MSE のバッファ制御で `waiting` が正常に何度も出るので、ここで拾うと
     * 誤検知になる。
     */
    function watchNativeMedia(media: HTMLVideoElement) {
      let stallTimer: ReturnType<typeof setTimeout> | null = null
      // 一度でも再生が始まったか。**`paused` だけを抑止条件にすると「まだ再生を
      // 押していない」状態まで飲み込む** --- `<video>` に `autoPlay` は無いので
      // 読み込み直後は常に `paused === true` であり、そこで配信が死んでいると
      // （プレイリストは 200 だがセグメントが無応答）唯一届くイベントが
      // `paused=true` の `stalled` なので、猶予が一度も張られず**永久に黒いまま
      // 何も出ない**（レビュー #190 の 5 回目の指摘。実 WebKit で
      // loadstart→progress→stalled のあと 20 秒待っても他のイベントは来ない）。
      // 抑止したいのは「再生していたユーザーが自分で止めた」場合だけである。
      let hasStarted = false
      const clearStallTimer = () => {
        if (stallTimer !== null) {
          clearTimeout(stallTimer)
          stallTimer = null
        }
      }
      const failed = (message: string) => {
        if (cancelled) return
        clearStallTimer()
        setError({ kind: 'other', status: 0, message })
        setLoading(false)
      }
      const onError = () =>
        failed('ライブ映像を再生できませんでした（映像データを読み込めません）')
      const onStall = () => {
        // **一時停止中の stall は失敗ではない。** WebKit は pause した瞬間に
        // `stalled` を出す（フェッチを止めるため）が、配信は正常なまま。しかも
        // 解除イベント（playing / canplay / timeupdate）は一時停止中には来ないので、
        // ここを見ないと猶予が必ず満了して**正常な配信にエラー画面が出る** ---
        // さらに `<video>` が invisible になり、ユーザーが一時停止した映像そのものが
        // 隠れる（レビュー #190 の 4 回目の指摘。WebKit で実測:
        // playing@0.0 → pause@2.3 → stalled@2.3 → 12 秒後にエラー表示）。
        //
        // hls.js 経路にこの watcher を張らない理由（MSE は正常時にも `waiting` を
        // 頻繁に出す）と同じ危険が、ネイティブ経路の `stalled` で現実化したもの。
        if (cancelled || (hasStarted && media.paused) || stallTimer !== null) return
        stallTimer = setTimeout(
          () => failed('ライブ映像が届いていません（映像データが途絶えました）'),
          nativeStallTimeoutMs,
        )
      }
      // `pause` も回復扱いにする（猶予の途中で一時停止された場合）。再開後に配信が
      // 本当に死んでいれば `waiting` が再び出て、そこで張り直される --- 実 WebKit で
      // 測った（再生中に stall → pause@6.05s → play@12.05s → waiting@12.05s）。
      // 「張り直す」側は `再開後に配信が復帰していなければ再びエラーになる` が守る
      const onProgress = () => clearStallTimer()
      const onPlaying = () => {
        hasStarted = true
        clearStallTimer()
      }

      media.addEventListener('error', onError)
      media.addEventListener('stalled', onStall)
      media.addEventListener('waiting', onStall)
      media.addEventListener('playing', onPlaying)
      media.addEventListener('canplay', onProgress)
      media.addEventListener('timeupdate', onProgress)
      media.addEventListener('pause', onProgress)
      teardown.push(() => {
        clearStallTimer()
        media.removeEventListener('error', onError)
        media.removeEventListener('stalled', onStall)
        media.removeEventListener('waiting', onStall)
        media.removeEventListener('playing', onPlaying)
        media.removeEventListener('canplay', onProgress)
        media.removeEventListener('timeupdate', onProgress)
        media.removeEventListener('pause', onProgress)
      })
    }

    async function start() {
      let probe: Awaited<ReturnType<typeof probeLivePlaylist>>
      try {
        probe = await probeLivePlaylist(url, controller.signal)
      } catch (err) {
        // 中断（チャンネル切り替え・破棄）は無視する。エラー表示にはしない ---
        // 単に「もう見たいものが変わった」だけで、失敗ではない
        if (err instanceof DOMException && err.name === 'AbortError') return
        throw err
      }
      if (cancelled) return
      if (!probe.ok) {
        setError(probe.error)
        setLoading(false)
        return
      }

      if (!video) return

      const canPlayType = video.canPlayType.bind(video)

      // 再生経路は 3 段の梯子で選ぶ。**各段は「実際に確かめた能力」で選ばれる**
      // （レビュー #190 の 2 回目の指摘。それまでは m3u8 の MIME への戻り値だけを
      // 見ていたが、あれはどの実ブラウザでも Safari と Chrome を区別しない）:
      //
      //   1. `<video>` がプレイリストもセグメント（`video/mp2t`）も再生できる
      //      → ネイティブ。hls.js は import すらしない（約 520 KB を読ませない）
      //   2. hls.js が動く（MSE / ManagedMediaSource がある）→ hls.js
      //   3. どちらも駄目だが `<video>` が m3u8 に支持を表明する → ネイティブへ
      //      最後の望みを託す（`lib/live.ts` の `claimsHlsPlaylistSupport`）
      if (supportsNativeHls(canPlayType)) {
        // src を入れる前に張る（入れた後だと、失敗が速いときに取り逃がす）
        watchNativeMedia(video)
        video.src = url
      } else {
        const { default: Hls } = await import('hls.js')
        if (cancelled) return
        if (!Hls.isSupported()) {
          // MSE も ManagedMediaSource も無い（iOS 17.1 未満の iPhone Safari が
          // これに当たる）。hls.js では原理的に再生できないので、`<video>` 自身が
          // m3u8 に支持を表明しているならそちらへ渡す。ここを「非対応」と断じると、
          // ネイティブなら完璧に再生できる端末を締め出す。**渡して駄目だった場合は
          // `watchNativeMedia` が拾ってエラー表示 + 再読み込みを出す**
          // （`live-player.test.tsx` の「ネイティブ経路のメディア失敗」3 件 /
          // `web/e2e/live.mjs` ⑦）--- ここは 1 段目と同じ表面を持つ
          if (claimsHlsPlaylistSupport(canPlayType)) {
            watchNativeMedia(video)
            video.src = url
            setLoading(false)
            return
          }
          setError({
            kind: 'other',
            status: 0,
            message: 'このブラウザはライブ視聴（HLS）に対応していません',
          })
          setLoading(false)
          return
        }
        const hls = new Hls() as unknown as HlsLike
        hlsRef.current = hls
        hls.on(Hls.Events.ERROR, (_event, data) => {
          if (!data.fatal || cancelled) return
          // fatal のまま放置すると hls.js が内部でリトライを続け、エラー画面の
          // 裏でセグメント要求が続く（= idle GC も効かない。レビュー #190 の
          // 指摘）。表示するエラーは「壊れて止まった」なので、実際に止める
          hls.destroy()
          hlsRef.current = null
          setError({ kind: 'other', status: 0, message: 'ライブ再生中にエラーが発生しました' })
        })
        hls.loadSource(url)
        hls.attachMedia(video)
      }
      setLoading(false)
    }

    void start()

    return () => {
      cancelled = true
      controller.abort()
      // メディアイベントのリスナと stall タイマーを外す。
      //
      // **実際に効いている防御は `failed()` の `cancelled` チェックの方である。**
      // この行を `src` の解除より前に置いているのは「解除自体が出しうる `error` を
      // 拾わないため」だが、**その効き目は測れていない** --- 順序を入れ替えても、
      // さらに `cancelled` チェックを外しても、WebKit ではチャンネル切替で
      // `error` が出ず判定に差が出なかった（`web/e2e/live.mjs` で実測）。
      // 順序は無害な保険として残す。効くと分かっている主張ではない
      for (const fn of teardown) fn()
      hlsRef.current?.destroy()
      hlsRef.current = null
      if (video) {
        video.removeAttribute('src')
        video.load()
      }
    }
  }, [site, networkId, serviceId, retryNonce])

  // 離脱のヒント（issue #191）。**再生を担っているのはこのコンポーネントだけ**
  // なので、その生存（= このチャンネルを見ている間）にヒントの送信を紐づける。
  //
  // **probe の effect とは分ける。** あちらは `retryNonce` にも依存しており、
  // 同居させると「再読み込み」ボタンのたびに離脱ヒントが飛ぶ（直後の probe が
  // 期限を戻すので実害は無いが、`rokuban_live_leave_hints_total` が離脱以外の
  // 数を数えることになり、idle GC 回収数と対で読めなくなる）。
  //
  // 発火点は 2 系統:
  //
  //   - **cleanup**: チャンネル切り替え・再生停止・画面遷移（アンマウント）。
  //     `pages/live.tsx` は切り替え時に `playingKey` を落として
  //     `LivePlayer` を外すので、切り替えはここを必ず通る
  //   - **`pagehide` / `visibilitychange`（hidden）**: タブ・ウィンドウを閉じる、
  //     別アプリへ切り替える等。**`unload` は使わない** --- モバイル Safari では
  //     発火せず（bfcache のため）、`pagehide` が唯一届く終端イベントである。
  //     `visibilitychange` も併せて聴くのは、モバイルではタブが破棄されずに
  //     hidden のまま放置される経路があり、そこでは `pagehide` すら来ないため。
  //     **hidden で送っても壊れない**（音声だけ聴き続けている等でセグメント要求が
  //     続いていれば、その要求が期限を戻す。`lib/live.ts` の
  //     `sendLiveLeaveHint` 参照）
  useEffect(() => {
    const leave = () => sendLiveLeaveHint(site, networkId, serviceId)
    const onVisibilityChange = () => {
      if (document.visibilityState === 'hidden') leave()
    }
    window.addEventListener('pagehide', leave)
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      window.removeEventListener('pagehide', leave)
      document.removeEventListener('visibilitychange', onVisibilityChange)
      leave()
    }
  }, [site, networkId, serviceId])

  return (
    <div className={cn('relative aspect-video w-full max-w-3xl rounded bg-black', className)}>
      <video
        ref={videoRef}
        controls
        playsInline
        className={cn('size-full rounded', (loading || error) && 'invisible')}
      />

      {loading && !error && (
        <div
          role="status"
          className="absolute inset-0 flex items-center justify-center text-sm text-muted-foreground"
        >
          読み込み中…
        </div>
      )}

      {error && (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 px-4 text-center">
          <LiveErrorMessage error={error} />
          <button
            type="button"
            onClick={() => setRetryNonce((n) => n + 1)}
            className="rounded-md border border-border px-3 py-1.5 text-sm text-foreground transition-colors hover:bg-muted"
          >
            再読み込み
          </button>
        </div>
      )}
    </div>
  )
}

/**
 * LiveErrorMessage はエラー種別ごとの文言。
 *
 * `unreachable` は他 2 種と異なり赤（destructive）にしない --- ハイブリッド構成では
 * 自宅（streamer）が落ちているだけの正常状態でありうる（docs/overview.md
 * §サーバーレスデプロイ）。`capacity` / `other` は本文をそのまま見せる
 * （docs/frontend.md「エラーの本文も UI まで運ぶ」。400 を黙って隠さない、と同じ規律）。
 */
function LiveErrorMessage({ error }: { error: LiveLoadError }) {
  if (error.kind === 'unreachable') {
    return (
      <p className="text-sm text-muted-foreground">
        録画サーバーの自宅側に接続できません。自宅サーバーが起動しているか、
        ネットワークが繋がっているかを確認してください。
      </p>
    )
  }
  if (error.kind === 'capacity') {
    return (
      <div className="text-sm text-destructive">
        <p>いま視聴できません（チューナー不足または同時視聴数の上限）。</p>
        {/* 待てば直ることが読めないと「壊れている」と誤解される（レビュー #190 の
            指摘）。直前まで別のチャンネルを見ていた場合は、そのセッションの
            解放待ちである可能性が高い。切り替え時に離脱ヒントを送るので通常は
            猶予（既定 8 秒 = 3 × segment_seconds + 2 秒）で解放されるが、
            ヒントが届かなかった場合は従来どおり live.idle_timeout（既定 30 秒）
            まで伸びる。ここでは長い方を案内する --- 短い方を書くと「待ったのに
            直らない」になる */}
        <p className="text-muted-foreground">
          チャンネルを切り替えた直後は、前のチャンネルの解放待ちの可能性があります。
          30 秒ほど待って再読み込みしてください。
        </p>
        {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
      </div>
    )
  }
  return (
    <div className="text-sm text-destructive">
      <p>ライブ視聴でエラーが発生しました。</p>
      {error.message !== '' && <p className="text-muted-foreground">{error.message}</p>}
    </div>
  )
}
