import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { useNow } from '@/lib/use-now'

function ClockReader({
  label,
  intervalMs,
  active = true,
}: {
  label: string
  intervalMs: number
  active?: boolean
}) {
  const nowMs = useNow(intervalMs, active)
  return <output data-testid={label}>{nowMs}</output>
}

afterEach(() => {
  vi.useRealTimers()
})

describe('useNow', () => {
  it('複数の間隔を 1 本のタイマーで配り、各購読者は自分の間隔で更新する', async () => {
    vi.useFakeTimers()
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    vi.setSystemTime(initialNowMs)

    const view = render(
      <>
        <ClockReader label="fast" intervalMs={1000} />
        <ClockReader label="slow" intervalMs={30_000} />
      </>,
    )
    expect(screen.getByTestId('fast')).toHaveTextContent(String(initialNowMs))
    expect(screen.getByTestId('slow')).toHaveTextContent(String(initialNowMs))
    expect(vi.getTimerCount()).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(screen.getByTestId('fast')).toHaveTextContent(String(initialNowMs + 1000))
    expect(screen.getByTestId('slow')).toHaveTextContent(String(initialNowMs))
    expect(vi.getTimerCount()).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(29_000)
    })
    expect(screen.getByTestId('slow')).toHaveTextContent(String(initialNowMs + 30_000))
    expect(vi.getTimerCount()).toBe(1)

    view.rerender(<ClockReader label="slow" intervalMs={30_000} />)
    expect(vi.getTimerCount()).toBe(1)
    view.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('inactive の購読者はタイマーを開始しない', () => {
    vi.useFakeTimers()
    const view = render(<ClockReader label="paused" intervalMs={1000} active={false} />)

    expect(screen.getByTestId('paused')).toBeInTheDocument()
    expect(vi.getTimerCount()).toBe(0)

    view.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})
