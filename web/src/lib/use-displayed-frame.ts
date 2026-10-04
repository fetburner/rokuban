import { useCallback, useEffect, useRef, type RefObject } from 'react'

/**
 * useDisplayedFrameSeconds は最後に表示されたフレームの `mediaTime`（秒）を返す関数を返す。
 *
 * `requestVideoFrameCallback` が使えるときだけ、`enabled` の間だけ追う。`seeking` で捨てる
 * （シーク直後の表示フレームは通知が来るまで分からない）。使えない環境では常に null。
 * 原本 HLS では `mediaTime` はセッション内時刻なので、使う側がセッション起点を足す
 * （現状の呼び出し元は encoded MP4 の `RecordingPlayer` だけで、起点は 0）。
 */
export function useDisplayedFrameSeconds(
  videoRef: RefObject<HTMLVideoElement | null>,
  enabled: boolean,
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
  }, [videoRef, enabled])
  return useCallback(() => mediaTimeRef.current, [])
}
