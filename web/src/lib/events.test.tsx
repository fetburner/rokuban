import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'

import { useServerEvents } from '@/lib/events'

/**
 * EventSourceStub は jsdom に無い EventSource を埋める。最後に作られたインスタンスを
 * 覚えておき、テストからサーバー側のイベントを発火できるようにする。
 */
class EventSourceStub {
  static last: EventSourceStub | null = null
  private listeners = new Map<string, Set<(event: Event) => void>>()
  closed = false
  url: string

  // 引数プロパティ（`constructor(public url)`）は erasableSyntaxOnly で使えない
  constructor(url: string) {
    this.url = url
    EventSourceStub.last = this
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    const set = this.listeners.get(type) ?? new Set()
    set.add(listener)
    this.listeners.set(type, set)
  }

  removeEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.get(type)?.delete(listener)
  }

  close(): void {
    this.closed = true
  }

  emit(type: string): void {
    for (const listener of this.listeners.get(type) ?? []) listener(new Event(type))
  }
}

function Subscriber() {
  useServerEvents()
  return null
}

/** renderSubscriber は SSE の購読だけを持つ最小のツリーを描く。 */
function renderSubscriber(queryClient: QueryClient) {
  return render(
    <QueryClientProvider client={queryClient}>
      <Subscriber />
    </QueryClientProvider>,
  )
}

/**
 * staleKeys は指定した接頭辞のクエリが stale になっているかを返す。
 *
 * invalidate の観測は「再取得が起きたか」ではなく stale 化で見る。データを持つ
 * クエリを観測者無しでキャッシュに置くと、invalidate は fetch を起こさない。
 */
function isStale(queryClient: QueryClient, key: readonly unknown[]): boolean {
  const query = queryClient.getQueryCache().find({ queryKey: key })
  return query?.isStale() ?? false
}

afterEach(() => {
  Reflect.deleteProperty(globalThis, 'EventSource')
  EventSourceStub.last = null
})

describe('useServerEvents', () => {
  it('reservations のイベントで容量超過も取り直す（予約集合からの導出値）', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    // 新鮮なデータとしてキャッシュに置く（staleTime: Infinity なので放置では stale にならない）
    queryClient.setQueryData(['/api/reservations'], [])
    queryClient.setQueryData(['/api/capacity/overages', { start: 'a', end: 'b' }], [])
    queryClient.setQueryData(['/api/recordings'], [])
    renderSubscriber(queryClient)

    expect(isStale(queryClient, ['/api/reservations'])).toBe(false)
    expect(isStale(queryClient, ['/api/capacity/overages', { start: 'a', end: 'b' }])).toBe(false)

    EventSourceStub.last?.emit('reservations')

    expect(isStale(queryClient, ['/api/reservations'])).toBe(true)
    // 容量超過は予約から導出されるので、予約が変わったら一緒に無効化する
    expect(isStale(queryClient, ['/api/capacity/overages', { start: 'a', end: 'b' }])).toBe(true)
    // 無関係なトピックは巻き込まない
    expect(isStale(queryClient, ['/api/recordings'])).toBe(false)
  })

  it('購読を解除すると接続を閉じる', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = renderSubscriber(queryClient)

    const source = EventSourceStub.last
    expect(source?.url).toBe('/api/events')
    expect(source?.closed).toBe(false)
    view.unmount()
    expect(source?.closed).toBe(true)
  })
})
