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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
  return renderHook(
    ({ serverReservedIds }: { serverReservedIds: ReadonlySet<string> }) =>
      useReservationActions(serverReservedIds, sourceByProgramId),
    { wrapper, initialProps: { serverReservedIds: initialServerReservedIds } },
  )
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
