import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { AppShell } from '@/components/app-shell'

/**
 * デスクトップのサイドバーに出る全項目。並びは頻度順
 * （docs/frontend/design.md §頻度 3 段: 一等地=番組・録画 / 中間=予約・ライブ /
 * 端=検索・ルール）で、`app-shell.tsx` の `navItems` と比較するのではなく
 * ここにリテラルで書く（実装の定数と比較するテストは並びが崩れても検知できない）。
 */
const SIDEBAR_LABELS = ['番組', '録画', '予約', 'ライブ', '検索', 'ルール']
/** モバイルのボトムタブに常時出る項目（「その他」を除く）。 */
const MOBILE_PRIMARY_LABELS = ['番組', '録画', '予約']
/** モバイルで「その他」ポップオーバーに畳まれる項目。 */
const MOBILE_MORE_LABELS = ['ライブ', '検索', 'ルール']
const STORAGE_KEY = 'rokuban:sidebar:collapsed'

/**
 * AppShell は `useRouterState` / `Link`（tanstack/react-router）を使うため、
 * routes.test.tsx と同様に最小限の Router を組んでレンダリングする。
 * ページ内容は本テストの関心事ではないので、各ルートは何も描画しない。
 * ナビゲーションのアクティブ表示・実際のリンク遷移を確認するテストのために
 * `navItems` の全パス分のルートを用意しておく。
 */
function renderShell(initialPath = '/', options: ShellStubOptions = {}) {
  stubFetch(options)

  const rootRoute = createRootRoute({
    component: () => (
      <AppShell>
        <div>ページ本体</div>
      </AppShell>
    ),
  })
  const paths = ['/', '/recordings', '/reservations', '/live', '/search', '/rules']
  const children = paths.map((path) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: () => null }),
  )
  const router = createRouter({
    routeTree: rootRoute.addChildren(children),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
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

/** モバイルの「その他」ポップオーバーのトリガー。 */
function getMoreTrigger() {
  return screen.getByRole('button', { name: 'その他' })
}

/**
 * findLiveLinks は「ライブ」の導線が現れるのを待つ。
 *
 * ライブ項目は `GET /api/capabilities` の解決後に現れる（issue #209）ので、
 * 全項目を前提にするテストはこれを待ってから読む。待たずに同期で読むと
 * 「まだ来ていない」を「出ている / 出ていない」と取り違える
 * （CLAUDE.md「非同期の空虚な成功に注意する」）。
 */
function findLiveLinks() {
  return screen.findAllByRole('link', { name: 'ライブ' })
}

/**
 * stubFetch は AppShell がマウント時に叩く 2 本を振り分ける。
 *
 * - `/api/breakers`: CircuitBreakerBanner（発動中のブレーカーが無い前提の空配列）
 * - `/api/capabilities`: ナビの出し分け（issue #209。`live` に応じて「ライブ」が出る）
 */
function stubFetch({ live = true, capabilitiesStatus = 200 }: ShellStubOptions) {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(
        capabilitiesStatus === 200
          ? new Response(JSON.stringify({ live }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          : new Response('boom', { status: capabilitiesStatus }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify([]), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
  }) as unknown as typeof fetch
}

/** ShellStubOptions は renderShell がサーバー応答を差し替えるためのつまみ。 */
type ShellStubOptions = {
  /** サーバーの `live.enabled`（`GET /api/capabilities`）。既定は有効。 */
  live?: boolean
  /** 能力 API のステータス。200 以外なら本文は無視され、取得失敗になる。 */
  capabilitiesStatus?: number
}

beforeEach(() => {
  localStorage.clear()
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

  it('畳んだ状態でもサイドバーの全項目が読み上げ名で引ける', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()
    await findLiveLinks()

    await user.click(getToggle())
    expect(getToggle()).toHaveAttribute('aria-expanded', 'false')

    // ボトムタブ（番組/録画/予約）にも同名のリンクがあるため、`screen` に対する
    // クエリでは「サイドバー側が読み上げ名を失って 2→1 に減った」regression を
    // 検知できない（1 でも通ってしまう）。サイドバーに絞って各ラベルが
    // ちょうど 1 件あることを見る。
    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const sidebarNav = navs.find(
      (nav) => within(nav).queryAllByRole('link').length === SIDEBAR_LABELS.length,
    )
    expect(sidebarNav).toBeDefined()
    for (const label of SIDEBAR_LABELS) {
      const links = within(sidebarNav as HTMLElement).getAllByRole('link', { name: label })
      expect(links).toHaveLength(1)
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
    await findLiveLinks()

    // 展開時: サイドバーとボトムタブの両方に出る項目（一等地〜中間の一部）は
    // リンクが 2 つ（サイドバー + ボトムタブ）。「その他」に畳まれた項目は
    // サイドバーにしか出ないので 1 つ。
    const navsExpanded = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    expect(navsExpanded).toHaveLength(2)
    for (const label of MOBILE_PRIMARY_LABELS) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThanOrEqual(2)
    }
    for (const label of MOBILE_MORE_LABELS) {
      expect(screen.getAllByRole('link', { name: label })).toHaveLength(1)
    }
    expect(getMoreTrigger()).toBeInTheDocument()

    await user.click(getToggle())

    // 畳んだ後もボトムタブ側のナビゲーションは変わらず存在する
    const navsCollapsed = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    expect(navsCollapsed).toHaveLength(2)
    for (const label of MOBILE_PRIMARY_LABELS) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThanOrEqual(2)
    }
    expect(getMoreTrigger()).toBeInTheDocument()
  })

  it('サイドバーの並びは頻度順（番組/録画/予約/ライブ/検索/ルール）で固定されている', async () => {
    renderShell()
    await findToggle()
    await findLiveLinks()

    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const sidebarNav = navs.find(
      (nav) => within(nav).queryAllByRole('link').length === SIDEBAR_LABELS.length,
    )
    expect(sidebarNav).toBeDefined()
    const labels = within(sidebarNav as HTMLElement)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(SIDEBAR_LABELS)
  })

  it('ボトムタブの常時項目の並びは頻度順（番組/録画/予約）で固定されている', async () => {
    renderShell()
    await findToggle()

    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const bottomNav = navs.find((nav) => within(nav).queryByRole('button', { name: 'その他' }))
    expect(bottomNav).toBeDefined()
    const labels = within(bottomNav as HTMLElement)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(MOBILE_PRIMARY_LABELS)
  })

  it('ボトムタブは常時 4 個（常時項目 3 個 + その他）に抑えている', async () => {
    renderShell()
    await findToggle()

    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const bottomNav = navs.find((nav) => within(nav).queryByRole('button', { name: 'その他' }))
    expect(bottomNav).toBeDefined()
    expect(within(bottomNav as HTMLElement).getAllByRole('listitem')).toHaveLength(4)
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

describe('モバイルの「その他」', () => {
  it('既定では閉じており、トリガーを押すとライブ・検索・ルールへのリンクが現れる', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()
    await findLiveLinks()

    expect(screen.queryByRole('dialog', { name: 'その他のナビゲーション' })).not.toBeInTheDocument()

    await user.click(getMoreTrigger())

    const menu = await screen.findByRole('dialog', { name: 'その他のナビゲーション' })
    for (const label of MOBILE_MORE_LABELS) {
      expect(within(menu).getByRole('link', { name: label })).toHaveAttribute('href')
    }
  })

  it('中の並びは頻度順（ライブ/検索/ルール）で固定されている', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()
    await findLiveLinks()

    await user.click(getMoreTrigger())
    const menu = await screen.findByRole('dialog', { name: 'その他のナビゲーション' })
    const labels = within(menu)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(MOBILE_MORE_LABELS)
  })

  it('中の項目をクリックすると実際に遷移し、ポップオーバーが閉じる', async () => {
    const user = userEvent.setup()
    renderShell()
    await findToggle()
    await findLiveLinks()

    await user.click(getMoreTrigger())
    const menu = await screen.findByRole('dialog', { name: 'その他のナビゲーション' })
    await user.click(within(menu).getByRole('link', { name: 'ライブ' }))

    // ポップオーバーが DOM 上から消える（閉じ忘れは jsdom で観測できる壊れ方）
    await vi.waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'その他のナビゲーション' })).not.toBeInTheDocument(),
    )
    // 実際に /live へ遷移したこと（見た目だけ閉じてルートは変わっていない、を弾く）
    const liveLink = screen.getByRole('link', { name: 'ライブ' })
    expect(liveLink).toHaveAttribute('aria-current', 'page')
  })

  it('「その他」配下のルート表示中はトリガーがアクティブ、それ以外では非アクティブ（両方向）', async () => {
    const { unmount } = renderShell('/search')
    await findToggle()

    expect(getMoreTrigger()).toHaveAttribute('aria-current', 'true')
    for (const label of MOBILE_PRIMARY_LABELS) {
      for (const link of screen.getAllByRole('link', { name: label })) {
        expect(link).not.toHaveAttribute('aria-current')
      }
    }
    unmount()

    renderShell('/recordings')
    await findToggle()

    expect(getMoreTrigger()).not.toHaveAttribute('aria-current')
  })
})

/**
 * サーバーの `live.enabled` が false のとき、ライブへの導線を出さない
 * （issue #209）。出すとルートが登録されておらず、プレイリストが 404 になる
 * だけの「機能しない導線」になる。
 *
 * **両方向を見る。** 「出ない」だけを見るテストは、能力 API の解決を待たずに
 * 読んでいても通ってしまう（`live: true` でも初回は出ていない）。有効側で
 * 「待てば出る」ことを固定して初めて、無効側の「出ない」が主張になる。
 */
describe('ナビの出し分け（live.enabled）', () => {
  /** 無効側の「出ない」を、能力 API の解決を待った後で読むための待機点。 */
  async function waitForNavSettled() {
    await findToggle()
    // 「その他」は常に出る（畳まれる項目が検索・ルールだけになっても消えない）。
    // これが出た後にもう一度マイクロタスクを回して、能力 API 由来の再描画が
    // 起きるだけの猶予を作る
    expect(getMoreTrigger()).toBeInTheDocument()
    await screen.findByRole('link', { name: 'ルール' })
  }

  it('live.enabled が false ならサイドバーから「ライブ」が消える', async () => {
    renderShell('/', { live: false })
    await waitForNavSettled()

    expect(screen.queryByRole('link', { name: 'ライブ' })).not.toBeInTheDocument()

    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const sidebarNav = navs.find((nav) => within(nav).queryAllByRole('link').length === 5)
    expect(sidebarNav).toBeDefined()
    const labels = within(sidebarNav as HTMLElement)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(['番組', '録画', '予約', '検索', 'ルール'])
  })

  it('live.enabled が false でも「その他」の中身からライブだけが消える', async () => {
    const user = userEvent.setup()
    renderShell('/', { live: false })
    await waitForNavSettled()

    await user.click(getMoreTrigger())
    const menu = await screen.findByRole('dialog', { name: 'その他のナビゲーション' })
    const labels = within(menu)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(['検索', 'ルール'])
  })

  it('live.enabled が false でもボトムタブは 4 個（常時 3 個 + その他）のまま', async () => {
    renderShell('/', { live: false })
    await waitForNavSettled()

    const navs = screen.getAllByRole('navigation', { name: '主ナビゲーション' })
    const bottomNav = navs.find((nav) => within(nav).queryByRole('button', { name: 'その他' }))
    expect(bottomNav).toBeDefined()
    expect(within(bottomNav as HTMLElement).getAllByRole('listitem')).toHaveLength(4)
    const labels = within(bottomNav as HTMLElement)
      .getAllByRole('link')
      .map((el) => el.textContent)
    expect(labels).toEqual(MOBILE_PRIMARY_LABELS)
  })

  it('live.enabled が true なら「ライブ」がサイドバーと「その他」に出る（両方向）', async () => {
    renderShell('/', { live: true })
    await findToggle()

    // サイドバーとボトムタブの「その他」で 2 箇所（サイドバー 1 + ポップオーバーは
    // 閉じているので 0）。少なくとも 1 つ出ることを待って確認する
    const links = await findLiveLinks()
    expect(links.length).toBeGreaterThanOrEqual(1)
  })

  it('能力 API が失敗したらライブは出さない（未確定を「有効」に倒さない）', async () => {
    // live: true を返すはずの応答が 500 で落ちる構成。ここでライブを出すと、
    // サーバーが無効なデプロイでも「導線が出てから消える」窓ができる
    renderShell('/', { live: true, capabilitiesStatus: 500 })
    await waitForNavSettled()

    expect(screen.queryByRole('link', { name: 'ライブ' })).not.toBeInTheDocument()
  })
})
