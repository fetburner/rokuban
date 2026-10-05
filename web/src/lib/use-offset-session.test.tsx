import type { RefObject } from 'react'
import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { originalVODSessionOriginSeconds } from '@/lib/live'
import { useOffsetSession } from '@/lib/use-offset-session'

type Options = Parameters<typeof useOffsetSession>[0]

function fakeVideo(paused = true, seekableEnd = 10) {
  const video = document.createElement('video')
  Object.defineProperty(video, 'paused', { value: paused, configurable: true })
  Object.defineProperty(video, 'duration', { value: seekableEnd, configurable: true })
  Object.defineProperty(video, 'seekable', {
    value: { length: 1, start: () => 0, end: () => seekableEnd },
    configurable: true,
  })
  return video
}

function options(start: Options['start'], video = fakeVideo(), overrides: Partial<Options> = {}): Options {
  return {
    active: true,
    identity: 'chase',
    recordingId: 42,
    start,
    videoRef: { current: video } as RefObject<HTMLVideoElement | null>,
    originSeconds: (offsetSeconds) => offsetSeconds,
    onLeave: vi.fn(),
    ...overrides,
  }
}

describe('useOffsetSession', () => {
  it('セッション範囲の境界を含めて currentTime を変え、位置を報告する', () => {
    const video = fakeVideo(true, 10)
    const onRecordingPositionChange = vi.fn()
    const initial = options(
      { type: 'offset', offsetSeconds: 5 },
      video,
      { onRecordingPositionChange },
    )
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    let seekResult: ReturnType<typeof result.current.seek>
    act(() => {
      seekResult = result.current.seek(15)
    })

    expect(seekResult!).toEqual({ type: 'inside', offsetSeconds: 5 })
    expect(video.currentTime).toBe(10)
    expect(onRecordingPositionChange).toHaveBeenCalledWith(15)
    expect(initial.onLeave).not.toHaveBeenCalled()
  })

  it('セッション起点と同じ録画秒は範囲内として currentTime 0 に移る', () => {
    const video = fakeVideo(true, 10)
    const initial = options({ type: 'offset', offsetSeconds: 5 }, video)
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    let seekResult: ReturnType<typeof result.current.seek>
    act(() => {
      seekResult = result.current.seek(5)
    })

    expect(seekResult!).toEqual({ type: 'inside', offsetSeconds: 5 })
    expect(video.currentTime).toBe(0)
    expect(initial.onLeave).not.toHaveBeenCalled()
  })

  it('セッション範囲外は録画秒の offset へ張り直し、再生意図を引き継ぐ', () => {
    const video = fakeVideo(false, 10)
    const onLeave = vi.fn()
    const onRecordingPositionChange = vi.fn()
    const initial = options(
      { type: 'offset', offsetSeconds: 5 },
      video,
      { onLeave, onRecordingPositionChange },
    )
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    let seekResult: ReturnType<typeof result.current.seek>
    act(() => {
      seekResult = result.current.seek(20.75)
    })

    expect(seekResult!).toEqual({ type: 'restarted', offsetSeconds: 20 })
    expect(result.current.offsetSeconds).toBe(20)
    expect(result.current.sessionStartSeconds).toBe(20)
    expect(result.current.startPositionSeconds).toBeCloseTo(0.75)
    expect(result.current.isResumePlaybackPending()).toBe(true)
    expect(onRecordingPositionChange).toHaveBeenCalledWith(20.75)
    expect(onLeave).toHaveBeenCalledWith(5)
  })

  it('保存位置を秒に切り下げた offset で始め、端数をセッション起点から渡す', () => {
    const initial = options(
      { type: 'saved-position', positionMs: 23_500 },
      fakeVideo(),
      { identity: 'original-vod', originSeconds: originalVODSessionOriginSeconds },
    )
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    expect(result.current.offsetSeconds).toBe(23)
    expect(result.current.sessionStartSeconds).toBeCloseTo(22.98963, 5)
    expect(result.current.startPositionSeconds).toBeCloseTo(0.51037, 4)
    expect(result.current.hasExplicitStart).toBe(false)
  })

  it('416 後の呼び出し元が選んだ後退 offset で再試行し、再生を続ける', () => {
    const video = fakeVideo(false, 10)
    const onLeave = vi.fn()
    const initial = options(
      { type: 'offset', offsetSeconds: 62 },
      video,
      { identity: 'original-vod', originSeconds: originalVODSessionOriginSeconds, onLeave },
    )
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })
    const previousSession = result.current.sessionKey

    act(() => {
      result.current.restartAtOffset(55)
    })

    expect(result.current.offsetSeconds).toBe(55)
    expect(result.current.startPositionSeconds).toBe(0)
    expect(result.current.sessionKey).not.toBe(previousSession)
    expect(result.current.isResumePlaybackPending()).toBe(true)
    expect(onLeave).toHaveBeenCalledWith(62)
  })

  it('開始意図の変更は同じ hook instance でも現在 offset を置き換える', () => {
    const initial = options({ type: 'offset', offsetSeconds: 5 })
    const { result, rerender } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })
    act(() => {
      result.current.restartAtOffset(20, 0.5)
    })
    expect(result.current.offsetSeconds).toBe(20)

    rerender({ ...initial, start: { type: 'position', recordingSeconds: 3.25 } })

    expect(result.current.offsetSeconds).toBe(3)
    expect(result.current.startPositionSeconds).toBeCloseTo(0.25)
    expect(result.current.hasExplicitStart).toBe(true)
  })

  it('同じ録画の保存位置更新は開始済みセッションを巻き戻さない', () => {
    const initial = options({ type: 'saved-position', positionMs: 23_500 })
    const { result, rerender } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    rerender({ ...initial, start: { type: 'saved-position', positionMs: 31_000 } })

    expect(result.current.offsetSeconds).toBe(23)
    expect(result.current.startPositionSeconds).toBeCloseTo(0.5)
  })

  it('保存位置からの retry は最新値を同じ offset 起点で再開し、保存位置の意図を保つ', () => {
    const initial = options(
      { type: 'saved-position', positionMs: 23_500 },
      fakeVideo(),
      { identity: 'original-vod', originSeconds: originalVODSessionOriginSeconds },
    )
    const { result, rerender } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })
    const origin = originalVODSessionOriginSeconds(23)

    act(() => result.current.clearStartPosition())
    rerender({ ...initial, start: { type: 'saved-position', positionMs: 31_500 } })
    act(() => result.current.retry())

    expect(result.current.offsetSeconds).toBe(23)
    expect(result.current.startPositionSeconds).toBeCloseTo(31.5 - origin, 5)
    expect(result.current.hasExplicitStart).toBe(false)

    act(() => result.current.clearStartPosition())
    rerender({ ...initial, start: { type: 'saved-position', positionMs: 32_500 } })
    act(() => result.current.retry())

    expect(result.current.offsetSeconds).toBe(23)
    expect(result.current.startPositionSeconds).toBeCloseTo(32.5 - origin, 5)
    expect(result.current.hasExplicitStart).toBe(false)
  })

  it('明示開始からの retry は開始位置を未指定に戻し、呼び出し元の先頭位置を使う', () => {
    const initial = options({ type: 'offset', offsetSeconds: 5 })
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })
    act(() => result.current.retry())

    expect(result.current.offsetSeconds).toBe(5)
    expect(result.current.startPositionSeconds).toBeNull()
    expect(result.current.hasExplicitStart).toBe(true)
  })

  it('親が再生元を切り替える範囲外 seek は内部で張り直さない', () => {
    const video = fakeVideo(false, 10)
    const onSourceRangeExit = vi.fn(() => true)
    const initial = options(
      { type: 'offset', offsetSeconds: 5 },
      video,
      { onSourceRangeExit },
    )
    const { result } = renderHook((value: Options) => useOffsetSession(value), { initialProps: initial })

    let seekResult: ReturnType<typeof result.current.seek>
    act(() => {
      seekResult = result.current.seek(20)
    })

    expect(seekResult!).toEqual({ type: 'source-changed' })
    expect(onSourceRangeExit).toHaveBeenCalledWith(20, true)
    expect(result.current.offsetSeconds).toBe(5)
  })
})
