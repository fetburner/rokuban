import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { ConnectionBanner } from '@/components/connection-banner'
import { disconnectedBannerDelayMs, useServerEvents } from '@/lib/events'
import { EventSourceStub } from '@/test/event-source-stub'

function Subscriber() {
  useServerEvents()
  return null
}

/** renderBanner は SSE の購読と ConnectionBanner を一緒に描く。 */
function renderBanner() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <Subscriber />
      <ConnectionBanner />
    </QueryClientProvider>,
  )
}

/** advance は偽タイマーを進め、それによって走る再レンダーを流し切る。 */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

afterEach(() => {
  Reflect.deleteProperty(globalThis, 'EventSource')
  EventSourceStub.last = null
  vi.useRealTimers()
})

describe('ConnectionBanner', () => {
  it('切断直後は帯を出さない（瞬断で点滅させない）', () => {
    vi.useFakeTimers()
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    renderBanner()

    act(() => {
      EventSourceStub.last?.emit('error')
    })
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('切断が disconnectedBannerDelayMs 続くと帯が出る（それより前では出ない）', async () => {
    vi.useFakeTimers()
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    renderBanner()

    act(() => {
      EventSourceStub.last?.emit('error')
    })
    // 遅延の 1ms 手前ではまだ出ない（境界のズレを検出する）
    await advance(disconnectedBannerDelayMs - 1)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()

    await advance(1)
    expect(screen.getByRole('status')).toHaveTextContent('更新通知が止まっています。再接続中…')
    // 値は定数ではなくリテラルで押さえる（定数を変えても通るテストにしない）
    expect(disconnectedBannerDelayMs).toBe(10_000)
  })

  it('切断が続く間に追加の error が来てもタイマーを再セットしない', async () => {
    vi.useFakeTimers()
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    renderBanner()

    act(() => {
      EventSourceStub.last?.emit('error')
    })
    await advance(6_000)
    // 同じ切断が続いている状態を模す追加の error。もしタイマーを再セット
    // していたら、ここからさらに disconnectedBannerDelayMs 待たないと
    // 出ないはずだが、正しい実装では「最初の error から 10 秒」で出る
    act(() => {
      EventSourceStub.last?.emit('error')
    })
    await advance(4_000)
    expect(screen.getByRole('status')).toBeInTheDocument()
  })

  it('復旧（open）すると帯が消える', async () => {
    vi.useFakeTimers()
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    renderBanner()

    act(() => {
      EventSourceStub.last?.emit('error')
    })
    await advance(disconnectedBannerDelayMs)
    expect(screen.getByRole('status')).toBeInTheDocument()

    act(() => {
      EventSourceStub.last?.emit('open')
    })
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('最終接続時刻（HH:MM 形式）を表示する', async () => {
    vi.useFakeTimers()
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    renderBanner()

    // 一度 open してから切断する --- lastConnectedAt を持たせるため
    act(() => {
      EventSourceStub.last?.emit('open')
    })
    act(() => {
      EventSourceStub.last?.emit('error')
    })
    await advance(disconnectedBannerDelayMs)

    expect(screen.getByRole('status')).toHaveTextContent(/最終接続 \d{1,2}:\d{2}/)
  })
})
