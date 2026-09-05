import { useEffect, useMemo, useRef, useState } from 'react'

import type { EncodedAsset } from '@/api/generated'
import { formatBytes } from '@/lib/format'
import {
  loadPlaybackPosition,
  loadPlaybackRate,
  playbackRates,
  recordingFileURL,
  recordingSubtitleURL,
  savePlaybackPosition,
  savePlaybackRate,
  shouldSavePlaybackPosition,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'

type RecordingPlayerProps = {
  recordingId: number
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
  const [profile, setProfile] = useState(profiles[0] ?? '')
  // props の資産一覧が更新されて選択中プロファイルが消えた場合は、effect で一度
  // 無効な値を描いてから直すのではなく、表示値をその場で先頭へ導出する。
  const selectedProfile = profiles.includes(profile) ? profile : (profiles[0] ?? '')
  const [playbackRate, setPlaybackRate] = useState(loadPlaybackRate)
  const videoRef = useRef<HTMLVideoElement>(null)
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
  // 「`profile` は変わらず `playbackRate` も既に 1.5 のまま」という場合に依存配列が
  // 前回と同じと判定されて effect が再実行されず、新しい要素の既定値（1 倍）の
  // ままになる --- select の表示は 1.5× でも実際の再生は 1 倍に戻る、という
  // 見た目と実体のずれ（レビュー指摘）。**`defaultPlaybackRate` にも同じ値を
  // 入れる。** `src` を差し替える media element load algorithm は `playbackRate` を
  // `defaultPlaybackRate` へ戻すため、`playbackRate` だけ設定しても再生が始まった
  // 瞬間に 1 倍へ巻き戻りうる。
  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    video.defaultPlaybackRate = playbackRate
    video.playbackRate = playbackRate
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
        </div>
      ) : (
        // 選択肢が 1 つ（= セレクタを出さない）でも、押す前にサイズを見せる
        // という値札の方針（issue #236 M7-3）は変わらないので、常にキャプション
        // として出す。サイズが取れない資産でも選択肢（プロファイル名）自体は
        // 隠さない --- 分類の失敗で機能を隠さないというドロップ統計と同じ判断。
        selectedAsset && (
          <p className="text-muted-foreground">{assetOptionLabel(selectedAsset)}</p>
        )
      )}

      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor={`playback-rate-${recordingId}`} className="text-muted-foreground">
          再生速度
        </label>
        <select
          id={`playback-rate-${recordingId}`}
          value={playbackRate}
          onChange={(event) => {
            const rate = Number(event.target.value)
            setPlaybackRate(rate)
            savePlaybackRate(rate)
          }}
          className="rounded border border-border bg-background px-2 py-1 text-xs"
        >
          {playbackRates.map((rate) => (
            <option key={rate} value={rate}>
              {Number.isInteger(rate) ? rate.toFixed(1) : rate}×
            </option>
          ))}
        </select>
        {document.pictureInPictureEnabled === true && (
          <button
            type="button"
            onClick={() => {
              const video = videoRef.current
              if (!video) return
              void video.requestPictureInPicture()
            }}
            className="rounded border border-border px-2 py-1 text-xs hover:bg-muted"
          >
            ピクチャーインピクチャー
          </button>
        )}
      </div>

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
          if (!restorePending.current) return
          restorePending.current = false
          const pos = loadPlaybackPosition(recordingId, selectedProfile)
          if (pos !== null && pos > 0) {
            e.currentTarget.currentTime = pos
          }
        }}
        onTimeUpdate={(e) => {
          const v = e.currentTarget
          // timeupdate は約 4Hz で発火するが保存値は秒単位なので、秒が変わったときだけ書く
          if (!shouldSavePlaybackPosition(lastSavedSecond.current, v.currentTime)) return
          lastSavedSecond.current = Math.floor(v.currentTime)
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
        }}
        onPause={(e) => {
          const v = e.currentTarget
          savePlaybackPosition(recordingId, selectedProfile, v.currentTime, v.duration)
        }}
      >
        <track
          kind="subtitles"
          srcLang="ja"
          label="日本語"
          src={recordingSubtitleURL(recordingId, selectedProfile)}
        />
      </video>

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
