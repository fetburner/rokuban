import { useEffect, useRef, useState } from 'react'

import {
  loadPlaybackPosition,
  recordingFileURL,
  savePlaybackPosition,
  shouldSavePlaybackPosition,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'

type RecordingPlayerProps = {
  recordingId: number
  /** 再生可能な encoded プロファイル名（active media_assets）。空ならプレイヤーを出さない。 */
  encodedProfiles: string[]
  /** 原本 TS があるとき VLC 向けリンクを出す。 */
  hasOriginal?: boolean
  className?: string
  /**
   * 変化するたびに `<video>` へスクロール + フォーカスする（値そのものに
   * 意味は無いトークン。録画一覧の「再生」ボタンから展開されたときに
   * インクリメントされる。`pages/recordings.tsx` の `RecordingRow`
   * 参照）。**`.play()` は呼ばない** --- 呼ぶと本編データの取得が
   * 暗黙に始まってしまう（M7 の値札方針。詳細は呼び出し元のコメント）。
   * 実際の再生開始はネイティブ `<video controls>` への利用者の
   * もう一段のクリックに委ねる。
   */
  focusToken?: number
}

/**
 * RecordingPlayer は encoded 派生物をネイティブ video 要素で再生する。
 * MP4 progressive + Range（streamer）。位置は localStorage（サーバー履歴なし）。
 */
export function RecordingPlayer({
  recordingId,
  encodedProfiles,
  hasOriginal = false,
  className,
  focusToken,
}: RecordingPlayerProps) {
  const profiles = encodedProfiles
  const [profile, setProfile] = useState(profiles[0] ?? '')
  const videoRef = useRef<HTMLVideoElement>(null)
  // プロファイル切替時に load したあとだけ currentTime を復元する
  const restorePending = useRef(true)
  // timeupdate 間引き用: 直近に保存した Math.floor(currentTime)。null は未保存
  const lastSavedSecond = useRef<number | null>(null)

  useEffect(() => {
    if (profiles.length === 0) return
    if (!profiles.includes(profile)) {
      setProfile(profiles[0]!)
    }
  }, [profiles, profile])

  useEffect(() => {
    restorePending.current = true
    lastSavedSecond.current = null
  }, [recordingId, profile])

  // `focusToken` が変わるたびに video 要素へスクロール + フォーカスする。
  // `scrollIntoView` は jsdom に実装が無い（web/e2e/README.md 参照。この効果は
  // 実ブラウザでしか測れない領域）ので optional call にして落ちないようにする。
  useEffect(() => {
    if (!focusToken) return
    const v = videoRef.current
    if (!v) return
    v.scrollIntoView?.({ block: 'center', behavior: 'smooth' })
    v.focus()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusToken])

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
              VLC 等で開く
            </a>
            ことができます。
          </p>
        ) : (
          <p>再生可能なファイルがありません。</p>
        )}
      </div>
    )
  }

  const src = recordingFileURL(recordingId, profile)

  return (
    <section className={cn('flex flex-col gap-2', className)} aria-label="再生">
      {profiles.length > 1 && (
        <div className="flex flex-wrap items-center gap-2">
          <label htmlFor={`profile-${recordingId}`} className="text-muted-foreground">
            プロファイル
          </label>
          <select
            id={`profile-${recordingId}`}
            value={profile}
            onChange={(e) => setProfile(e.target.value)}
            className="rounded border border-border bg-background px-2 py-1 text-xs"
          >
            {profiles.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>
      )}

      <video
        key={`${recordingId}:${profile}`}
        ref={videoRef}
        controls
        playsInline
        preload="metadata"
        src={src}
        // tabIndex={-1}: プログラムからの focus() は常に効くようにするが、
        // キーボードの Tab 順には追加しない（native controls 自体が
        // 個々のボタンにフォーカスを持つため、コンテナである <video> を
        // Tab 対象にする必要はない）。`<video controls>` は仕様上
        // フォーカス可能な領域だが、明示的な tabindex を持たない場合の
        // 扱いはブラウザ実装に委ねられている --- 再生ボタンからの
        // フォーカス移動を確実にするため明示する。
        tabIndex={-1}
        className="aspect-video w-full max-w-3xl rounded bg-black"
        onLoadedMetadata={(e) => {
          if (!restorePending.current) return
          restorePending.current = false
          const pos = loadPlaybackPosition(recordingId, profile)
          if (pos !== null && pos > 0) {
            e.currentTarget.currentTime = pos
          }
        }}
        onTimeUpdate={(e) => {
          const v = e.currentTarget
          // timeupdate は約 4Hz で発火するが保存値は秒単位なので、秒が変わったときだけ書く
          if (!shouldSavePlaybackPosition(lastSavedSecond.current, v.currentTime)) return
          lastSavedSecond.current = Math.floor(v.currentTime)
          savePlaybackPosition(recordingId, profile, v.currentTime, v.duration)
        }}
        onPause={(e) => {
          const v = e.currentTarget
          savePlaybackPosition(recordingId, profile, v.currentTime, v.duration)
        }}
      />

      {hasOriginal && (
        <p className="text-muted-foreground">
          原本 TS:{' '}
          <a
            href={recordingFileURL(recordingId)}
            className="text-primary underline-offset-2 hover:underline"
          >
            ダウンロード / VLC
          </a>
        </p>
      )}
    </section>
  )
}
