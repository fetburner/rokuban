import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { AppShell } from '@/components/app-shell'

const NAV_LABELS = ['番組', '検索', 'ルール', '予約', '録画']
const STORAGE_KEY = 'rokuban:sidebar:collapsed'

/**
 * AppShell は `useRouterState` / `Link`（tanstack/react-router）を使うため、
 * routes.test.tsx と同様に最小限の Router を組んでレンダリングする。
 * ページ内容は本テストの関心事ではないので、ルートは "/" だけを持つ。
 */
function renderShell() {
  const rootRoute = createRootRoute({
    component: () => (
      <AppShell>
        <div>ページ本体</div>
      </AppShell>
    ),
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })

  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router as never} />
    </QueryClientProvider>,
  )
}

/** サイドバーのトグルボタン（"ナビゲーションを畳む" / "ナビゲーションを開く"）。 */
function getToggle() {
  return screen.getByRole('button', { name: /ナビゲーションを(畳む|開く)/ })
}

/**
 * ルートの初期解決は非同期（TanStack Router がルートマッチを Promise で返す）
 * なので、レンダリング直後は `findBy*` で最初の描画を待つ。以降の状態遷移は
 * userEvent 経由の act 内で同期に反映されるので `getToggle` で読める。
 */
function findToggle() {
  return screen.findByRole('button', { name: /ナビゲーションを(畳む|開く)/ })
}

beforeEach(() => {
  localStorage.clear()
  // CircuitBreakerBanner がマウント時に GET /api/breakers を叩くため、
  // 空配列を返すスタブを用意する（発動中のブレーカーが無い前提）。
  globalThis.fetch = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ) as unknown as typeof fetch
  // jsdom は window.scrollTo を実装していないが、ルーターのスクロール復元が呼ぶ
  window.scrollTo = vi.fn()
})

describe('AppShell / Sidebar の畳み込み', () => {
  it('初期状態（未保存）は展開されている', async () => {
    renderShell()

    expect(await findToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('録番')).toBeInTheDocument()
  })

  it('トグルを押すと畳まれ、もう一度押すと展開に戻る（両方向）', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()

    // 展開 → 畳む
    await user.click(getToggle())
    expect(getToggle()).toHaveAttribute('aria-expanded', 'false')
    expect(getToggle()).toHaveAttribute('aria-label', 'ナビゲーションを開く')
    // ロゴの見出しは畳んだ状態では出ない（DOM から消える。CSS の hidden ではない）
    expect(screen.queryByText('録番')).not.toBeInTheDocument()

    // 畳む → 展開
    await user.click(getToggle())
    expect(getToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(getToggle()).toHaveAttribute('aria-label', 'ナビゲーションを畳む')
    expect(screen.getByText('録番')).toBeInTheDocument()
  })

  it('畳んだ状態でも全ナビゲーション項目が読み上げ名で引ける', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()

    await user.click(getToggle())
    expect(getToggle()).toHaveAttribute('aria-expanded', 'false')

    for (const label of NAV_LABELS) {
      // jsdom は `hidden md:flex` 等の CSS を適用しないため、ボトムタブと
      // サイドバーの両方が常に DOM 上に存在する。sidebar 側のリンクが
      // ラベルを失うとここが 1（ボトムタブ分だけ）に減るので、
      // 2 以上であることまで確認しないと sidebar 側の退行を検知できない。
      const links = screen.getAllByRole('link', { name: label })
      expect(links.length).toBeGreaterThanOrEqual(2)
      for (const link of links) {
        expect(link).toHaveAttribute('href')
      }
    }
  })

  it('畳んだ状態を localStorage に保存し、次回の初期化で復元する', async () => {
    const user = userEvent.setup()
    const view = renderShell()
    await findToggle()

    await user.click(getToggle())
    expect(localStorage.getItem(STORAGE_KEY)).toBe('1')

    view.unmount()
    renderShell()

    expect(await findToggle()).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('録番')).not.toBeInTheDocument()
  })

  it('展開状態を保存した場合も次回の初期化で復元する（未保存の展開と区別する）', async () => {
    const user = userEvent.setup()
    const view = renderShell()
    await findToggle()

    // 一度畳んでから戻し、明示的に "0"（展開）を保存させる
    await user.click(getToggle())
    await user.click(getToggle())
    expect(localStorage.getItem(STORAGE_KEY)).toBe('0')

    view.unmount()
    renderShell()

    expect(await findToggle()).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('録番')).toBeInTheDocument()
  })

  it('保存済みで畳んだ状態から始めても正しく初期化される', async () => {
    localStorage.setItem(STORAGE_KEY, '1')

    renderShell()

    expect(await findToggle()).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('録番')).not.toBeInTheDocument()
  })

  it('md 未満向けのボトムタブは畳み状態に関係なく常に出る', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()

    // 展開時: サイドバーとボトムタブの両方に主ナビゲーションがある
    const navsExpanded = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    expect(navsExpanded).toHaveLength(2)
    for (const label of NAV_LABELS) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThanOrEqual(2)
    }

    await user.click(getToggle())

    // 畳んだ後もボトムタブ側のナビゲーションは変わらず存在する
    const navsCollapsed = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    expect(navsCollapsed).toHaveLength(2)
    for (const label of NAV_LABELS) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThanOrEqual(2)
    }
  })

  it('現在地には aria-current="page" が付く', async () => {
    renderShell()
    await findToggle()

    const links = screen.getAllByRole('link', { name: '番組' })
    for (const link of links) {
      expect(link).toHaveAttribute('aria-current', 'page')
    }
    const otherLinks = screen.getAllByRole('link', { name: '検索' })
    for (const link of otherLinks) {
      expect(link).not.toHaveAttribute('aria-current')
    }
  })
})
