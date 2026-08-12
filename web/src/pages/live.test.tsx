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
function stubFetch(options: {
  services?: Service[]
  programsByServiceId?: Record<number, ProgramListItem[]>
  /** サーバーの `live.enabled`（`GET /api/capabilities`）。既定は有効。 */
  live?: boolean
  /** 能力 API のステータス。200 以外なら「有効か無効か分からない」状態になる。 */
  capabilitiesStatus?: number
}) {
  const { services = [], programsByServiceId = {}, live = true, capabilitiesStatus = 200 } = options
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')

    // ライブへの導線・画面はサーバー側の live.enabled に連動する（issue #209）。
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(
        capabilitiesStatus === 200
          ? new Response(JSON.stringify({ live }), { status: 200 })
          : new Response('boom', { status: capabilitiesStatus }),
      )
    }

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

describe('LivePage / live.enabled が false のとき（issue #209）', () => {
  it('無効である旨と有効化の手がかりを出し、プレイヤーもチャンネル一覧も出さない', async () => {
    stubFetch({ live: false, services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    renderLive()

    expect(await screen.findByText('この環境ではライブ視聴が無効です')).toBeInTheDocument()
    // 原因（サーバー設定）に辿り着ける文言が要る。無効だと言うだけでは、
    // 運用者は live.enabled という設定の存在を知らないままになる
    expect(screen.getByText(/live\.enabled/)).toBeInTheDocument()
    // 空白の再生エラーにしない = プレイヤーもチャンネル一覧も出さない
    expect(screen.queryByRole('navigation', { name: 'チャンネル一覧' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /チャンネル A/ })).not.toBeInTheDocument()
  })

  it('プレイリストを一度も取りに行かない', async () => {
    stubFetch({ live: false, services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    renderLive()
    await screen.findByText('この環境ではライブ視聴が無効です')

    const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
    const playlistCalls = calls.filter(([url]) => String(url).includes('/live/playlist.m3u8'))
    expect(playlistCalls).toEqual([])
  })

  it('有効なら従来どおりチャンネル一覧とプレイヤーが出る（両方向）', async () => {
    stubFetch({ live: true, services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    renderLive()

    expect(await screen.findByRole('navigation', { name: 'チャンネル一覧' })).toBeInTheDocument()
    expect(screen.queryByText('この環境ではライブ視聴が無効です')).not.toBeInTheDocument()
  })

  /**
   * **能力 API が失敗したときに「無効です」と言ってはならない。**
   * 導線を出さない側に倒すのは無料（ナビは黙って消えるだけ）だが、画面の文言は
   * 原因の名指しなので、聞けなかっただけの状態で「サーバーの設定が無効」と書くと
   * `live.enabled: true` のデプロイで能力 API が瞬断しただけの運用者を誤った
   * 原因へ誘導する --- issue #209 が消したかった「原因にたどり着けない」の再演
   * （レビューでの指摘）。
   */
  it('能力 API が失敗したときは「無効」と断言せず、確認できないことを出す', async () => {
    stubFetch({
      live: true,
      capabilitiesStatus: 500,
      services: [service({ serviceId: 1, name: 'チャンネル A' })],
    })
    renderLive()

    expect(
      await screen.findByText('ライブ視聴が利用できるかを確認できませんでした'),
    ).toBeInTheDocument()
    expect(screen.queryByText('この環境ではライブ視聴が無効です')).not.toBeInTheDocument()
    expect(screen.queryByText(/live\.enabled/)).not.toBeInTheDocument()
  })

  it('能力 API の解決前は「無効」と断言しない', async () => {
    // 能力 API だけ永久に未解決にする（他は即座に返す）。1 フレーム目だけでなく
    // 「解決を待っている間ずっと」断言しないことを見る
    stubFetch({ live: true, services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    const settled = globalThis.fetch as unknown as (
      input: string | URL | Request,
    ) => Promise<Response>
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/capabilities') return new Promise<Response>(() => {})
      return settled(input)
    }) as unknown as typeof fetch

    renderLive()
    // 画面自体は描画される（ヘッダが出る）ところまで待ってから、断言が無いことと
    // チャンネル一覧も出ていないこと（= 読み込み中の表示）を見る
    await screen.findByRole('heading', { name: 'ライブ' })
    await vi.waitFor(() => {
      expect(screen.queryByText('この環境ではライブ視聴が無効です')).not.toBeInTheDocument()
      expect(screen.queryByRole('navigation', { name: 'チャンネル一覧' })).not.toBeInTheDocument()
    })
    // **`unknown` の文言も出してはならない。** 「まだ聞いていない」と「聞けなかった」を
    // 潰すと、有効なデプロイの初回読み込みごとに「確認できませんでした」が出る ---
    // disabled の誤断言と同じクラスの、起きていない失敗の主張（再レビューでの指摘。
    // この 1 行が無いと、pending を unknown の枝に潰す変異が 527 件すべて通る）
    expect(
      screen.queryByText('ライブ視聴が利用できるかを確認できませんでした'),
    ).not.toBeInTheDocument()
  })
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
      if (url.pathname === '/api/capabilities') {
        return Promise.resolve(new Response(JSON.stringify({ live: true }), { status: 200 }))
      }
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
    // ON AIR は「いま電波に乗っている」を示すバッジ（走査線は 3 箇所限定の 1 つ。
    // docs/frontend/design.md）。放送中の番組が無いときは出ない
    expect(screen.queryByText('ON AIR')).not.toBeInTheDocument()
  })

  it('いま放送中の番組があるときだけ ON AIR バッジ（走査線）を出す', async () => {
    stubFetch({
      services: [service({ serviceId: 1, name: 'チャンネル A' })],
      programsByServiceId: { 1: [program({ serviceId: 1, name: '放送中の番組' })] },
    })
    renderLive()

    const badge = await screen.findByText('ON AIR')
    expect(badge.className.split(' ')).toContain('tally-scanlines')
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
