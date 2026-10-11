import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { TunerStatus } from '@/components/tuner-status'

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('TunerStatus', () => {
  it('同じ観測の再取得中でも、時間が閾値を越えたら古い観測を表示する', async () => {
    const initialNowMs = Date.parse('2026-10-01T00:00:00Z')
    vi.useFakeTimers()
    vi.setSystemTime(initialNowMs)
    let tunerFetches = 0
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/sites/default/tuners') {
        tunerFetches += 1
        return Promise.resolve(
          jsonResponse([
            {
              site: 'default',
              index: 0,
              name: 'Tuner 0',
              types: ['GR'],
              isAvailable: true,
              isFault: false,
              observedAt: new Date(initialNowMs).toISOString(),
            },
          ]),
        )
      }
      throw new Error(`unexpected fetch: ${url.pathname}`)
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
    expect(tunerFetches).toBe(0)

    // React Query の invalidate/refetch はデータを変えない。Date.now() だけを閾値の
    // 先へ動かすので、refetch 後に古さが表示されるかをコンポーネント経路で測る。
    vi.setSystemTime(initialNowMs + 60 * 60_000 + 1)
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ['/api/tuners', 'default'] })
    })

    expect(tunerFetches).toBe(1)
    expect(screen.queryByText('観測が止まっています')).not.toBeInTheDocument()

    // setSystemTime と同じデータの再取得だけでは通知されない。時計の tick が
    // useSyncExternalStore のスナップショットを進めた時点で初めて古さが表示される。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(screen.getByTestId('tuner-status')).toHaveTextContent('観測が止まっています')
    expect(tunerFetches).toBe(1)
  })
})
