import { useCallback, useEffect, useRef, useState } from 'react'
import type { RefObject } from 'react'
import { PLAYBACK_POSITION_MINIMUM_MS } from '@/lib/playback-position'

/** OffsetSessionStart は録画 HLS セッションの開始意図を表す。 */
export type OffsetSessionStart =
  | { type: 'offset'; offsetSeconds: number }
  | { type: 'position'; recordingSeconds: number }
  | { type: 'saved-position'; positionMs?: number }
  | { type: 'beginning' }

type OffsetSessionState = {
  identity: string
  offsetSeconds: number
  startRecordingSeconds: number | null
  explicit: boolean
  generation: number
}

type OffsetSessionSeekResult =
  | { type: 'inside'; offsetSeconds: number }
  | { type: 'restarted'; offsetSeconds: number }
  | { type: 'source-changed' }
  | { type: 'unavailable' }

type UseOffsetSessionOptions = {
  active: boolean
  /** 同じ録画内で chase と original-vod を切り替えた場合も別セッションとして扱う。 */
  identity: string
  recordingId?: number
  start: OffsetSessionStart
  videoRef: RefObject<HTMLVideoElement | null>
  /** 録画秒からセッション offset への起点写像。配信元ごとの規則は呼び出し元が渡す。 */
  originSeconds: (offsetSeconds: number) => number
  onLeave: (offsetSeconds: number) => void
  onSourceRangeExit?: (recordingSeconds: number, wasPlaying: boolean) => boolean
  onRecordingPositionChange?: (recordingSeconds: number) => void
}

function validOffset(seconds: number): number {
  return Number.isSafeInteger(seconds) && seconds >= 0 ? seconds : 0
}

function startIdentity(start: OffsetSessionStart): string {
  switch (start.type) {
    case 'offset':
      return `offset:${start.offsetSeconds}`
    case 'position':
      return `position:${start.recordingSeconds}`
    case 'saved-position':
      // 保存値は再生中にもサーバーから更新される。開始時に一度だけ読む値なので、
      // 同じ録画で更新が届いても現在のセッションを巻き戻さない。
      return 'saved-position'
    case 'beginning':
      return 'beginning'
  }
}

function createSessionState(
  identity: string,
  start: OffsetSessionStart,
  originSeconds: (offsetSeconds: number) => number,
): OffsetSessionState {
  switch (start.type) {
    case 'offset': {
      const offsetSeconds = validOffset(start.offsetSeconds)
      return {
        identity,
        offsetSeconds,
        startRecordingSeconds: originSeconds(offsetSeconds),
        explicit: true,
        generation: 0,
      }
    }
    case 'position': {
      const recordingSeconds = Number.isFinite(start.recordingSeconds)
        ? Math.max(0, start.recordingSeconds)
        : 0
      const offsetSeconds = Math.floor(recordingSeconds)
      return {
        identity,
        offsetSeconds,
        startRecordingSeconds: recordingSeconds,
        explicit: true,
        generation: 0,
      }
    }
    case 'saved-position': {
      const recordingSeconds = start.positionMs !== undefined &&
        Number.isFinite(start.positionMs) && start.positionMs >= PLAYBACK_POSITION_MINIMUM_MS
        ? start.positionMs / 1000
        : null
      const offsetSeconds = recordingSeconds === null ? 0 : Math.floor(recordingSeconds)
      return {
        identity,
        offsetSeconds,
        startRecordingSeconds: recordingSeconds,
        explicit: false,
        generation: 0,
      }
    }
    case 'beginning':
      return {
        identity,
        offsetSeconds: 0,
        startRecordingSeconds: 0,
        explicit: true,
        generation: 0,
      }
  }
}

function seekableEndSeconds(video: HTMLVideoElement): number {
  try {
    const ranges = video.seekable
    if (ranges.length > 0) return ranges.end(ranges.length - 1)
    if (Number.isFinite(video.duration)) return video.duration
  } catch {
    if (Number.isFinite(video.duration)) return video.duration
  }
  return 0
}

/** useOffsetSession は追っかけと原本 HLS が共有する開始・張り直し状態を持つ。 */
export function useOffsetSession({
  active,
  identity: sessionIdentity,
  recordingId,
  start,
  videoRef,
  originSeconds,
  onLeave,
  onSourceRangeExit,
  onRecordingPositionChange,
}: UseOffsetSessionOptions) {
  const identity = JSON.stringify([
    active,
    sessionIdentity,
    recordingId ?? null,
    startIdentity(start),
  ])
  const [storedState, setStoredState] = useState(() => createSessionState(identity, start, originSeconds))
  const currentState = storedState.identity === identity
    ? storedState
    : createSessionState(identity, start, originSeconds)
  const stateRef = useRef(currentState)
  const startRef = useRef(start)
  const activeRef = useRef(active)
  const originSecondsRef = useRef(originSeconds)
  const onLeaveRef = useRef(onLeave)
  const onSourceRangeExitRef = useRef(onSourceRangeExit)
  const onRecordingPositionChangeRef = useRef(onRecordingPositionChange)
  const resumePlaybackPendingRef = useRef(false)
  const startReassertPendingRef = useRef(false)

  // 開始意図の変更は、同じ LivePlayer を使い続けるライブ画面でも新しいセッションにする。
  // render 中に条件付きで同期し、effect が走る前の要求にも新しい offset を使う。
  if (storedState.identity !== identity) setStoredState(currentState)
  useEffect(() => {
    stateRef.current = currentState
  }, [currentState])
  useEffect(() => {
    startRef.current = start
    activeRef.current = active
    originSecondsRef.current = originSeconds
    onLeaveRef.current = onLeave
    onSourceRangeExitRef.current = onSourceRangeExit
    onRecordingPositionChangeRef.current = onRecordingPositionChange
  }, [active, onLeave, onRecordingPositionChange, onSourceRangeExit, originSeconds, start])

  const updateState = useCallback((next: OffsetSessionState) => {
    stateRef.current = next
    setStoredState(next)
  }, [])

  const restartAtOffset = useCallback((
    offsetSeconds: number,
    startPositionSeconds = 0,
    options: { leaveSameOffset?: boolean } = {},
  ) => {
    const current = stateRef.current
    const nextOffset = validOffset(offsetSeconds)
    const nextOrigin = originSecondsRef.current(nextOffset)
    const video = videoRef.current
    resumePlaybackPendingRef.current = Boolean(
      (video !== null && !video.paused) || resumePlaybackPendingRef.current,
    )
    startReassertPendingRef.current = false
    // 同じ offset の 416 は state の URL だけでは変化しないため、その場で離脱を知らせて再要求する。
    if (activeRef.current && current.offsetSeconds === nextOffset && options.leaveSameOffset !== false) {
      onLeaveRef.current(nextOffset)
    }
    updateState({
      ...current,
      offsetSeconds: nextOffset,
      startRecordingSeconds: nextOrigin + Math.max(0, startPositionSeconds),
      explicit: true,
      generation: current.generation + 1,
    })
  }, [updateState, videoRef])

  // Explicit starts retry from the beginning of their current session. A
  // saved-position session instead retries from the latest server position,
  // while keeping its original offset and implicit start intent.
  const retry = useCallback(() => {
    const current = stateRef.current
    const latestStart = startRef.current
    const origin = originSecondsRef.current(current.offsetSeconds)
    const savedPositionSeconds = !current.explicit &&
      latestStart.type === 'saved-position' &&
      latestStart.positionMs !== undefined &&
      Number.isFinite(latestStart.positionMs) &&
      latestStart.positionMs >= PLAYBACK_POSITION_MINIMUM_MS
      ? latestStart.positionMs / 1000
      : null
    const video = videoRef.current
    resumePlaybackPendingRef.current = Boolean(
      (video !== null && !video.paused) || resumePlaybackPendingRef.current,
    )
    startReassertPendingRef.current = false
    updateState({
      ...current,
      startRecordingSeconds: savedPositionSeconds === null
        ? null
        : Math.max(origin, savedPositionSeconds),
      generation: current.generation + 1,
    })
  }, [updateState, videoRef])

  const seek = useCallback((recordingSeconds: number): OffsetSessionSeekResult => {
    const video = videoRef.current
    if (!activeRef.current || !video || !Number.isFinite(recordingSeconds)) {
      return { type: 'unavailable' }
    }
    const target = Math.max(0, recordingSeconds)
    const current = stateRef.current
    const origin = originSecondsRef.current(current.offsetSeconds)
    const localTarget = target - origin
    if (target >= origin && localTarget <= seekableEndSeconds(video)) {
      video.currentTime = localTarget
      onRecordingPositionChangeRef.current?.(target)
      return { type: 'inside', offsetSeconds: current.offsetSeconds }
    }

    const wasPlaying = !video.paused || resumePlaybackPendingRef.current
    if (onSourceRangeExitRef.current?.(target, wasPlaying) === true) {
      return { type: 'source-changed' }
    }

    const nextOffset = Math.floor(target)
    const nextOrigin = originSecondsRef.current(nextOffset)
    restartAtOffset(nextOffset, Math.max(0, target - nextOrigin))
    onRecordingPositionChangeRef.current?.(target)
    return { type: 'restarted', offsetSeconds: nextOffset }
  }, [restartAtOffset, videoRef])

  const clearStartPosition = useCallback(() => {
    const current = stateRef.current
    if (current.startRecordingSeconds === null) return
    updateState({ ...current, startRecordingSeconds: null })
  }, [updateState])

  const reportPosition = useCallback((mediaSeconds: number) => {
    if (!Number.isFinite(mediaSeconds)) return
    const current = stateRef.current
    onRecordingPositionChangeRef.current?.(
      originSecondsRef.current(current.offsetSeconds) + Math.max(0, mediaSeconds),
    )
  }, [])
  const getStartPositionSeconds = useCallback(() => {
    const current = stateRef.current
    if (current.startRecordingSeconds === null) return null
    const origin = originSecondsRef.current(current.offsetSeconds)
    return Math.max(0, current.startRecordingSeconds - origin)
  }, [])

  // 再生元の切替・offset の変更・開始意図の変更で古い HLS セッションを離れる。
  // pagehide / hidden でも同じ offset の離脱ヒントを送る。
  useEffect(() => {
    if (!active || recordingId === undefined) return
    const leave = () => onLeaveRef.current(currentState.offsetSeconds)
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
  }, [active, currentState.identity, currentState.offsetSeconds, recordingId])

  const sessionStartSeconds = originSeconds(currentState.offsetSeconds)
  const startPositionSeconds = currentState.startRecordingSeconds === null
    ? null
    : Math.max(0, currentState.startRecordingSeconds - sessionStartSeconds)

  return {
    offsetSeconds: currentState.offsetSeconds,
    sessionStartSeconds,
    startPositionSeconds,
    hasExplicitStart: currentState.explicit,
    sessionKey: `${currentState.identity}:${currentState.generation}`,
    resumePlaybackPendingRef,
    startReassertPendingRef,
    restartAtOffset,
    retry,
    seek,
    clearStartPosition,
    reportPosition,
    getStartPositionSeconds,
  }
}
