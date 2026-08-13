import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query'
import { act, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { epgRefreshIntervalMs, operationalRefreshIntervalMs, useServerEvents } from '@/lib/events'

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

const reservationsKey = ['/api/reservations', { start: 'a' }]
const epgKey = ['/api/sites/tokyo/programs', { start: 'a' }]
/**
 * 番組リスト（pages/programs.tsx の useInfiniteQuery）のキー。URL ではなく手書きなので、
 * `/api/sites/` の接頭辞では引っかからない。
 */
const programListKey = ['/api/programs', 'infinite', 0, 1, undefined]
/**
 * 予約詳細（pages/reservation-detail.tsx）のキー。URL は `/api/sites/...` だが、
 * 先頭要素を一覧と揃えてあるので運用状態グループ（60 秒）に入る。
 */
const reservationDetailKey = ['/api/reservations', 'detail', 'tokyo', 300000]

/** fetchCounts は監視中のクエリが実際に何回 fetch されたかを数える。 */
type FetchCounts = {
  reservations: number
  reservationDetail: number
  epg: number
  programList: number
}

/**
 * ActiveQueries は観測者付きのクエリを 4 本張る。観測者が居ないと invalidate は
 * stale 化するだけで fetch を起こさないので、「再取得が実際に走ったか」を見る
 * テストではこちらを使う。
 */
function ActiveQueries({ counts }: { counts: FetchCounts }) {
  useQuery({
    queryKey: reservationsKey,
    queryFn: () => {
      counts.reservations += 1
      return Promise.resolve([])
    },
  })
  useQuery({
    queryKey: epgKey,
    queryFn: () => {
      counts.epg += 1
      return Promise.resolve([])
    },
  })
  useQuery({
    queryKey: reservationDetailKey,
    queryFn: () => {
      counts.reservationDetail += 1
      return Promise.resolve({})
    },
  })
  useQuery({
    queryKey: programListKey,
    queryFn: () => {
      counts.programList += 1
      return Promise.resolve([])
    },
  })
  return null
}

/**
 * advance は偽タイマーを進め、それによって走り出した fetch と再描画を流し切る。
 *
 * 偽タイマー下では `waitFor` が進行を作れず必ずタイムアウトするので、
 * 「進める」と「待つ」をこの 1 つにまとめる。戻った時点で、その時刻までに
 * 起きるはずの再取得は必ずカウントに反映されている。
 */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

/**
 * renderLevelPaths は SSE 購読と観測者付きクエリを一緒に描き、初回 fetch が
 * 済むまで待ってから返す。以降のカウント増加はすべて回復経路によるもの。
 */
async function renderLevelPaths() {
  globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
  const queryClient = new QueryClient({
    // staleTime: Infinity なので、放置しただけでは再取得は起きない。
    // 増分は必ず invalidate 由来になる
    defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } },
  })
  const counts: FetchCounts = { reservations: 0, reservationDetail: 0, epg: 0, programList: 0 }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Subscriber />
      <ActiveQueries counts={counts} />
    </QueryClientProvider>,
  )
  await advance(0)
  // 初回 fetch を観測してから始める。ここが 0 のままだと、以降の「増えた」が
  // 何も測っていないことになる
  expect(counts).toEqual({ reservations: 1, reservationDetail: 1, epg: 1, programList: 1 })
  return { queryClient, counts, view }
}

afterEach(() => {
  Reflect.deleteProperty(globalThis, 'EventSource')
  EventSourceStub.last = null
  vi.useRealTimers()
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

  it('SSE が来なくても運用状態のクエリは 60 秒周期で取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // SSE のイベントは 1 通も送らない。接続は生きたまま通知だけ落ちた状態を模す
    // 59 秒では動かない（周期より短い時間で通ってしまうテストにしない）
    await advance(59_000)
    expect(counts.reservations).toBe(1)

    await advance(1_000)
    expect(counts.reservations).toBe(2)
    // 周期は定数ではなくリテラルで押さえる（定数を変えても通るテストにしない）
    expect(operationalRefreshIntervalMs).toBe(60_000)
  })

  it('予約詳細は運用状態グループ（60 秒）で取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 予約詳細の URL は /api/sites/{site}/programs/{id}/reservation だが、
    // キーの先頭要素を一覧と揃えてあるので EPG の 10 分ではなく 60 秒側に入る。
    // 所属を決めるのは URL ではなくキーの先頭要素
    await advance(60_000)
    expect(counts.reservationDetail).toBe(2)
    // 同じ時点で EPG 側は動いていない（「どの周期でも増える」では所属を
    // 主張したことにならない）
    expect(counts.epg).toBe(1)
  })

  it('EPG は運用状態より長い周期でしか取り直さない', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 運用状態が 9 回取り直される間、EPG は 1 回も取り直さない
    await advance(9 * 60_000)
    expect(counts.reservations).toBe(10)
    expect(counts.epg).toBe(1)
    expect(counts.programList).toBe(1)

    await advance(60_000)
    expect(counts.epg).toBe(2)
    // 番組リストは URL ではなく手書きのキーなので、接頭辞を 1 つ落とすと
    // ここだけが取り残される（実ブラウザで見つけた取りこぼし）
    expect(counts.programList).toBe(2)
    expect(epgRefreshIntervalMs).toBe(600_000)
  })

  it('epg のイベントで番組リスト（手書きのクエリキー）も取り直す', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    queryClient.setQueryData(programListKey, [])
    queryClient.setQueryData(epgKey, [])
    queryClient.setQueryData(['/api/recordings'], [])
    renderSubscriber(queryClient)

    expect(isStale(queryClient, programListKey)).toBe(false)

    EventSourceStub.last?.emit('epg')

    expect(isStale(queryClient, programListKey)).toBe(true)
    expect(isStale(queryClient, epgKey)).toBe(true)
    expect(isStale(queryClient, ['/api/recordings'])).toBe(false)
  })

  it('背面タブでは定期取得を投げず、前面に戻ると再開する', async () => {
    vi.useFakeTimers()
    const visibility = vi.spyOn(document, 'visibilityState', 'get')
    visibility.mockReturnValue('hidden')
    const { counts } = await renderLevelPaths()

    await advance(3 * 60_000)
    expect(counts.reservations).toBe(1)

    // 「何も起きないまま成功」でないことを、同じ観測方法で反対側を見て確かめる
    visibility.mockReturnValue('visible')
    await advance(60_000)
    expect(counts.reservations).toBe(2)
    visibility.mockRestore()
  })

  it('再接続したら切断中の変更を全グループ取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 初回接続の open では取り直さない（各クエリの mount 時の取得と二重になる）
    EventSourceStub.last?.emit('open')
    await advance(0)
    expect(counts).toEqual({ reservations: 1, reservationDetail: 1, epg: 1, programList: 1 })

    // 切断 → 再接続。切断中に飛んだ通知は再送されないので、周期を待たずに取り直す
    EventSourceStub.last?.emit('error')
    EventSourceStub.last?.emit('open')
    await advance(0)

    expect(counts).toEqual({ reservations: 2, reservationDetail: 2, epg: 2, programList: 2 })
  })
})
