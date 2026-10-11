import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { useNow, type WakeAt } from '@/lib/use-now'

function ClockReader({
  label,
  intervalMs,
  active = true,
  wakeAtMs,
}: {
  label: string
  intervalMs?: number
  active?: boolean
  wakeAtMs?: WakeAt
}) {
  const nowMs = useNow({ intervalMs, active, wakeAtMs })
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

  it('購読者が 0 の間に時間が経っても、再購読で新しい時刻へ直る', async () => {
    vi.useFakeTimers()
    const startMs = Date.parse('2026-10-01T00:00:00Z')
    vi.setSystemTime(startMs)
    render(<ClockReader label="a" intervalMs={1000} />).unmount()

    vi.setSystemTime(startMs + 60 * 60_000)
    render(<ClockReader label="b" intervalMs={1000} />)
    await act(async () => {})
    expect(screen.getByTestId('b')).toHaveTextContent(String(startMs + 60 * 60_000))
  })

  it('壁時計が戻っても、戻った時刻へ追従して更新し続ける', async () => {
    vi.useFakeTimers()
    const startMs = Date.parse('2026-10-01T12:00:00Z')
    vi.setSystemTime(startMs)
    render(<ClockReader label="a" intervalMs={1000} />)

    vi.setSystemTime(startMs - 60 * 60_000)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(screen.getByTestId('a')).toHaveTextContent(String(startMs - 60 * 60_000 + 1000))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(screen.getByTestId('a')).toHaveTextContent(String(startMs - 60 * 60_000 + 2000))
  })

  it('指定時刻にも共有時計を更新する', async () => {
    vi.useFakeTimers()
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    const wakeAtMs = initialNowMs + 2_500
    vi.setSystemTime(initialNowMs)

    render(<ClockReader label="scheduled" intervalMs={1000} wakeAtMs={wakeAtMs} />)
    expect(screen.getByTestId('scheduled')).toHaveTextContent(String(initialNowMs))
    expect(vi.getTimerCount()).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_500)
    })
    expect(screen.getByTestId('scheduled')).toHaveTextContent(String(wakeAtMs))
    expect(vi.getTimerCount()).toBe(1)
  })

  it('境界関数は一度だけ起きて次の境界を共有タイマーへ登録する', async () => {
    vi.useFakeTimers()
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    vi.setSystemTime(initialNowMs)
    const nextWakeAt = (nowMs: number) => nowMs + 2_500

    render(<ClockReader label="boundary" wakeAtMs={nextWakeAt} />)
    expect(screen.getByTestId('boundary')).toHaveTextContent(String(initialNowMs))
    expect(vi.getTimerCount()).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_499)
    })
    expect(screen.getByTestId('boundary')).toHaveTextContent(String(initialNowMs))

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(screen.getByTestId('boundary')).toHaveTextContent(String(initialNowMs + 2_500))
    expect(vi.getTimerCount()).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_500)
    })
    expect(screen.getByTestId('boundary')).toHaveTextContent(String(initialNowMs + 5_000))
    expect(vi.getTimerCount()).toBe(1)
  })

  it('間隔なしの一度きりの予約は起床後に共有タイマーを止める', async () => {
    vi.useFakeTimers()
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    vi.setSystemTime(initialNowMs)

    render(<ClockReader label="one-shot" wakeAtMs={initialNowMs + 2_500} />)
    expect(vi.getTimerCount()).toBe(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_500)
    })

    expect(screen.getByTestId('one-shot')).toHaveTextContent(String(initialNowMs + 2_500))
    expect(vi.getTimerCount()).toBe(0)
  })
})
