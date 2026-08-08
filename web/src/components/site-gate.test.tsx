import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { SiteGate } from '@/components/site-gate'
import { useCurrentSite } from '@/lib/site'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Child は SiteGate の子として、実際に解決された site を画面に出す。 */
function Child() {
  const site = useCurrentSite()
  return <div data-testid="child">child content: {site}</div>
}

function renderGate() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <SiteGate>
        <Child />
      </SiteGate>
    </QueryClientProvider>,
  )
}

describe('SiteGate', () => {
  // GET /api/sites が解決するまで children を描画しない
  // （非同期の空虚な成功への対策。CLAUDE.md テスト規律）。
  // フェッチを手動で制御し、解決前のスナップショットを実際に確認する。
  it('GET /api/sites が解決するまで読み込み中を表示し、子は描画しない', async () => {
    let resolveFetch!: (res: Response) => void
    globalThis.fetch = vi.fn(
      () => new Promise<Response>((resolve) => (resolveFetch = resolve)),
    ) as unknown as typeof fetch

    renderGate()

    expect(await screen.findByText('読み込み中…')).toBeInTheDocument()
    expect(screen.queryByTestId('child')).not.toBeInTheDocument()

    resolveFetch(jsonResponse(['tokyo']))
    expect(await screen.findByTestId('child')).toBeInTheDocument()
  })

  it('成功したらレジストリの先頭サイトを SiteContext に流して子を描画する', async () => {
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(jsonResponse(['tokyo', 'osaka'])),
    ) as unknown as typeof fetch

    renderGate()

    expect(await screen.findByText('child content: tokyo')).toBeInTheDocument()
  })

  it('GET /api/sites が失敗したらエラーを表示し、子は描画しない', async () => {
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(new Response('boom', { status: 500 })),
    ) as unknown as typeof fetch

    renderGate()

    expect(await screen.findByText(/サイト一覧の取得に失敗しました/)).toBeInTheDocument()
    expect(screen.queryByTestId('child')).not.toBeInTheDocument()
  })

  // レジストリが空（internal/config.validateMirakcRegistry によりサーバー起動時に
  // 弾かれるので実運用では起きないはずだが）でも、空配列を site として使って
  // クラッシュせず、説明を出す。
  it('サイト一覧が空なら説明を表示し、子は描画しない', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve(jsonResponse([]))) as unknown as typeof fetch

    renderGate()

    expect(await screen.findByText(/利用可能なサイトがありません/)).toBeInTheDocument()
    expect(screen.queryByTestId('child')).not.toBeInTheDocument()
  })
})
