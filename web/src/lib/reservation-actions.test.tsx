import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ProgramListItem, Reservation } from '@/api/generated'
import { programIdentity, type SiteProgram } from '@/lib/all-sites-services'
import { useReservationActions } from '@/lib/reservation-actions'

// useReservationActions は useNavigate（router context）と useToast
// （ToastProvider context）を呼ぶが、この自己修復ロジックの検証には
// どちらの中身も関係しない。実物の context を組み立てる代わりに no-op へ
// 差し替える。
vi.mock('@tanstack/react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@tanstack/react-router')>()
  return { ...actual, useNavigate: () => vi.fn() }
})
vi.mock('@/components/toaster', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/components/toaster')>()
  return { ...actual, useToast: () => vi.fn() }
})

const site = 'default'
const programId = 1
const key = programIdentity(site, programId)

const program: SiteProgram = {
  site,
  programId,
  networkId: 32736,
  serviceId: 1024,
  eventId: programId,
  startAt: new Date().toISOString(),
  endAt: new Date().toISOString(),
  durationMs: 0,
  name: '番組',
  description: '',
  genres: [],
  isFree: true,
} satisfies ProgramListItem & { site: string }

const sourceByProgramId = new Map<string, Reservation['source']>()
const programListKey = ['/api/programs', 'infinite'] as const

afterEach(() => {
  vi.restoreAllMocks()
})

/** stubFetch は intent PUT/DELETE を常に成功させる。 */
function stubFetch() {
  globalThis.fetch = vi.fn(() =>
    Promise.resolve(new Response(JSON.stringify({}), { status: 200 })),
  ) as unknown as typeof fetch
}

function renderActions(initialServerReservedIds: ReadonlySet<string>) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
  const view = renderHook(
    ({ serverReservedIds }: { serverReservedIds: ReadonlySet<string> }) =>
      useReservationActions(serverReservedIds, sourceByProgramId, false, undefined),
    { wrapper, initialProps: { serverReservedIds: initialServerReservedIds } },
  )
  return { ...view, queryClient }
}

describe('useReservationActions の楽観更新の自己修復', () => {
  it('サーバー値が追いついた後にサーバー値が反転しても、古い楽観上書きは復活しない', async () => {
    stubFetch()

    // 1. サーバーはまだ X を予約していない。予約ボタンを押すと楽観的に true になる
    const { result, rerender } = renderActions(new Set())
    await act(async () => {
      result.current.reserve(program)
      // reserve は mutateAsync を await する非同期 IIFE なので、成功まで
      // 待ってから観測する
      await Promise.resolve()
    })
    await waitFor(() => expect(result.current.reservedProgramIds.has(key)).toBe(true))

    // 2. サーバーが追いつく（ruler が予約行を作る）。表示は変わらず true のまま
    rerender({ serverReservedIds: new Set([key]) })
    await waitFor(() => expect(result.current.reservedProgramIds.has(key)).toBe(true))

    // 3. 別経路（別タブ・ルール再評価）でサーバー側の予約が消える。
    // 自己修復が効いていれば、古い楽観上書き（true）は既に消えているので
    // サーバー値どおり「未予約」に見える。効いていなければ true のまま残る
    // （バグ: リロードするまで誤表示が続く）。
    rerender({ serverReservedIds: new Set() })
    expect(result.current.reservedProgramIds.has(key)).toBe(false)
  })
})

describe('useReservationActions の reservationStateUnknown ガード', () => {
  it('reservationStateUnknown が true のとき、reserve を呼んでも PUT .../intent が飛ばない（ボタンの disabled とは別の二重の網）', async () => {
    stubFetch()
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
    const { result } = renderHook(
      () => useReservationActions(new Set(), sourceByProgramId, true, undefined),
      { wrapper },
    )

    // reserve は mutateAsync を await する非同期 IIFE。ガードが無ければ
    // このマイクロタスクの間に fetch（PUT .../intent）が飛ぶ。
    await act(async () => {
      result.current.reserve(program)
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe('useReservationActions の skip 意図解除', () => {
  it('clearIntent は DELETE .../intent を送り、番組一覧を無効化する', async () => {
    stubFetch()
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    const { result, queryClient } = renderActions(new Set())
    queryClient.setQueryData(programListKey, [])
    expect(queryClient.getQueryCache().find({ queryKey: programListKey })?.isStale()).toBe(false)

    await act(async () => {
      result.current.clearIntent(program)
      await Promise.resolve()
      await Promise.resolve()
    })

    expect(fetchMock).toHaveBeenCalledWith(
      `/api/sites/${site}/programs/${programId}/intent`,
      expect.objectContaining({ method: 'DELETE' }),
    )
    expect(queryClient.getQueryCache().find({ queryKey: programListKey })?.isStale()).toBe(true)
  })

  it('reserve は番組一覧を無効化しない', async () => {
    stubFetch()
    const { result, queryClient } = renderActions(new Set())
    queryClient.setQueryData(programListKey, [])

    await act(async () => {
      result.current.reserve(program)
      await Promise.resolve()
      await Promise.resolve()
    })

    // reserve は skip 意図を変更せず、予約集合と容量超過だけを更新する。
    expect(queryClient.getQueryCache().find({ queryKey: programListKey })?.isStale()).toBe(false)
  })
})
