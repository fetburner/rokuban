import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { act, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { Recording } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { ingestRefetchIntervalMs, ingestStaleAfterMs } from '@/lib/ingest'
import { routeTree } from '@/routes'

/**
 * 取り込み進捗の**定期再取得の配線**（issue #212）。
 *
 * `hasLiveIngestProgress` 自体の判定は `lib/ingest.test.ts` が固定しているが、
 * それを `refetchInterval` に渡す 1 行（`pages/recordings.tsx` の
 * `useInfiniteQuery` と `pages/recording-detail.tsx` の `useGetRecording`）は
 * そこでは通らない。**その 1 行が広い述語に戻されても純関数のテストは緑のまま
 * になる**ので、実際に壊れる経路（ページが投げるリクエストの回数）の上に判定を
 * 置く（CLAUDE.md「壊す場所を、実際に壊れる経路の上に置く」）。
 *
 * 測り方は `lib/events.test.tsx` の前例に倣う --- 偽タイマーを進めて、その時刻
 * までに走った fetch を数える。`staleTime: Infinity` にしてあるので、増分は
 * 必ず `refetchInterval` 由来になる（放置やフォーカスでは増えない）。
 *
 * このファイルは `useServerEvents`（`lib/events.ts` の 60 秒 invalidate）を
 * マウントしない。混ぜると「5 秒の周期が止まったか」と「60 秒の invalidate が
 * 効いたか」が同じカウンタに乗って区別できなくなる。60 秒側は
 * `lib/events.test.tsx` が別に固定している。
 */

function sampleRecording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 3,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: 1,
    title: 'ポーリングを測る録画',
    startAt: '2026-01-01T12:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-01T12:30:00Z',
    ...overrides,
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** counts はページが投げたリクエストの回数。 */
type Counts = { list: number; detail: number }

/**
 * setupFetch は `/api/recordings` 系だけを数えるフェイクを張る。
 *
 * `build` は**リクエストのたびに**呼ぶ --- 転送が進んでいる状況では worker が
 * `observed_at` を書き直し続けるので、テスト側も毎回「今」の観測を返さないと
 * 偽タイマーを進めた瞬間に自分で停滞扱いになってしまう。
 *
 * 一覧のクエリは `limit=50`（`pages/recordings.tsx` の `pageSize`）で識別する。
 * 同じパスに `?status=finished&limit=20` を投げる別の利用者（ヘッダーの
 * ストレージ残高。`lib/storage-forecast.ts`）が居るため、パスだけでは
 * 数えたいものと混ざる。
 */
function setupFetch(build: () => Recording[], counts: Counts) {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))

    if (url.pathname === '/api/recordings') {
      if (url.searchParams.get('limit') === '50') counts.list += 1
      return Promise.resolve(jsonResponse(build()))
    }
    const detail = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (detail) {
      counts.detail += 1
      const found = build().find((r) => r.id === Number(detail[1]))
      return Promise.resolve(jsonResponse(found ?? build()[0]))
    }
    return Promise.resolve(jsonResponse([]))
  }) as unknown as typeof fetch
}

/**
 * advance は偽タイマーを進め、それによって走り出した fetch と再描画を流し切る
 * （`lib/events.test.tsx` の同名ヘルパーと同じ理由 --- 偽タイマー下では
 * `waitFor` が進行を作れない）。
 */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

/**
 * renderPage はページを描き、初回取得が済むまで進めてからカウンタを返す。
 * 戻った時点の `counts` は初回ぶんだけで、以降の増分はすべて定期再取得。
 */
async function renderPage(path: string, build: () => Recording[]) {
  window.scrollTo = vi.fn()
  const counts: Counts = { list: 0, detail: 0 }
  setupFetch(build, counts)

  const queryClient = new QueryClient({
    // staleTime / gcTime を無限にして、放置やマウントでは取り直させない。
    // 増分が refetchInterval 由来であることをこれで担保する
    defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } },
  })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  // SiteGate → ページ本体 → 初回取得までを流し切る
  await advance(0)
  await advance(0)
  return { counts, queryClient }
}

/** transferring は「転送中」の録画を作る。observedAgoMs で停滞させられる。 */
function transferring(observedAgoMs: number): Recording {
  return sampleRecording({
    ingest: {
      state: 'transferring',
      writtenBytes: 250_000_000,
      expectedBytes: 1_000_000_000,
      observedAt: new Date(Date.now() - observedAgoMs).toISOString(),
    },
  })
}

afterEach(() => {
  vi.useRealTimers()
})

describe('録画一覧の取り込み進捗ポーリング（配線）', () => {
  it('進捗が動いている間は 5 秒周期で取り直す', async () => {
    vi.useFakeTimers()
    // 毎回「今」の観測を返す = 転送が進み続けている状況
    const { counts } = await renderPage('/recordings', () => [transferring(1_000)])
    expect(counts.list).toBe(1)

    // 4 秒では動かない（周期より短い時間で通ってしまうテストにしない）
    await advance(4_000)
    expect(counts.list).toBe(1)

    await advance(1_000)
    expect(counts.list).toBe(2)

    await advance(5_000)
    expect(counts.list).toBe(3)

    // 周期は定数ではなくリテラルで押さえる（定数を変えても通るテストにしない）
    expect(ingestRefetchIntervalMs).toBe(5_000)
  })

  it('進捗が停滞したら周期的な取り直しを止める', async () => {
    vi.useFakeTimers()
    // observed_at が古いまま更新されない = River のバックオフ待ち / discard 後の
    // record_sweep 待ち。分オーダーでしか動かないものを 5 秒で叩かない
    const { counts } = await renderPage('/recordings', () => [
      transferring(ingestStaleAfterMs + 60_000),
    ])
    expect(counts.list).toBe(1)

    await advance(60_000)
    expect(counts.list).toBe(1)
  })

  it('取り込み待ち・取り込みが来ない録画だけの一覧では取り直さない', async () => {
    vi.useFakeTimers()
    // pending には「権限不足で失敗し続けている ingest」も落ちる（issue #211）。
    // failed / canceled は ingest ジョブが一度も投入されない（state='unknown'）。
    // どちらも真にすると 5 秒ポーリングが恒久的に続く
    const { counts } = await renderPage('/recordings', () => [
      sampleRecording({ id: 1, title: '取り込み待ち', ingest: { state: 'pending' } }),
      sampleRecording({ id: 2, title: '失敗', status: 'failed', ingest: { state: 'unknown' } }),
      sampleRecording({ id: 4, title: '中止', status: 'canceled', ingest: { state: 'unknown' } }),
      sampleRecording({ id: 5, title: '録画中', status: 'recording', ingest: { state: 'unknown' } }),
      sampleRecording({ id: 6, title: '取り込み済み', sizeBytes: 100, ingest: { state: 'committed' } }),
    ])
    expect(counts.list).toBe(1)

    await advance(60_000)
    expect(counts.list).toBe(1)
  })

  it('停滞していた転送が再開すれば 5 秒周期に戻る（自己回復）', async () => {
    vi.useFakeTimers()
    // 最初は停滞。ある時点から worker が観測を書き直し始める
    let resumed = false
    const { counts, queryClient } = await renderPage('/recordings', () => [
      transferring(resumed ? 1_000 : ingestStaleAfterMs + 60_000),
    ])
    expect(counts.list).toBe(1)

    await advance(60_000)
    expect(counts.list).toBe(1)

    // 再開しただけでは、こちらから取りに行かない限り気付けない。実際に拾うのは
    // lib/events.ts の 60 秒 invalidate なので、それと同じ invalidate を 1 回だけ
    // 起こす（このファイルは useServerEvents をマウントしないため手で模す）
    resumed = true
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ['/api/recordings'] })
    })
    await advance(0)
    const afterResume = counts.list
    expect(afterResume).toBeGreaterThan(1)

    // 新しい観測を読んだ後は 5 秒周期が復活する
    await advance(5_000)
    expect(counts.list).toBe(afterResume + 1)
  })
})

describe('録画単体ページの取り込み進捗ポーリング（配線）', () => {
  it('進捗が動いている間は 5 秒周期で取り直す', async () => {
    vi.useFakeTimers()
    const { counts } = await renderPage('/recordings/3', () => [transferring(1_000)])
    expect(counts.detail).toBe(1)

    await advance(4_000)
    expect(counts.detail).toBe(1)

    await advance(1_000)
    expect(counts.detail).toBe(2)
  })

  it('進捗が停滞したら周期的な取り直しを止める', async () => {
    vi.useFakeTimers()
    const { counts } = await renderPage('/recordings/3', () => [
      transferring(ingestStaleAfterMs + 60_000),
    ])
    expect(counts.detail).toBe(1)

    await advance(60_000)
    expect(counts.detail).toBe(1)
  })

  it('取り込みが来ない録画では取り直さない', async () => {
    vi.useFakeTimers()
    const { counts } = await renderPage('/recordings/3', () => [
      sampleRecording({ status: 'failed', ingest: { state: 'unknown' } }),
    ])
    expect(counts.detail).toBe(1)

    await advance(60_000)
    expect(counts.detail).toBe(1)
  })
})
