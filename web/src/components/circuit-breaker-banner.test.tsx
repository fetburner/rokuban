import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { CircuitBreaker } from '@/api/generated'
import { CircuitBreakerBanner } from '@/components/circuit-breaker-banner'
import { ToastProvider } from '@/components/toaster'

function renderBanner() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0 },
      mutations: { retry: false },
    },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <CircuitBreakerBanner />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** fetch をトピック別に振り分けるスタブ。一覧取得と resume 呼び出しの両方に応答する。 */
function stubFetch(opts: { list: CircuitBreaker[]; resume?: 'success' | 'error' }) {
  const fn = vi.fn((input: string | URL | Request, _init?: RequestInit) => {
    const url = String(input)
    if (url.includes('/resume')) {
      if (opts.resume === 'error') {
        return Promise.resolve(jsonResponse({ error: '発動していません' }, 404))
      }
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    return Promise.resolve(jsonResponse(opts.list))
  })
  globalThis.fetch = fn as unknown as typeof fetch
  return fn
}

const trippedBreaker: CircuitBreaker = {
  site: 'default',
  name: 'ruler_deletes',
  trippedAt: '2026-07-25T10:00:00.000Z',
  pending: 3,
  threshold: 20,
  detail: {
    total: 3,
    programs: [
      { programId: 101, title: '朝のニュース' },
      { programId: 102, title: '昼の情報番組' },
    ],
  },
}

const excerptBreaker: CircuitBreaker = {
  ...trippedBreaker,
  name: 'reconcile_total_loss',
  pending: 200,
  detail: {
    total: 200,
    programs: trippedBreaker.detail.programs,
  },
}

describe('CircuitBreakerBanner', () => {
  it('発動中のブレーカーがあると通知が表示される', async () => {
    stubFetch({ list: [trippedBreaker] })

    renderBanner()

    expect(await screen.findByText('ルール評価による予約の削除が停止中')).toBeInTheDocument()
    expect(screen.getByText(/保留 3 件/)).toBeInTheDocument()
    expect(screen.getByText(/閾値 20/)).toBeInTheDocument()
    expect(screen.getByText(/削除が保留されています/)).toBeInTheDocument()
  })

  it('何も発動していなければ何も表示されない', async () => {
    const fetchMock = stubFetch({ list: [] })

    renderBanner()

    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    // ToastProvider 自体は常に空の aria-live コンテナを持つので、
    // バナー固有の要素・文言が無いことをピンポイントで確認する
    // （余計な枠（role="alert"）を出さないことが要件）。
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/削除が保留されています/)).not.toBeInTheDocument()
  })

  it('detail の番組（programId と title）が表示される', async () => {
    stubFetch({ list: [trippedBreaker] })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '内訳を見る' }))

    expect(screen.getByText(/101/)).toBeInTheDocument()
    expect(screen.getByText(/朝のニュース/)).toBeInTheDocument()
    expect(screen.getByText(/102/)).toBeInTheDocument()
    expect(screen.getByText(/昼の情報番組/)).toBeInTheDocument()
  })

  it('total が programs の件数より多いとき、抜粋であることが分かる表示になる', async () => {
    stubFetch({ list: [excerptBreaker] })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '内訳を見る' }))

    expect(screen.getByText(/200 件中 2 件を表示/)).toBeInTheDocument()
    expect(screen.getByText(/抜粋/)).toBeInTheDocument()
  })

  it('再開ボタンは確認を経てから API を叩く（確認せずに叩かれない）', async () => {
    const fetchMock = stubFetch({ list: [trippedBreaker], resume: 'success' })
    const user = userEvent.setup()
    renderBanner()

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

    // ダイアログを開くだけでは叩かれない
    await user.click(await screen.findByRole('button', { name: '再開' }))
    expect(await screen.findByRole('button', { name: '再開する' })).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(1)

    // キャンセルしても叩かれない
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '再開する' })).not.toBeInTheDocument(),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)

    // 確認してはじめて叩かれる
    await user.click(screen.getByRole('button', { name: '再開' }))
    await user.click(await screen.findByRole('button', { name: '再開する' }))

    // 成功後に一覧を再取得するので呼び出し回数は増え続けうる。POST が
    // 実際に行われたことは呼び出し内容そのもので確認する（回数は問わない）。
    await waitFor(() => {
      const resumeCall = fetchMock.mock.calls.find(([u]) => String(u).includes('/resume'))
      expect(resumeCall).toBeDefined()
      expect(String(resumeCall![0])).toBe('/api/sites/default/breakers/ruler_deletes/resume')
      expect(resumeCall![1]).toMatchObject({ method: 'POST' })
    })

    expect(await screen.findByText(/再開しました/)).toBeInTheDocument()

    // 確定後はダイアログが閉じる。呼び出し側は個別のクローズ処理を持たず
    // AlertDialogAction（Close ラップ）に任せているので、ここで固定する（#131）。
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '再開する' })).not.toBeInTheDocument(),
    )
  })

  it('再開が失敗したときエラーが表示される（黙って成功に見せない）', async () => {
    const fetchMock = stubFetch({ list: [trippedBreaker], resume: 'error' })
    const user = userEvent.setup()
    renderBanner()

    await user.click(await screen.findByRole('button', { name: '再開' }))
    await user.click(await screen.findByRole('button', { name: '再開する' }))

    expect(await screen.findByText(/再開に失敗しました/)).toBeInTheDocument()
    // 失敗しても発動中の表示自体は消えない(黙って成功したように見せない)
    expect(screen.getByText('ルール評価による予約の削除が停止中')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
