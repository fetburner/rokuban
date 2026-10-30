import { useCallback, useEffect, useRef, type RefObject } from 'react'

/**
 * useDisplayedFrameSeconds は最後に表示されたフレームの `mediaTime`（秒）を返す関数を返す。
 *
 * `requestVideoFrameCallback` が使えるときだけ、`enabled` の間だけ追う。`seeking` で捨てる
 * （シーク直後の表示フレームは通知が来るまで分からない）。使えない環境では常に null。
 * `videoKey` は `<video>` 要素が作り直される単位（画質切替など）。変わったら新しい要素へ張り直す。
 * 録画 HLS の `mediaTime` は `currentTime` と同じ録画先頭からの秒なので、そのまま使える。
 */
export function useDisplayedFrameSeconds(
  videoRef: RefObject<HTMLVideoElement | null>,
  enabled: boolean,
  videoKey: unknown,
): () => number | null {
  const mediaTimeRef = useRef<number | null>(null)
  useEffect(() => {
    const video = videoRef.current
    if (!enabled || !video || typeof video.requestVideoFrameCallback !== 'function') return
    let handle = 0
    const onFrame: VideoFrameRequestCallback = (_now, metadata) => {
      mediaTimeRef.current = metadata.mediaTime
      handle = video.requestVideoFrameCallback(onFrame)
    }
    const onSeeking = () => {
      mediaTimeRef.current = null
    }
    handle = video.requestVideoFrameCallback(onFrame)
    video.addEventListener('seeking', onSeeking)
    return () => {
      video.cancelVideoFrameCallback?.(handle)
      video.removeEventListener('seeking', onSeeking)
      mediaTimeRef.current = null
    }
  }, [videoRef, enabled, videoKey])
  return useCallback(() => mediaTimeRef.current, [])
}
