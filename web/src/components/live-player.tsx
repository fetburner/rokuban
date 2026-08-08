import { useEffect, useRef, useState } from 'react'

import type { LiveLoadError } from '@/lib/live'
import {
  claimsHlsPlaylistSupport,
  livePlaylistURL,
  probeLivePlaylist,
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
  serviceId: number
  className?: string
}

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
 * 要求が止まるが、streamer 側のチューナー開放は `live.idle_timeout` 経過まで遅延する
 * （明示的にセッションを閉じる API が無いため。issue #92 の着手時コメント参照）。
 */
export function LivePlayer({ site, serviceId, className }: LivePlayerProps) {
  const videoRef = useRef<HTMLVideoElement>(null)
  const hlsRef = useRef<HlsLike | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<LiveLoadError | null>(null)
  // retryNonce を変えると effect が再実行される（依存配列に入れる）
  const [retryNonce, setRetryNonce] = useState(0)

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

    const url = livePlaylistURL(site, serviceId)

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
        video.src = url
      } else {
        const { default: Hls } = await import('hls.js')
        if (cancelled) return
        if (!Hls.isSupported()) {
          // MSE も ManagedMediaSource も無い（iOS 17.1 未満の iPhone Safari が
          // これに当たる）。hls.js では原理的に再生できないので、`<video>` 自身が
          // m3u8 に支持を表明しているならそちらへ渡す --- 駄目でも `<video>` の
          // error イベントに落ちるだけで、失うものは無い。ここを「非対応」と
          // 断じると、ネイティブなら完璧に再生できる端末を締め出す
          if (claimsHlsPlaylistSupport(canPlayType)) {
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
      hlsRef.current?.destroy()
      hlsRef.current = null
      if (video) {
        video.removeAttribute('src')
        video.load()
      }
    }
  }, [site, serviceId, retryNonce])

  return (
    <div className={cn('relative aspect-video w-full max-w-3xl rounded bg-black', className)}>
      <video
        ref={videoRef}
        controls
        playsInline
        className={cn('size-full rounded', (loading || error) && 'invisible')}
      />

      {loading && !error && (
        <div className="absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
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
        録画サーバーの自宅側（streamer）に接続できません。自宅サーバーが起動しているか、
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
            idle GC（既定 30 秒）待ちである可能性が高い */}
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
