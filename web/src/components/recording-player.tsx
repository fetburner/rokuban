import { useEffect, useRef, useState } from 'react'

import {
  loadPlaybackPosition,
  recordingFileURL,
  savePlaybackPosition,
} from '@/lib/playback-position'
import { cn } from '@/lib/utils'

type RecordingPlayerProps = {
  recordingId: number
  /** 再生可能な encoded プロファイル名（active media_assets）。空ならプレイヤーを出さない。 */
  encodedProfiles: string[]
  /** 原本 TS があるとき VLC 向けリンクを出す。 */
  hasOriginal?: boolean
  className?: string
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
}: RecordingPlayerProps) {
  const profiles = encodedProfiles
  const [profile, setProfile] = useState(profiles[0] ?? '')
  const videoRef = useRef<HTMLVideoElement>(null)
  // プロファイル切替時に load したあとだけ currentTime を復元する
  const restorePending = useRef(true)

  useEffect(() => {
    if (profiles.length === 0) return
    if (!profiles.includes(profile)) {
      setProfile(profiles[0]!)
    }
  }, [profiles, profile])

  useEffect(() => {
    restorePending.current = true
  }, [recordingId, profile])

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
