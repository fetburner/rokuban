import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { usePlaybackLeave } from '@/lib/use-playback-leave'

describe('usePlaybackLeave', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('URL が変わらない render では送らず、pagehide・URL 変更・unmount で現在の URL を送る', () => {
    const beacon = vi.fn(() => true)
    vi.stubGlobal('navigator', { sendBeacon: beacon })
    const view = renderHook(({ url }) => usePlaybackLeave(url), { initialProps: { url: '/api/leave/one' } })

    view.rerender({ url: '/api/leave/one' })
    expect(beacon).not.toHaveBeenCalled()

    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(beacon).toHaveBeenCalledExactlyOnceWith('/api/leave/one')

    view.rerender({ url: '/api/leave/two' })
    expect(beacon).toHaveBeenCalledTimes(2)
    expect(beacon).toHaveBeenNthCalledWith(2, '/api/leave/one')

    view.unmount()
    expect(beacon).toHaveBeenNthCalledWith(3, '/api/leave/two')
    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(beacon).toHaveBeenCalledTimes(3)
  })

  it('visibilitychange は hidden のときに現在の URL を送る', () => {
    const beacon = vi.fn(() => true)
    vi.stubGlobal('navigator', { sendBeacon: beacon })
    const visibility = vi.spyOn(document, 'visibilityState', 'get')
    const { unmount } = renderHook(() => usePlaybackLeave('/api/leave/current'))

    visibility.mockReturnValue('visible')
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(beacon).not.toHaveBeenCalled()

    visibility.mockReturnValue('hidden')
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(beacon).toHaveBeenCalledExactlyOnceWith('/api/leave/current')

    unmount()
    visibility.mockRestore()
  })

  it('URL を持たない再生元では離脱要求を送らない', () => {
    const beacon = vi.fn(() => true)
    vi.stubGlobal('navigator', { sendBeacon: beacon })
    const { unmount } = renderHook(() => usePlaybackLeave(null))

    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('pagehide'))
    })
    unmount()

    expect(beacon).not.toHaveBeenCalled()
  })
})
