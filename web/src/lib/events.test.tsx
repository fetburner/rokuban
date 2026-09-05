import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query'
import { act, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { getGetStorageQueryKey } from '@/api/generated'
import {
  epgRefreshIntervalMs,
  operationalRefreshIntervalMs,
  storageRefreshIntervalMs,
  useConnectionState,
  useEncodeProgress,
  useServerEvents,
} from '@/lib/events'
import { EventSourceStub } from '@/test/event-source-stub'

function Subscriber() {
  useServerEvents()
  return null
}

/** ConnectionStatusProbe は useConnectionState の戻り値を DOM に出すだけの観測用コンポーネント。 */
function ConnectionStatusProbe() {
  const { status, lastConnectedAt } = useConnectionState()
  return <div data-testid="connection-status">{`${status}:${lastConnectedAt ?? 'null'}`}</div>
}

const progressProfiles = {
  mobile: ['mobile'],
  desktop: ['desktop'],
} as const

function ProgressProbe({
  recordingId,
  profile = 'mobile',
}: {
  recordingId: number
  profile?: keyof typeof progressProfiles
}) {
  const progress = useEncodeProgress(recordingId, progressProfiles[profile])
  return <div data-testid={`${recordingId}-${profile}`}>{progress.get(profile) ?? 'unknown'}</div>
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
/**
 * 録画一覧のキー。予約と同じ運用状態グループ（60 秒）に属するが、グループの
 * 定義は別行なので予約のカウントでは「録画も 60 秒で取り直す」を主張できない
 * （`docs/frontend/recordings.md` のエンコード状態の収束がこれに依っている）。
 */
const recordingsKey = ['/api/recordings', { limit: 50 }]
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
/**
 * ストレージ残高（`components/storage-balance.tsx`）のキー。手書きではなく
 * 生成キーを import する --- 手書きだと画面が実際に使っているキーとの
 * ずれを検出できない（`web/e2e/sse-refresh.mjs` の「実ブラウザ」参照）。
 */
const storageKey = getGetStorageQueryKey()
/**
 * チューナー状態（`components/tuner-status.tsx`）のキー。URL
 * （`/api/sites/{site}/tuners`）ではなく手書きなので、epg の接頭辞
 * （`/api/sites/`）には引っかからず、tuners グループにだけ入る。
 */
const tunersKey = ['/api/tuners', 'tokyo']

/** fetchCounts は監視中のクエリが実際に何回 fetch されたかを数える。 */
type FetchCounts = {
  reservations: number
  recordings: number
  reservationDetail: number
  epg: number
  programList: number
  storage: number
  tuners: number
}

/**
 * ActiveQueries は観測者付きのクエリを 6 本張る。観測者が居ないと invalidate は
 * stale 化するだけで fetch を起こさないので、「再取得が実際に走ったか」を見る
 * テストではこちらを使う。
 */
// oxlint-disable react/immutability -- テストハーネスが所有する観測用カウンタ
function ActiveQueries({ counts }: { counts: FetchCounts }) {
  useQuery({
    queryKey: reservationsKey,
    queryFn: () => {
      counts.reservations += 1
      return Promise.resolve([])
    },
  })
  useQuery({
    queryKey: recordingsKey,
    queryFn: () => {
      counts.recordings += 1
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
  useQuery({
    queryKey: storageKey,
    queryFn: () => {
      counts.storage += 1
      return Promise.resolve([])
    },
  })
  useQuery({
    queryKey: tunersKey,
    queryFn: () => {
      counts.tuners += 1
      return Promise.resolve([])
    },
  })
  return null
}
// oxlint-enable react/immutability

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
  const counts: FetchCounts = {
    reservations: 0,
    recordings: 0,
    reservationDetail: 0,
    epg: 0,
    programList: 0,
    storage: 0,
    tuners: 0,
  }
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Subscriber />
      <ActiveQueries counts={counts} />
    </QueryClientProvider>,
  )
  await advance(0)
  // 初回 fetch を観測してから始める。ここが 0 のままだと、以降の「増えた」が
  // 何も測っていないことになる
  expect(counts).toEqual({
    reservations: 1,
    recordings: 1,
    reservationDetail: 1,
    epg: 1,
    programList: 1,
    storage: 1,
    tuners: 1,
  })
  return { queryClient, counts, view }
}

afterEach(() => {
  Reflect.deleteProperty(globalThis, 'EventSource')
  EventSourceStub.last = null
  vi.useRealTimers()
})

describe('useServerEvents', () => {
  it('encode-progress の payload を録画とプロファイル別の揮発値として受け取る', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Subscriber />
        <ProgressProbe recordingId={42} />
      </QueryClientProvider>,
    )

    expect(view.getByText('unknown')).toBeInTheDocument()
    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 42,
          profile: 'mobile',
          progress: 0.25,
        }),
      )
    })
    expect(view.getByText('0.25')).toBeInTheDocument()
  })

  it('録画 ID と profile が異なる進捗を混ぜない', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Subscriber />
        <ProgressProbe recordingId={42} />
        <ProgressProbe recordingId={42} profile="desktop" />
        <ProgressProbe recordingId={43} />
      </QueryClientProvider>,
    )

    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 42,
          profile: 'mobile',
          progress: 0.25,
        }),
      )
    })
    expect(view.getByTestId('42-mobile')).toHaveTextContent('0.25')
    expect(view.getByTestId('42-desktop')).toHaveTextContent('unknown')
    expect(view.getByTestId('43-mobile')).toHaveTextContent('unknown')
  })

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

  it('recordings のイベントでエンコード待機列も取り直す', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    const key = ['/api/encode-queue']
    queryClient.setQueryData(key, { queued: 2, running: 1 })
    renderSubscriber(queryClient)

    expect(isStale(queryClient, key)).toBe(false)

    EventSourceStub.last?.emit('recordings')

    expect(isStale(queryClient, key)).toBe(true)
  })

  // トピック名と代表キーはリテラルで書く（実装の queryGroups を import すると
  // 「トピックの書き忘れ・書き間違い」を一緒に見逃す）。撃ったトピックのキーだけが
  // stale になることを見るので、トピックの取り違えも検出できる
  const topicKeys = {
    reservations: ['/api/reservations', { start: 'a' }],
    recordings: ['/api/recordings', { start: 'a' }],
    breakers: ['/api/breakers'],
    epg: ['/api/sites/tokyo/programs', { start: 'a' }],
  } as const
  const topics = Object.keys(topicKeys) as (keyof typeof topicKeys)[]

  it.each(topics)('%s のトピックは購読されていて、そのグループだけを無効化する', (topic) => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    for (const key of Object.values(topicKeys)) queryClient.setQueryData(key, [])
    renderSubscriber(queryClient)
    for (const key of Object.values(topicKeys)) expect(isStale(queryClient, key)).toBe(false)

    EventSourceStub.last?.emit(topic)

    for (const other of topics) {
      expect(isStale(queryClient, topicKeys[other])).toBe(other === topic)
    }
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
    expect(counts.recordings).toBe(1)

    await advance(1_000)
    expect(counts.reservations).toBe(2)
    // 録画一覧も同じ 60 秒で取り直す。エンコード状態（encodeStatus）は専用の
    // SSE トピックも NOTIFY も持たず、この周期だけが収束経路なので、予約とは
    // 別に数える（docs/frontend/recordings.md）
    expect(counts.recordings).toBe(2)
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

  it('SSE が来なくてもストレージ残高は専用の周期で取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 299 秒では動かない（周期より短い時間で通ってしまうテストにしない）
    await advance(299_000)
    expect(counts.storage).toBe(1)
    // 同じ時点で運用状態（60 秒周期）は 5 回取り直されている --- storage が
    // 運用状態の周期に紛れ込んでいないことの確認
    expect(counts.reservations).toBe(5)

    await advance(1_000)
    expect(counts.storage).toBe(2)
    // 周期は定数ではなくリテラルで押さえる（定数を変えても通るテストにしない）
    expect(storageRefreshIntervalMs).toBe(300_000)
  })

  /**
   * チューナー状態は専用グループを持つ（storage と同じ周期
   * `storageRefreshIntervalMs` を流用する）。キーが手書きなので、グループを
   * 落とすとどこからも取り直されなくなる --- epg の接頭辞には一致しない。
   */
  it('SSE が来なくてもチューナー状態は専用の周期で取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 299 秒では動かない（周期より短い時間で通ってしまうテストにしない）
    await advance(299_000)
    expect(counts.tuners).toBe(1)
    // 同じ時点で epg（10 分周期）はまだ 1 回も取り直していない
    expect(counts.epg).toBe(1)

    await advance(1_000)
    expect(counts.tuners).toBe(2)
  })

  /**
   * 周期タイマーは `refreshIntervalMs` の値ごとに別の `setInterval` に分かれて
   * いるので、あるクエリキーが周期の違う 2 グループに一致すると、両方の
   * タイマーが同時に発火する時刻（5 分と 10 分の最小公倍数 = 10 分）に別々の
   * 呼び出しで 2 回 invalidate される --- 1 回の `invalidateQueries` に
   * まとめても、呼び出しが分かれている以上まとめられない。
   *
   * tuners のキーを `/api/tuners` にして epg の `/api/sites/` と重ならなく
   * してあるのはこれを避けるため。同じ 5 分周期で epg と重ならない storage を
   * 対照に置く（tuners だけが余分に増えたことを言えるようにする）。
   */
  it('5 分と 10 分のタイマーが同時に発火しても tuners は余分に取り直さない', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    await advance(300_000)
    // t=300s: 5 分側だけが発火する
    expect(counts.storage).toBe(2)
    expect(counts.tuners).toBe(2)
    expect(counts.epg).toBe(1)

    await advance(300_000)
    // t=600s: 5 分側と 10 分側が両方発火する。storage（5 分・重なり無し）は
    // 3 回で、tuners も同じ 3 回でなければならない
    expect(counts.storage).toBe(3)
    expect(counts.tuners).toBe(3)
    expect(counts.epg).toBe(2)
  })

  it('storage にはトピックが無く、SSE イベントでは取り直されない', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    queryClient.setQueryData(storageKey, [])
    renderSubscriber(queryClient)

    expect(isStale(queryClient, storageKey)).toBe(false)

    // どのトピックを撃っても storage は無効化されない（対応する SSE トピックを
    // 持たないグループなので、定期 invalidate だけが回復経路になる）
    EventSourceStub.last?.emit('reservations')
    EventSourceStub.last?.emit('recordings')
    EventSourceStub.last?.emit('breakers')
    EventSourceStub.last?.emit('epg')

    expect(isStale(queryClient, storageKey)).toBe(false)
  })

  it('tuners にはトピックが無く、どの SSE イベントでも取り直されない', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    queryClient.setQueryData(tunersKey, [])
    renderSubscriber(queryClient)

    expect(isStale(queryClient, tunersKey)).toBe(false)

    EventSourceStub.last?.emit('reservations')
    EventSourceStub.last?.emit('recordings')
    EventSourceStub.last?.emit('breakers')
    // epg も巻き込まない --- キーを手書き（`/api/tuners`）にしてあるので、
    // epg の広い接頭辞（`/api/sites/`）には一致しない
    EventSourceStub.last?.emit('epg')

    expect(isStale(queryClient, tunersKey)).toBe(false)
  })

  it('epg のイベントで番組リスト（手書きのクエリキー）も取り直す', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    queryClient.setQueryData(programListKey, [])
    queryClient.setQueryData(epgKey, [])
    queryClient.setQueryData(['/api/recordings'], [])
    // サイト一覧（`/api/sites`）は epg の接頭辞（`/api/sites/`）にスラッシュ 1 文字の
    // 差で前方一致しない --- サイト一覧は EPG 同期でも録画の同期でも変わらない
    // 別の資源なので、epg の定期 invalidate に巻き込まれてはいけない
    queryClient.setQueryData(['/api/sites'], [])
    renderSubscriber(queryClient)

    expect(isStale(queryClient, programListKey)).toBe(false)

    EventSourceStub.last?.emit('epg')

    expect(isStale(queryClient, programListKey)).toBe(true)
    expect(isStale(queryClient, epgKey)).toBe(true)
    expect(isStale(queryClient, ['/api/recordings'])).toBe(false)
    expect(isStale(queryClient, ['/api/sites'])).toBe(false)
  })

  it('背面タブでは定期取得を投げず、前面に戻ると再開する', async () => {
    vi.useFakeTimers()
    const visibility = vi.spyOn(document, 'visibilityState', 'get')
    visibility.mockReturnValue('hidden')
    const { counts } = await renderLevelPaths()

    await advance(3 * 60_000)
    expect(counts.reservations).toBe(1)
    // 録画一覧も止まる（エンコード状態の収束がこの周期しか持たないので、
    // 「背面では止まる」は録画側でも主張しておく。docs/frontend/recordings.md）
    expect(counts.recordings).toBe(1)

    // 「何も起きないまま成功」でないことを、同じ観測方法で反対側を見て確かめる
    visibility.mockReturnValue('visible')
    await advance(60_000)
    expect(counts.reservations).toBe(2)
    expect(counts.recordings).toBe(2)
    visibility.mockRestore()
  })

  it('再接続したら切断中の変更を全グループ取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderLevelPaths()

    // 初回接続の open では取り直さない（各クエリの mount 時の取得と二重になる）
    EventSourceStub.last?.emit('open')
    await advance(0)
    expect(counts).toEqual({
      reservations: 1,
      recordings: 1,
      reservationDetail: 1,
      epg: 1,
      programList: 1,
      storage: 1,
      tuners: 1,
    })

    // 切断 → 再接続。切断中に飛んだ通知は再送されないので、周期を待たずに取り直す。
    // storage / tuners は SSE トピックを持たないが、再接続時の全グループ
    // invalidate は topic の有無を見ずに queryGroups 全体を回すのでここも取り直される
    EventSourceStub.last?.emit('error')
    EventSourceStub.last?.emit('open')
    await advance(0)

    expect(counts).toEqual({
      reservations: 2,
      recordings: 2,
      reservationDetail: 2,
      epg: 2,
      programList: 2,
      storage: 2,
      tuners: 2,
    })
  })
})

describe('useConnectionState', () => {
  it('connecting で始まり、open / error で遷移する', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Subscriber />
        <ConnectionStatusProbe />
      </QueryClientProvider>,
    )

    // マウント直後、まだ open を受けていない
    expect(view.getByTestId('connection-status')).toHaveTextContent(/^connecting:/)

    act(() => {
      EventSourceStub.last?.emit('open')
    })
    expect(view.getByTestId('connection-status')).toHaveTextContent(/^open:/)

    // 「切断中」は readyState ではなく error イベントで定義する
    act(() => {
      EventSourceStub.last?.emit('error')
    })
    expect(view.getByTestId('connection-status')).toHaveTextContent(/^disconnected:/)

    // 再接続（次の open）で復帰する
    act(() => {
      EventSourceStub.last?.emit('open')
    })
    expect(view.getByTestId('connection-status')).toHaveTextContent(/^open:/)
  })

  it('open すると lastConnectedAt（ISO 文字列）を持つ', () => {
    // lastConnectedAt はモジュール状態なので、このテストの前に他のテストが
    // open していれば mount 時点で既に非 null なことがある（アンマウントで
    // リセットするのは status だけ --- lastConnectedAt は「最後に繋がっていた
    // 時刻」なので、再マウントしても失ってはいけない値である）。
    // ここでは「open 後は必ず妥当な ISO 文字列になっている」ことだけを見る。
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Subscriber />
        <ConnectionStatusProbe />
      </QueryClientProvider>,
    )

    act(() => {
      EventSourceStub.last?.emit('open')
    })
    const text = view.getByTestId('connection-status').textContent ?? ''
    expect(text).toMatch(/^open:/)
    const lastConnectedAt = text.slice(text.indexOf(':') + 1)
    // ISO 文字列であること（形式が壊れたら Date が Invalid Date になり toISOString が例外を投げる）
    expect(new Date(lastConnectedAt).toISOString()).toBe(lastConnectedAt)
  })

  it('アンマウントすると connecting に戻る（モジュール状態のテスト間の持ち越しを防ぐ）', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Subscriber />
        <ConnectionStatusProbe />
      </QueryClientProvider>,
    )
    act(() => {
      EventSourceStub.last?.emit('open')
    })
    expect(view.getByTestId('connection-status')).toHaveTextContent(/^open:/)
    view.unmount()

    const view2 = render(
      <QueryClientProvider client={queryClient}>
        <ConnectionStatusProbe />
      </QueryClientProvider>,
    )
    expect(view2.getByTestId('connection-status')).toHaveTextContent(/^connecting:/)
  })
})
