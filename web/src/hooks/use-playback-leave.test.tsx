import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { usePlaybackLeave } from '@/hooks/use-playback-leave'

describe('usePlaybackLeave', () => {
  it('URL が変わらない render では送らず、最新の sender で pagehide と cleanup を送る', () => {
    const originalSend = vi.fn()
    const currentSend = vi.fn()
    const view = renderHook(
      ({ url, send }) => usePlaybackLeave(url, send),
      { initialProps: { url: '/api/leave/one', send: originalSend } },
    )

    view.rerender({ url: '/api/leave/one', send: currentSend })
    expect(originalSend).not.toHaveBeenCalled()
    expect(currentSend).not.toHaveBeenCalled()

    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(currentSend).toHaveBeenCalledExactlyOnceWith('/api/leave/one')
    expect(originalSend).not.toHaveBeenCalled()

    view.rerender({ url: '/api/leave/two', send: currentSend })
    expect(currentSend).toHaveBeenCalledTimes(2)
    expect(currentSend).toHaveBeenNthCalledWith(2, '/api/leave/one')

    view.unmount()
    expect(currentSend).toHaveBeenNthCalledWith(3, '/api/leave/two')
    act(() => window.dispatchEvent(new Event('pagehide')))
    expect(currentSend).toHaveBeenCalledTimes(3)
  })

  it('visibilitychange は hidden のときに現在の URL を送る', () => {
    const send = vi.fn()
    const visibility = vi.spyOn(document, 'visibilityState', 'get')
    const { unmount } = renderHook(() => usePlaybackLeave('/api/leave/current', send))

    visibility.mockReturnValue('visible')
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(send).not.toHaveBeenCalled()

    visibility.mockReturnValue('hidden')
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(send).toHaveBeenCalledExactlyOnceWith('/api/leave/current')

    unmount()
    visibility.mockRestore()
  })
})
