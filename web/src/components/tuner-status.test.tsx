import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { TunerStatus } from '@/components/tuner-status'

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('TunerStatus', () => {
  it('データが変わらなくても、時間が閾値を越えたら古い観測を表示する', async () => {
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    vi.useFakeTimers()
    vi.setSystemTime(initialNowMs)
    globalThis.fetch = vi.fn(() => {
      throw new Error('unexpected fetch')
    }) as unknown as typeof fetch

    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    const tuners = [
      {
        site: 'default',
        index: 0,
        name: 'Tuner 0',
        types: ['GR'],
        isAvailable: true,
        isFault: false,
        observedAt: new Date(initialNowMs).toISOString(),
      },
    ]
    queryClient.setQueryData(['/api/sites'], { data: ['default'], status: 200 })
    queryClient.setQueryData(['/api/tuners', 'default'], { data: tuners, status: 200 })
    render(
      <QueryClientProvider client={queryClient}>
        <TunerStatus />
      </QueryClientProvider>,
    )

    expect(screen.getByTestId('tuner-status')).toHaveTextContent('チューナー1本')
    expect(screen.queryByText('観測が止まっています')).not.toBeInTheDocument()

    vi.setSystemTime(initialNowMs + 60 * 60_000 + 1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(screen.getByTestId('tuner-status')).toHaveTextContent('観測が止まっています')
  })
})
