import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ProgramListItem, Service } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

function service(overrides: Partial<Service>): Service {
  return {
    networkId: 1,
    serviceId: 1,
    name: 'サービス',
    channelType: 'GR',
    channel: '1',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
    ...overrides,
  }
}

function program(overrides: Partial<ProgramListItem>): ProgramListItem {
  return {
    programId: 1,
    networkId: 1,
    serviceId: 1,
    eventId: 1,
    startAt: new Date(Date.now() - 60_000).toISOString(),
    endAt: new Date(Date.now() + 3600_000).toISOString(),
    durationMs: 3660_000,
    name: '番組',
    description: '',
    genres: [],
    isFree: true,
    ...overrides,
  }
}

/**
 * renderLive は実際の routeTree（`@/routes`）を使って `/live` を開く。
 *
 * `useSearch({ from: '/live' })` は routes.tsx が登録した `validateSearch` に
 * 依存するため、`test/router.tsx` の `renderInRouter`（validateSearch を持たない
 * 汎用の 1 ルート）ではなく、`routes.test.tsx` の `/search` テストと同じ流儀で
 * 本物のルートツリーを使う。
 */
function renderLive(initialEntry = '/live') {
  window.scrollTo = vi.fn()

  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  })
  return render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })}
    >
      <ToastProvider>
        {/* 型はアプリ本体（main.tsx）の router 登録で付くため、ここでは構造だけ見る */}
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

/** stubFetch は pathname ごとに応答を振り分ける（routes.test.tsx と同じ形）。 */
function stubFetch(options: { services?: Service[]; programsByServiceId?: Record<number, ProgramListItem[]> }) {
  const { services = [], programsByServiceId = {} } = options
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')

    // ライブ視聴の HLS プレイリストは OpenAPI 対象外の別経路。この画面の
    // テストではプレイヤー本体を検証しない（components/live-player.test.tsx が
    // 状態遷移を担う）ので、streamer 不在（unreachable）に落として hls.js の
    // 動的 import を誘発しない
    if (url.pathname.includes('/live/playlist.m3u8')) {
      return Promise.reject(new TypeError('Failed to fetch'))
    }

    if (url.pathname === '/api/breakers') {
      return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }))
    }
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') {
      return Promise.resolve(new Response(JSON.stringify(['default']), { status: 200 }))
    }
    if (url.pathname === '/api/sites/default/services') {
      return Promise.resolve(new Response(JSON.stringify(services), { status: 200 }))
    }
    if (url.pathname === '/api/sites/default/programs') {
      const serviceIdParam = url.searchParams.get('serviceId')
      const serviceId = serviceIdParam !== null ? Number(serviceIdParam) : undefined
      const list = serviceId !== undefined ? programsByServiceId[serviceId] ?? [] : []
      return Promise.resolve(new Response(JSON.stringify(list), { status: 200 }))
    }
    return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }))
  }) as unknown as typeof fetch
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('LivePage', () => {
  it('チャンネル一覧が無ければ空状態を出す', async () => {
    stubFetch({ services: [] })
    renderLive()

    expect(await screen.findByText('チャンネルがありません')).toBeInTheDocument()
  })

  it('チャンネル一覧の取得に失敗したらエラー状態を出す', async () => {
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/sites') {
        return Promise.resolve(new Response(JSON.stringify(['default']), { status: 200 }))
      }
      if (url.pathname === '/api/sites/default/services') {
        return Promise.resolve(new Response('boom', { status: 500 }))
      }
      return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }))
    }) as unknown as typeof fetch
    renderLive()

    expect(await screen.findByText('チャンネル一覧の取得に失敗しました')).toBeInTheDocument()
  })

  it('serviceId 未指定では番組を持つ先頭チャンネルが選ばれ、いま放送中の番組が出る', async () => {
    stubFetch({
      services: [
        service({ serviceId: 1, name: 'サブサービス', hasPrograms: false, remoteControlKeyId: 1 }),
        service({ serviceId: 2, name: 'メインサービス', hasPrograms: true, remoteControlKeyId: 2 }),
      ],
      programsByServiceId: {
        2: [program({ serviceId: 2, name: '放送中の番組' })],
      },
    })
    renderLive()

    // 番組を持つ先頭（サービス 2）が選ばれる
    expect(await screen.findByText('放送中の番組')).toBeInTheDocument()
    // チャンネル一覧（`nav[aria-label="チャンネル一覧"]`）の中だけを見る ---
    // AppShell の主ナビゲーションも「ライブ」の項目に `aria-current="page"` を
    // 付けている（いま /live を開いているため）ので、document 全体から探すと
    // 別の意味の "current" と衝突する
    const channelNav = screen.getByRole('navigation', { name: 'チャンネル一覧' })
    const currentLinks = within(channelNav)
      .getAllByRole('link')
      .filter((el) => el.getAttribute('aria-current') === 'page')
    expect(currentLinks).toHaveLength(1)
    expect(currentLinks[0]).toHaveTextContent('メインサービス')
  })

  it('?serviceId= で指定したチャンネルを選ぶ', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive('/live?serviceId=20')

    expect(await screen.findByText('B の番組')).toBeInTheDocument()
  })

  it('存在しない serviceId を指定すると番組を持つ先頭にフォールバックする', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
      },
    })
    renderLive('/live?serviceId=999')

    expect(await screen.findByText('A の番組')).toBeInTheDocument()
  })

  it('いま放送中の番組が無いときは代わりの文言を出す', async () => {
    stubFetch({ services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    renderLive()

    expect(await screen.findByText('いま放送中の番組の情報はありません')).toBeInTheDocument()
  })

  it('チャンネル一覧の別チャンネルを押すと選択が切り替わる', async () => {
    const user = userEvent.setup()
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive()

    await screen.findByText('A の番組')

    await user.click(screen.getByRole('link', { name: /チャンネル B/ }))

    await waitFor(async () => {
      expect(await screen.findByText('B の番組')).toBeInTheDocument()
    })
    expect(screen.queryByText('A の番組')).not.toBeInTheDocument()
  })

  it('ハイライトは即座に切り替わるが、実際のナビゲーションはデバウンスする', async () => {
    const user = userEvent.setup()
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive()
    await screen.findByText('A の番組')

    await user.click(screen.getByRole('link', { name: /チャンネル B/ }))

    // ハイライトは即座に B へ移る
    expect(screen.getByRole('link', { name: /チャンネル B/ })).toHaveAttribute(
      'aria-current',
      'page',
    )
    // が、デバウンス（400ms）中はまだ A を見ている（probe / セッションを
    // まだ起こしていない）
    expect(screen.getByText('A の番組')).toBeInTheDocument()

    await waitFor(() => expect(screen.getByText('B の番組')).toBeInTheDocument())
  })

  it('デバウンス中に別チャンネルへ切り替えると、通り過ぎたチャンネルへは一度も遷移しない', async () => {
    const user = userEvent.setup()
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
        service({ serviceId: 30, name: 'チャンネル C' }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
        20: [program({ serviceId: 20, name: 'B の番組' })],
        30: [program({ serviceId: 30, name: 'C の番組' })],
      },
    })
    renderLive()
    await screen.findByText('A の番組')

    // B → C とデバウンス幅（400ms）内に連続でザップする
    await user.click(screen.getByRole('link', { name: /チャンネル B/ }))
    await user.click(screen.getByRole('link', { name: /チャンネル C/ }))

    await waitFor(() => expect(screen.getByText('C の番組')).toBeInTheDocument())

    // B の「いま放送中」問い合わせが一度も発生していない
    // （= B 向けの LivePlayer / probe も一度も起きていない）ことを、
    // fetch 呼び出しの実績から確認する
    const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
    const queriedServiceIds = calls
      .map(([url]) => new URL(url, 'http://localhost'))
      .filter((u) => u.pathname === '/api/sites/default/programs')
      .map((u) => u.searchParams.get('serviceId'))
    expect(queriedServiceIds).not.toContain('20')
    expect(queriedServiceIds).toContain('30')
  })
})
