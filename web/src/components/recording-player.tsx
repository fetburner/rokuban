import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react'

import type { EncodedAsset } from '@/api/generated'
import { formatBytes } from '@/lib/format'
import {
  applyPlaybackRate,
  loadPlaybackPosition,
  loadPlaybackRate,
  recordingFileURL,
  recordingSubtitleURL,
  savePlaybackPosition,
  savePlaybackRate,
  shouldSavePlaybackPosition,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'
import {
  SEEK_TILES_DISPLAY_HEIGHT,
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileAt,
  seekTileBackgroundSize,
  seekTilesURL,
} from '@/lib/seek-tiles'

type RecordingPlayerProps = {
  recordingId: number
  /** 追っかけ再生と揃えるVOD側の既定プロファイル。資産に無ければ先頭を使う。 */
  preferredProfile?: string
  /**
   * 再生可能な encoded 派生物（active media_assets）。空ならプレイヤーを出さない。
   * `sizeBytes` が省略された要素も**選択肢そのものは隠さない**（M7-3 の値札
   * 方針: サイズが取れないという分類の失敗で機能を隠さない。ドロップ統計の
   * 「分類できなかった PID」と同じ判断。docs/frontend/recordings.md）。
   */
  encodedAssets: EncodedAsset[]
  /** 原本 TS があるとき VLC 向けリンクを出す。 */
  hasOriginal?: boolean
  /** 原本 TS の実サイズ。`hasOriginal` のときだけ渡され、ダウンロード / VLC リンクに常置する。 */
  originalSizeBytes?: number
  className?: string
}

/**
 * RecordingPlayer は encoded 派生物をネイティブ video 要素で再生する。
 * MP4 progressive + Range（streamer）。位置は localStorage（サーバー履歴なし）。
 */
export function RecordingPlayer({
  recordingId,
  preferredProfile,
  encodedAssets,
  hasOriginal = false,
  originalSizeBytes,
  className,
}: RecordingPlayerProps) {
  // `encodedAssets` の参照が変わらない限り再計算しない --- 素の `.map()` だと
  // 毎レンダーで新しい配列になり、下の useEffect の依存配列がレンダーごとに
  // 変化したと判定されて毎回走ってしまう（中身は冪等で setProfile を呼ばない
  // 限りループにはならないが、無駄な再実行を避ける）。
  const profiles = useMemo(() => encodedAssets.map((a) => a.profile), [encodedAssets])
  const [profile, setProfile] = useState(
    preferredProfile !== undefined && profiles.includes(preferredProfile)
      ? preferredProfile
      : (profiles[0] ?? ''),
  )
  // props の資産一覧が更新されて選択中プロファイルが消えた場合は、effect で一度
  // 無効な値を描いてから直すのではなく、表示値をその場で先頭へ導出する。
  const selectedProfile = profiles.includes(profile) ? profile : (profiles[0] ?? '')
  const [playbackRate, setPlaybackRate] = useState(loadPlaybackRate)
  const videoRef = useRef<HTMLVideoElement>(null)
  // タイルは録画ごとに 1 枚で profile に依存しないので、キーは recordingId だけ。
  const [tilesRequestedFor, setTilesRequestedFor] = useState<number | null>(null)
  const [tilesAvailableFor, setTilesAvailableFor] = useState<number | null>(null)
  // 親は録画を切り替えてもこのコンポーネントを作り直さない（key が無い）。そのため
  // 帯の状態は recordingId と組で持ち、描くときに今の録画のものだけを使う。
  const [tilePreview, setTilePreview] = useState<{
    recordingId: number
    x: number
    y: number
    left: number
    scale: number
  } | null>(null)
  // スクラブ帯の再生済み割合（0..1）。timeupdate / seeked / loadedmetadata で更新する。
  const [played, setPlayed] = useState<{ recordingId: number; fraction: number } | null>(null)
  const playedFraction = played?.recordingId === recordingId ? played.fraction : 0
  const shownPreview =
    tilePreview?.recordingId === recordingId && tilesAvailableFor === recordingId ? tilePreview : null
  // プロファイル切替時に load したあとだけ currentTime を復元する
  const restorePending = useRef(true)
  // timeupdate 間引き用: 直近に保存した Math.floor(currentTime)。null は未保存
  const lastSavedSecond = useRef<number | null>(null)

  useEffect(() => {
    restorePending.current = true
    lastSavedSecond.current = null
  }, [recordingId, selectedProfile])

  // 録画を変えても速度は保つ（以前はここで 1 倍に戻していた）。速度は端末ごとの
  // 好みであって録画ごとの状態ではない（`lib/playback-position.ts`）。
  //
  // **`recordingId` を依存に含める。** `<video>` は `key={`${recordingId}:${profile}`}`
  // なので、別の録画に移ると DOM 要素ごと作り直される。`recordingId` が依存に無いと
  // 「`profile` は変わらず `playbackRate` state も既に 1.5 のまま」という場合に
  // 依存配列が前回と同じと判定されて effect が再実行されず、新しい要素の既定値
  // （1 倍）のままになる、という退行（レビュー指摘）。**`defaultPlaybackRate` にも同じ値を
  // 入れる。** `src` を差し替える media element load algorithm は `playbackRate` を
  // `defaultPlaybackRate` へ戻すため、`playbackRate` だけ設定しても再生が始まった
  // 瞬間に 1 倍へ巻き戻りうる。
  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const appliedRate = applyPlaybackRate(video, playbackRate)
    if (appliedRate !== playbackRate) setPlaybackRate(appliedRate)
  }, [recordingId, selectedProfile, playbackRate])

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

      const key = event.key.toLowerCase()
      const seekBy = (seconds: number) => {
        const target = Math.max(0, video.currentTime + seconds)
        video.currentTime = Number.isFinite(video.duration)
          ? Math.min(video.duration, target)
          : target
      }
      let handled = true
      switch (key) {
        case ' ':
          if (video.paused) void video.play()
          else video.pause()
          break
        case 'arrowleft':
          seekBy(-10)
          break
        case 'arrowright':
          seekBy(10)
          break
        case 'j':
          seekBy(-30)
          break
        case 'l':
          seekBy(30)
          break
        case 'm':
          video.muted = !video.muted
          break
        case 'f':
          void video.requestFullscreen?.()
          break
        default:
          if (/^[0-9]$/.test(key) && Number.isFinite(video.duration)) {
            video.currentTime = (video.duration * Number(key)) / 10
          } else {
            handled = false
          }
      }
      if (handled) event.preventDefault()
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  if (profiles.length === 0) {
    return (
      <div className={cn('text-muted-foreground', className)}>
        {hasOriginal ? (
          <p>
            ブラウザ再生用のエンコードがまだありません。原本は{' '}
            <a
              href={recordingFileURL(recordingId)}
              className="text-primary underline-offset-2 hover:underline"
            >
              VLC 等で開く{originalSizeBytes !== undefined && ` (${formatBytes(originalSizeBytes)})`}
            </a>
            ことができます。
          </p>
        ) : (
          <p>再生可能なファイルがありません。</p>
        )}
      </div>
    )
  }

  const src = recordingFileURL(recordingId, selectedProfile)
  const selectedAsset = encodedAssets.find((a) => a.profile === selectedProfile)
  // EncodedAsset に container 列がないため、プロファイルから拡張子を推測せず、
  // ダウンロード名は提案どおり .mp4 に固定する。保存されるデータ自体には影響しない。
  const downloadFilename = `recording-${recordingId}-${selectedProfile}.mp4`
  const encodedDownloadLink = (
    <a
      href={src}
      download={downloadFilename}
      aria-label="encoded 動画をダウンロード"
      className="text-primary underline-offset-2 hover:underline"
    >
      ダウンロード
    </a>
  )
  const updatePlayedFraction = (video: HTMLVideoElement) => {
    setPlayed({
      recordingId,
      fraction:
        Number.isFinite(video.duration) && video.duration > 0
          ? Math.max(0, Math.min(1, video.currentTime / video.duration))
          : 0,
    })
  }
  // スクラブ帯の上のポインタ位置 → 再生位置（秒）。duration 未確定なら null。
  // **プレビューはネイティブ controls のシークバーに重ねない。** ネイティブの
  // シークバーは位置も幅もブラウザごとに違い（Firefox は再生ボタンと音量の間、
  // Chrome も左右に余白がある）、外から測れないので、重ねると「見えたタイル」と
  // 「クリックで飛ぶ先」がずれる。座標を自分で持つ帯なら両者は同じ式から出る。
  const scrubSeconds = (event: ReactMouseEvent<HTMLDivElement>): number | null => {
    const video = videoRef.current
    if (!video || !Number.isFinite(video.duration) || video.duration <= 0) return null
    const rect = event.currentTarget.getBoundingClientRect()
    if (rect.width <= 0) return null
    const fraction = Math.max(0, Math.min(1, (event.clientX - rect.left) / rect.width))
    return fraction * video.duration
  }
  const handleScrubMove = (event: ReactPointerEvent<HTMLDivElement>) => {
    // プレビューはマウスだけに出す。タッチは pointerleave が来ないので、タップの
    // 後にプレビューが映像を覆ったまま残る。タップは帯のクリック（シーク）だけに効く。
    if (event.pointerType !== 'mouse') {
      setTilePreview(null)
      return
    }
    // タイルは**最初に触れたときだけ**取りに行く。マウント時に先読みすると、
    // 3 時間の録画で 2 MB 程度を、一度もホバーしない利用者にも払わせることになる。
    // 同じキーを再設定しても React は再描画しないので、毎回呼んでよい。
    setTilesRequestedFor(recordingId)

    const seconds = scrubSeconds(event)
    const tile = seconds === null ? null : seekTileAt(seconds)
    if (tile === null || tilesAvailableFor !== recordingId) {
      setTilePreview(null)
      return
    }
    const rect = event.currentTarget.getBoundingClientRect()
    // 帯が 1 枚ぶんより狭い（狭い画面）ときは、はみ出さないよう縮めて出す。
    const scale = Math.min(1, rect.width / SEEK_TILES_DISPLAY_WIDTH)
    const width = SEEK_TILES_DISPLAY_WIDTH * scale
    const left = Math.max(0, Math.min(rect.width - width, event.clientX - rect.left - width / 2))
    setTilePreview({ recordingId, ...tile, left, scale })
  }
  const handleScrubClick = (event: ReactMouseEvent<HTMLDivElement>) => {
    const video = videoRef.current
    const seconds = scrubSeconds(event)
    if (!video || seconds === null) return
    video.currentTime = seconds
    updatePlayedFraction(video)
  }

  return (
    <section className={cn('flex flex-col gap-2', className)} aria-label="再生">
      {profiles.length > 1 ? (
        <div className="flex flex-wrap items-center gap-2">
          <label htmlFor={`profile-${recordingId}`} className="text-muted-foreground">
            プロファイル
          </label>
          <select
            id={`profile-${recordingId}`}
            value={selectedProfile}
            onChange={(e) => setProfile(e.target.value)}
            className="rounded border border-border bg-background px-2 py-1 text-xs"
          >
            {encodedAssets.map((a) => (
              <option key={a.profile} value={a.profile}>
                {assetOptionLabel(a)}
              </option>
            ))}
          </select>
          {encodedDownloadLink}
        </div>
      ) : (
        // 選択肢が 1 つ（= セレクタを出さない）でも、押す前にサイズを見せる
        // という値札の方針（issue #236 M7-3）は変わらないので、常にキャプション
        // として出す。サイズが取れない資産でも選択肢（プロファイル名）自体は
        // 隠さない --- 分類の失敗で機能を隠さないというドロップ統計と同じ判断。
        <div className="flex flex-wrap items-center gap-2">
          {selectedAsset && (
            <p className="text-muted-foreground">{assetOptionLabel(selectedAsset)}</p>
          )}
          {encodedDownloadLink}
        </div>
      )}

      <div className="flex max-w-3xl flex-col gap-1">
        <video
          ref={videoRef}
          key={`${recordingId}:${selectedProfile}`}
          controls
          playsInline
          preload="metadata"
          src={src}
        // tabIndex は明示しない。実 Chromium で測ったところ `<video controls>` は
        // tabindex 無しでもそれ自体が唯一の Tab stop になっており（native controls
        // の個々のボタンは Tab stop ではない）、`tabIndex={-1}` を付けると逆に
        // Tab 順から完全に外れてキーボード到達性を落とす退行になった（実測: 対照
        // ページで `<button> <video controls> <video controls tabindex=-1> <button>`
        // の Tab 順が `VIDEO(tabindex無し) -> 次のbutton`。展開後に Tab を押しても
        // 一度も VIDEO に止まらないことを確認した）。`.focus()` はこの属性が無くても
        // 実 Chromium では効く（同じく実測）。以前ここに書いていた
        // 「個々のボタンにフォーカスを持つため tabindex は不要」という理屈は
        // 測らずに書いた誤りだった（CLAUDE.md「測っていない挙動を断言しない」）。
        className="aspect-video w-full max-w-3xl rounded bg-black"
        onLoadedMetadata={(e) => {
          updatePlayedFraction(e.currentTarget)
          if (!restorePending.current) return
          restorePending.current = false
          const pos = loadPlaybackPosition(recordingId, selectedProfile)
          if (pos !== null && pos > 0) {
            e.currentTarget.currentTime = pos
          }
        }}
        onSeeked={(e) => updatePlayedFraction(e.currentTarget)}
        onTimeUpdate={(e) => {
          const v = e.currentTarget
          updatePlayedFraction(v)
          // timeupdate は約 4Hz で発火するが保存値は秒単位なので、秒が変わったときだけ書く
          if (!shouldSavePlaybackPosition(lastSavedSecond.current, v.currentTime)) return
          lastSavedSecond.current = Math.floor(v.currentTime)
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
        }}
        onPause={(e) => {
          const v = e.currentTarget
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
        }}
        onRateChange={(e) => {
          const rate = e.currentTarget.playbackRate
          setPlaybackRate(rate)
          savePlaybackRate(rate)
        }}
      >
        <track
          kind="subtitles"
          srcLang="ja"
          label="日本語"
          src={recordingSubtitleURL(recordingId, selectedProfile)}
        />
        </video>

        {/*
          シークプレビュー用のスクラブ帯。ポインタ操作の補助なので aria-hidden にする
          （キーボード・支援技術の経路はネイティブ controls のシークバーが持つ）。
        */}
        <div
          aria-hidden="true"
          data-testid="seek-scrub"
          className="relative h-4 cursor-pointer"
          onPointerMove={handleScrubMove}
          onPointerLeave={() => setTilePreview(null)}
          onClick={handleScrubClick}
        >
          <div className="absolute inset-x-0 top-1/2 h-1.5 -translate-y-1/2 overflow-hidden rounded-full bg-muted">
            <div className="h-full bg-primary" style={{ width: `${playedFraction * 100}%` }} />
          </div>
          {tilesRequestedFor === recordingId && (
            <img
              src={seekTilesURL(recordingId)}
              alt=""
              className="pointer-events-none absolute size-px opacity-0"
              onLoad={() => setTilesAvailableFor(recordingId)}
              onError={() => {
                setTilesAvailableFor((current) => (current === recordingId ? null : current))
                setTilePreview(null)
              }}
            />
          )}
          {shownPreview && (
            <div
              data-testid="seek-tile-preview"
              className="pointer-events-none absolute bottom-full z-10 mb-1 origin-bottom-left overflow-hidden rounded border border-border bg-black shadow-lg"
              style={{
                left: shownPreview.left,
                width: SEEK_TILES_DISPLAY_WIDTH,
                height: SEEK_TILES_DISPLAY_HEIGHT,
                transform: shownPreview.scale < 1 ? `scale(${shownPreview.scale})` : undefined,
              }}
            >
              <div
                className="h-full w-full bg-no-repeat"
                style={{
                  backgroundImage: `url(${seekTilesURL(recordingId)})`,
                  backgroundPosition: `${shownPreview.x}px ${shownPreview.y}px`,
                  backgroundSize: seekTileBackgroundSize(),
                }}
              />
            </div>
          )}
        </div>
      </div>

      {hasOriginal && (
        <p className="text-muted-foreground">
          原本 TS:{' '}
          <a
            href={recordingFileURL(recordingId)}
            className="text-primary underline-offset-2 hover:underline"
          >
            ダウンロード / VLC
            {originalSizeBytes !== undefined && ` (${formatBytes(originalSizeBytes)})`}
          </a>
        </p>
      )}
    </section>
  )
}

/**
 * assetOptionLabel はプロファイルセレクタの選択肢・単一プロファイル時の
 * キャプションに共通で使う表示文字列。`sizeBytes` が省略された資産でも
 * プロファイル名だけは出す（値札方針: サイズが取れないことを理由に選択肢
 * そのものを隠さない）。
 */
function assetOptionLabel(asset: EncodedAsset): string {
  return asset.sizeBytes === undefined ? asset.profile : `${asset.profile} (${formatBytes(asset.sizeBytes)})`
}
