import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { useDisplayedFrameSeconds } from '@/lib/use-displayed-frame'

type Cb = (now: number, metadata: { mediaTime: number }) => void

/** requestVideoFrameCallback を持つ偽の video。fire で登録済みの通知を 1 回起こす。 */
function fakeVideo(withApi = true) {
  const video = document.createElement('video')
  let cb: Cb | null = null
  if (withApi) {
    Object.assign(video, {
      requestVideoFrameCallback: (c: Cb) => {
        cb = c
        return 1
      },
      cancelVideoFrameCallback: () => {
        cb = null
      },
    })
  }
  return {
    video,
    fire: (mediaTime: number) => {
      const c = cb
      cb = null
      act(() => c?.(0, { mediaTime }))
    },
  }
}

function setup(initial: HTMLVideoElement, enabled = true) {
  const ref = { current: initial }
  const view = renderHook(({ on, k }) => useDisplayedFrameSeconds(ref, on, k), {
    initialProps: { on: enabled, k: 'a' },
  })
  return {
    get: () => view.result.current(),
    rerender: (video: HTMLVideoElement, on = true, k = 'a') => {
      ref.current = video
      view.rerender({ on, k })
    },
  }
}

describe('useDisplayedFrameSeconds', () => {
  it('(a) 通知で mediaTime が入る', () => {
    const f = fakeVideo()
    const h = setup(f.video)
    expect(h.get()).toBeNull()
    f.fire(2.5)
    expect(h.get()).toBe(2.5)
  })

  it('(b) seeking で捨てる', () => {
    const f = fakeVideo()
    const h = setup(f.video)
    f.fire(2.5)
    act(() => {
      f.video.dispatchEvent(new Event('seeking'))
    })
    expect(h.get()).toBeNull()
  })

  it('(c) enabled=false では追わない', () => {
    const f = fakeVideo()
    const h = setup(f.video, false)
    f.fire(2.5)
    expect(h.get()).toBeNull()
  })

  it('(d) API が無ければ null', () => {
    const f = fakeVideo(false)
    const h = setup(f.video)
    expect(h.get()).toBeNull()
  })

  it('(e) 要素の作り直しで null に戻り、新しい要素の通知を拾う', () => {
    const a = fakeVideo()
    const b = fakeVideo()
    const h = setup(a.video)
    a.fire(2.5)
    h.rerender(b.video, true, 'b')
    expect(h.get()).toBeNull()
    b.fire(7)
    expect(h.get()).toBe(7)
    a.fire(9)
    expect(h.get()).toBe(7)
  })
})
