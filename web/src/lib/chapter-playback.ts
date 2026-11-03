import { useCallback, useEffect, useLayoutEffect, useRef, type RefObject } from 'react'

import { PLAY_AROUND_SECONDS, chapterBoundaryMsToSeekSeconds } from '@/lib/chapters'

export type ChapterPlaybackMode = 'around' | 'to' | 'from'

type ChapterPlaybackOptions = {
  videoRef: RefObject<HTMLVideoElement | null>
  seekToSeconds: (seconds: number) => void
  seekToBoundary: (boundaryMs: number) => void
  onClear?: () => void
  onFinish?: () => void
  onStart?: () => void
  onSelectBoundary?: () => void
}

/** useChapterPlayback は境界再生の停止位置・タイマー・境界への戻しを共通管理する。 */
export function useChapterPlayback(options: ChapterPlaybackOptions) {
  const optionsRef = useRef(options)
  useLayoutEffect(() => {
    optionsRef.current = options
  })
  const timerRef = useRef<number | undefined>(undefined)
  const stopAtRef = useRef<number | null>(null)
  const stopBoundaryMsRef = useRef<number | null>(null)

  const clear = useCallback(() => {
    window.clearTimeout(timerRef.current)
    timerRef.current = undefined
    stopAtRef.current = null
    stopBoundaryMsRef.current = null
    optionsRef.current.onClear?.()
  }, [])

  const finishIfActive = useCallback((media: HTMLVideoElement) => {
    if (stopAtRef.current === null) return
    const boundaryMs = stopBoundaryMsRef.current
    clear()
    optionsRef.current.onFinish?.()
    media.pause()
    if (boundaryMs !== null) optionsRef.current.seekToBoundary(boundaryMs)
  }, [clear])

  // 発火したら位置を確かめずに止める。timer は clear と再設定のたびに clearTimeout されるので、
  // 無効になった timer が後から走ることは無い。
  const rescheduleStop = useCallback((media: HTMLVideoElement) => {
    const stopAt = stopAtRef.current
    if (stopAt === null) return
    window.clearTimeout(timerRef.current)
    const remainingSeconds = Math.max(0, stopAt - media.currentTime)
    timerRef.current = window.setTimeout(
      () => finishIfActive(media),
      (remainingSeconds * 1000) / Math.max(media.playbackRate, 0.1) + 2000,
    )
  }, [finishIfActive])

  const playAround = useCallback((boundaryMs: number, mode: ChapterPlaybackMode = 'around') => {
    const media = optionsRef.current.videoRef.current
    if (!media) return
    clear()
    const boundarySeconds = boundaryMs / 1000
    const start = mode === 'from'
      ? chapterBoundaryMsToSeekSeconds(boundaryMs)
      : Math.max(0, boundarySeconds - PLAY_AROUND_SECONDS)
    const stopAt = mode === 'to' ? boundarySeconds : boundarySeconds + PLAY_AROUND_SECONDS

    optionsRef.current.onStart?.()
    stopAtRef.current = stopAt
    stopBoundaryMsRef.current = mode === 'to' ? boundaryMs : null
    optionsRef.current.seekToSeconds(start)
    void media.play().catch(() => {})
    // The timer starts with the request. If HLS never becomes playable, it still releases the
    // automatic-skip suppression; `playing` and playback-rate changes can refine the deadline.
    rescheduleStop(media)
  }, [clear, rescheduleStop])

  const stopIfReached = useCallback((media: HTMLVideoElement) => {
    const stopAt = stopAtRef.current
    if (stopAt !== null && media.currentTime >= stopAt) finishIfActive(media)
  }, [finishIfActive])

  const selectBoundary = useCallback((seconds: number) => {
    const media = optionsRef.current.videoRef.current
    if (!media) return
    clear()
    optionsRef.current.onSelectBoundary?.()
    if (!media.paused) media.pause()
    optionsRef.current.seekToBoundary(Math.round(seconds * 1000))
  }, [clear])

  useEffect(() => () => clear(), [clear])

  return { clear, finishIfActive, playAround, rescheduleStop, selectBoundary, stopIfReached }
}
