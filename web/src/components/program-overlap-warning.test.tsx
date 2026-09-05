import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { useGetProgramOverlaps, type ProgramOverlaps } from '@/api/generated'
import { ProgramOverlapWarning } from '@/components/program-overlap-warning'

/** testSite はこのテストが `ProgramOverlapWarning` に渡す site。 */
const testSite = 'default'

/**
 * Harness は ProgramOverlapWarning と同じクエリキーを共有する監視用の隣接要素
 * （`data-testid="query-status"`）を一緒に描画する。react-query は同一
 * queryKey を 1 回の fetch に重複排除するので fetch 回数は変わらない。
 *
 * 「0 件なら何も描画しない」を確かめるテストは、queryFn が実際に解決した
 * あとの「不在」を確認する必要がある。fetch が呼ばれた直後（Promise 未解決）に
 * 判定すると、実装を壊して count===0 のガードを外しても false positive で
 * 通ってしまう（unwrap(query.data) がまだ undefined のため）。この要素で
 * クエリの決着（success/error）を待ってから不在を確認する。
 */
function Harness({ programId }: { programId: number }) {
  const query = useGetProgramOverlaps(testSite, programId)
  return (
    <>
      <div data-testid="query-status">{query.status}</div>
      <ProgramOverlapWarning site={testSite} programId={programId} />
    </>
  )
}

function renderWarning(programId = 42) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0 },
    },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <Harness programId={programId} />
    </QueryClientProvider>,
  )
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubFetch(overlaps: ProgramOverlaps) {
  const fn = vi.fn((_input: string | URL | Request, _init?: RequestInit) =>
    Promise.resolve(jsonResponse(overlaps)),
  )
  globalThis.fetch = fn as unknown as typeof fetch
  return fn
}

const noOverlap: ProgramOverlaps = { count: 0, reservations: [] }

const twoOverlaps: ProgramOverlaps = {
  count: 2,
  reservations: [
    { programId: 101, title: 'ニュース7', startAt: '2026-07-25T19:00:00+09:00', durationMs: 1800000 },
    { programId: 102, title: 'ドラマ特番', startAt: '2026-07-25T19:15:00+09:00', durationMs: 2700000 },
  ],
}

describe('ProgramOverlapWarning', () => {
  it('件数が 0 なら何も表示されない', async () => {
    stubFetch(noOverlap)
    renderWarning()

    // クエリが決着する（success）まで待ってから不在を確認する（Harness のコメント参照）
    await screen.findByText('success')
    expect(screen.queryByText(/同じ時間帯に/)).not.toBeInTheDocument()
  })

  it('件数が 1 以上なら件数と内訳が表示される', async () => {
    stubFetch(twoOverlaps)
    renderWarning()

    expect(await screen.findByText(/同じ時間帯に2件の予約があります/)).toBeInTheDocument()
    expect(screen.getByText(/ニュース7/)).toBeInTheDocument()
    expect(screen.getByText(/ドラマ特番/)).toBeInTheDocument()
  })

  // 文言の回帰テスト。チューナー本数を見ていない（issue #21 の「案 C」）ため、
  // 「録画できません」「競合しています」のような断定はしてはいけない
  // （M2-10 で tuner_sync 射影 + 容量判定が入るまで断定禁止。docs/data.md §6.5）。
  it('断定的な文言を使っていない', async () => {
    stubFetch(twoOverlaps)
    renderWarning()

    await screen.findByText(/同じ時間帯に2件の予約があります/)

    const banned = ['録画できません', '競合しています', '競合', '録画できない']
    for (const phrase of banned) {
      expect(screen.queryByText(new RegExp(phrase))).not.toBeInTheDocument()
    }
  })

  it('programId ごとに GET /api/sites/{site}/programs/{id}/overlaps を叩く', async () => {
    const fetchMock = stubFetch(noOverlap)
    renderWarning(777)

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(String(fetchMock.mock.calls[0][0])).toBe(
      `/api/sites/${testSite}/programs/777/overlaps`,
    )
  })
})
