import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ProgramListItem, Reservation, Service } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

function service(overrides: Partial<Service>): Service {
  const networkId = overrides.networkId ?? 1
  const serviceId = overrides.serviceId ?? 1
  return {
    id: networkId * 100_000 + serviceId,
    networkId,
    serviceId,
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

function reservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    id: 1,
    site: 'default',
    programId: 1,
    source: 'rule',
    state: 'active',
    title: '予約',
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: new Date(Date.now() + 30 * 60_000).toISOString(),
    durationMs: 3600_000,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    skip: false,
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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })
  const result = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        {/* 型はアプリ本体（main.tsx）の router 登録で付くため、ここでは構造だけ見る */}
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { ...result, queryClient }
}

/**
 * interruptionSettled は中断予測（issue #235）が使う予約クエリの解決を待つ
 * （`pages/reservations.test.tsx` の `overagesSettled` と同じ考え方）。
 *
 * `queryClient.isFetching() === 0` だけでは「まだクエリが始まっていない瞬間」を
 * 「解決済み」と読んで通ってしまう（CLAUDE.md「非同期の空虚な成功」）ため、
 * `/api/reservations` が少なくとも 1 回 success になったことも要求する。
 * 「警告が出ない」ことの確認はこれを通してから行う。
 */
async function interruptionSettled(queryClient: QueryClient): Promise<void> {
  await waitFor(() => {
    expect(queryClient.isFetching()).toBe(0)
    const statuses = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['/api/reservations'] })
      .map((query) => query.state.status)
    expect(statuses).toContain('success')
  })
}

/** stubFetch は pathname ごとに応答を振り分ける（routes.test.tsx と同じ形）。 */
function stubFetch(options: {
  services?: Service[]
  programsByServiceId?: Record<number, ProgramListItem[]>
  /** サーバーの `live.enabled`（`GET /api/capabilities`）。既定は有効。 */
  live?: boolean
  /** 能力 API のステータス。200 以外なら「有効か無効か分からない」状態になる。 */
  capabilitiesStatus?: number
  /** `GET /api/reservations`（全サイト分）。既定は空 --- 中断予測（issue #235）用。 */
  reservations?: Reservation[]
}) {
  const {
    services = [],
    programsByServiceId = {},
    live = true,
    capabilitiesStatus = 200,
    reservations = [],
  } = options
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')

    if (url.pathname === '/api/reservations') {
      return Promise.resolve(new Response(JSON.stringify(reservations), { status: 200 }))
    }

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
    // 動的 import を誘発しない。**この経路への fetch が実際に呼ばれたかどうか
    // 自体は、選択と再生の分離（issue #234 M7-1）の判定に使う**
    // ---「再生」ボタンを押すまで一度も呼ばれないことを見る
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
      // `?service=<Service.id>` は複数指定可（orval のクエリシリアライズは
      // 同名パラメータの繰り返し）。サーバーは合成 id で厳密に絞るので、
      // ここでも同じ規則で絞る --- serviceId だけで拾うと、同じ id を持つ
      // 別 network の番組が混ざるフィクスチャ（issue #291）で実物と食い違う。
      const ids = url.searchParams.getAll('service').map(Number)
      const list = ids.flatMap((id) =>
        (programsByServiceId[id % 100_000] ?? []).filter(
          (p) => p.networkId === Math.floor(id / 100_000),
        ),
      )
      return Promise.resolve(new Response(JSON.stringify(list), { status: 200 }))
    }
    return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }))
  }) as unknown as typeof fetch
}

/** playlistFetchCalled は stubFetch 下で `/live/playlist.m3u8` への fetch が実際に呼ばれたか。 */
function playlistFetchCalled(): boolean {
  const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
  return calls.some(([url]) => String(url).includes('/live/playlist.m3u8'))
}

/** playlistFetchCallCount は `/live/playlist.m3u8` への fetch 呼び出し回数（累積）。 */
function playlistFetchCallCount(): number {
  const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
  return calls.filter(([url]) => String(url).includes('/live/playlist.m3u8')).length
}

/**
 * leaveHintURLs は離脱ヒント（issue #191）が飛んだ宛先の一覧。
 *
 * jsdom には `navigator.sendBeacon` が無いので、`sendLiveLeaveHint` は
 * `keepalive` つきの POST にフォールバックする --- つまりこの fetch モックに
 * 現れる（beacon 経路そのものは `components/live-player.test.tsx` が
 * 差し替えた `sendBeacon` で、実ブラウザでの到達は `web/e2e/live.mjs` ⑧が見る）。
 */
function leaveHintURLs(): string[] {
  const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
  return calls.map(([url]) => String(url)).filter((url) => url.includes('/live/leave'))
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

    expect(playlistFetchCalled()).toBe(false)
  })

  it('有効ならチャンネル一覧と選択画面（再生ボタン）が出る（両方向）', async () => {
    stubFetch({ live: true, services: [service({ serviceId: 1, name: 'チャンネル A' })] })
    renderLive()

    expect(await screen.findByRole('navigation', { name: 'チャンネル一覧' })).toBeInTheDocument()
    expect(screen.queryByText('この環境ではライブ視聴が無効です')).not.toBeInTheDocument()
    // 選択状態（再生ボタン）で止まる --- 有効なだけでは probe しない
    expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()
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

  it('?service=<Service.id> で指定したチャンネルを選ぶ', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive('/live?service=100020')

    expect(await screen.findByText('B の番組')).toBeInTheDocument()
  })

  it('視聴中チャンネルの「この局の番組表」は ?service=<Service.id> で厳密な一局へ遷移する', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive('/live?service=100020')

    expect(await screen.findByText('B の番組')).toBeInTheDocument()

    const link = screen.getByRole('link', { name: 'この局の番組表' })
    // 1 局の導線も番組表と同じ `Service.id` で表す。配列は既定の
    // シリアライズ（JSON.stringify）で 1 パラメータに載る。
    expect(link).toHaveAttribute('href', '/programs?service=%5B100020%5D')
  })

  it('一覧に無い service（未知の id）を指定すると番組を持つ先頭にフォールバックする', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A' }),
        service({ serviceId: 20, name: 'チャンネル B' }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
      },
    })
    renderLive('/live?service=999999')

    expect(await screen.findByText('A の番組')).toBeInTheDocument()
  })

  /**
   * issue #438: 旧 `?networkId=&serviceId=`（SI 2 値）の後方互換は持たない
   * （`5ab06f8` と同じ判断）。`LivePageSearch` はそのキーを持たないので
   * `service` は常に undefined になり、`pickInitialService` の既定（番組を
   * 持つ先頭）へ落ちる --- 旧リンクが指していたチャンネル（A）ではなく、
   * 番組を持つ先頭（B）が選ばれることで無視されたことを固定する。
   */
  it('旧 ?networkId=&serviceId= の直開きは無視され、既定（番組を持つ先頭）サービスが選ばれる', async () => {
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A', hasPrograms: false }),
        service({ serviceId: 20, name: 'チャンネル B', hasPrograms: true }),
      ],
      programsByServiceId: {
        10: [program({ serviceId: 10, name: 'A の番組' })],
        20: [program({ serviceId: 20, name: 'B の番組' })],
      },
    })
    renderLive('/live?networkId=1&serviceId=10')

    expect(await screen.findByText('B の番組')).toBeInTheDocument()
    expect(screen.queryByText('A の番組')).not.toBeInTheDocument()
  })

  /**
   * issue #291: SI の `serviceId` は network をまたぐと一意でない（Mirakurun が
   * `networkId * 100000 + serviceId` の合成 id を発明した理由そのもの）。
   * `GET /api/sites/{site}/services` が GR/BS 混在で同じ `serviceId` を持つ
   * サービスを 2 つ返す構成のフィクスチャで、`?service=<Service.id>` を指定した
   * ときに正しい network のチャンネルが選ばれることを固定する（issue #438 で
   * `?networkId=&serviceId=` から統一した後も、合成 id 自体が network をまたいで
   * 一意なのでこの区別は自動的に効く）。
   */
  describe('同じ serviceId を持つサービスが複数 network に存在するとき（issue #291）', () => {
    function crossNetworkServices() {
      return [
        service({ networkId: 1, serviceId: 100, name: 'GR の局', channelType: 'GR' }),
        service({ networkId: 2, serviceId: 100, name: 'BS の局', channelType: 'BS' }),
      ]
    }
    function crossNetworkPrograms() {
      return {
        100: [
          program({ networkId: 1, serviceId: 100, name: 'GR の番組' }),
          program({ networkId: 2, serviceId: 100, name: 'BS の番組' }),
        ],
      }
    }

    it('?service=<Service.id> を指定すると、その network のチャンネルだけが選ばれる（network 1 側）', async () => {
      const services = crossNetworkServices()
      stubFetch({ services, programsByServiceId: crossNetworkPrograms() })
      renderLive(`/live?service=${services[0].id}`)

      expect(await screen.findByText('GR の番組')).toBeInTheDocument()
      expect(screen.queryByText('BS の番組')).not.toBeInTheDocument()
      const nav = screen.getByRole('navigation', { name: 'チャンネル一覧' })
      const current = within(nav)
        .getAllByRole('link')
        .filter((el) => el.getAttribute('aria-current') === 'page')
      expect(current).toHaveLength(1)
      expect(current[0]).toHaveTextContent('GR の局')
    })

    it('?service=<Service.id> を指定すると、その network のチャンネルだけが選ばれる（network 2 側）', async () => {
      const services = crossNetworkServices()
      stubFetch({ services, programsByServiceId: crossNetworkPrograms() })
      renderLive(`/live?service=${services[1].id}`)

      expect(await screen.findByText('BS の番組')).toBeInTheDocument()
      expect(screen.queryByText('GR の番組')).not.toBeInTheDocument()
      const nav = screen.getByRole('navigation', { name: 'チャンネル一覧' })
      const current = within(nav)
        .getAllByRole('link')
        .filter((el) => el.getAttribute('aria-current') === 'page')
      expect(current).toHaveLength(1)
      expect(current[0]).toHaveTextContent('BS の局')
      // `web/e2e/live.mjs` ⓪ は「選択中リンクの href に要求した id が載っている」
      // ことで実ブラウザで効いたと判定する。その前提をここでも固定しておく ---
      // 載らなくなれば e2e は落ちるが、それはブラウザとサーバーを用意しないと
      // 分からない
      const href = current[0].getAttribute('href') ?? ''
      expect(href).toContain(`service=${services[1].id}`)
    })

    it('「再生」を押すと、選んだ network の (networkId, serviceId) でプレイリストを取りに行く', async () => {
      const user = userEvent.setup()
      const services = crossNetworkServices()
      stubFetch({ services, programsByServiceId: crossNetworkPrograms() })
      renderLive(`/live?service=${services[1].id}`)
      await screen.findByText('BS の番組')

      await user.click(screen.getByRole('button', { name: /再生/ }))

      await waitFor(() => expect(playlistFetchCalled()).toBe(true))
      const calls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls
      const playlistCall = calls.find(([url]) => String(url).includes('/live/playlist.m3u8'))
      expect(playlistCall?.[0]).toContain('/networks/2/services/100/')
    })

    /**
     * `playingKey`（再生状態の同定）も `Service.id`（`networkId` と `serviceId`
     * の合成）で判定する（`pages/live.tsx` の `playingKey` 定義部のコメント）。
     * `serviceId` 単独で判定すると、GR（network 1）を再生中に同じ `serviceId` の
     * BS（network 2）へ切り替えたとき「同じチャンネル」と誤認し、GR 向けの
     * `LivePlayer`（接続できません、のエラー表示）を押していない BS へそのまま
     * 引き継いでしまう --- これは選択（`aria-current`）とは別の判定なので、
     * `aria-current` 側だけを固定した上のテストでは捕まらない。
     */
    it('再生中に同じ serviceId の別 network へ切り替えると選択状態に戻る', async () => {
      const user = userEvent.setup()
      const services = crossNetworkServices()
      stubFetch({ services, programsByServiceId: crossNetworkPrograms() })
      renderLive(`/live?service=${services[0].id}`)
      await screen.findByText('GR の番組')

      await user.click(screen.getByRole('button', { name: /再生/ }))
      await screen.findByText(/接続できません/)
      const callsAfterGr = playlistFetchCallCount()
      expect(callsAfterGr).toBeGreaterThan(0)

      await user.click(screen.getByRole('link', { name: /BS の局/ }))
      await waitFor(() => expect(screen.getByText('BS の番組')).toBeInTheDocument())

      // BS へ切り替わったら選択状態（再生ボタン）に戻り、GR のエラー表示は消える
      expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()
      expect(screen.queryByText(/接続できません/)).not.toBeInTheDocument()
      // 押していない BS の playlist を一度も取りに行かない（引き継いでいれば
      // GR のときと同様に probe が飛び、この件数が増える）
      expect(playlistFetchCallCount()).toBe(callsAfterGr)
    })
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

  it('選択中チャンネルのチャンネル種別（GR/BS/CS）を表示する（issue #234 の含むもの 1）', async () => {
    stubFetch({
      services: [service({ serviceId: 1, name: 'チャンネル A', channelType: 'BS' })],
    })
    renderLive()

    // `screen.findByText('チャンネル A')` は情報欄（`<p>`）とチャンネル一覧の
    // 両方に一致して例外になるため、より具体的な「再生」ボタン（情報欄の直上に
    // 出る）を目印に読み込み完了を待つ
    await screen.findByRole('button', { name: /チャンネル Aを再生/ })
    // 情報欄（`.font-medium` を持つ p 要素と同じブロック）に種別バッジが出る。
    // チャンネル一覧側の見出し（`groupByChannelType` の小見出し）にも同じ文字列
    // "BS" が出るため、`getAllByText` で件数だけを見る（両方合わせて 2 件になる
    // ---見出し 1 + 情報欄バッジ 1）
    const matches = screen.getAllByText('BS')
    expect(matches).toHaveLength(2)
    // 情報欄バッジは <span>（見出しは <p>）。text-muted-foreground だと
    // bg-muted との合成後コントラストがライトで 4.5 を割る（issue #308）。
    // jsdom は色を測れないので、退行防止としてはクラス名のリテラル比較まで
    // （実測は e2e:design の担当）。
    const badge = matches.find((el) => el.tagName === 'SPAN')
    expect(badge).toBeDefined()
    expect(badge!.className).toContain('text-foreground')
    expect(badge!.className).not.toContain('text-muted-foreground')
  })

  it('チャンネル一覧のリモコン番号タグは text-foreground（issue #308。text-muted-foreground だと bg-muted との合成後コントラストがライトで 4.5 を割る）', async () => {
    stubFetch({
      services: [service({ serviceId: 1, name: 'チャンネル A', remoteControlKeyId: 3 })],
    })
    renderLive()

    const link = await screen.findByRole('link', { name: /チャンネル A/ })
    // jsdom は色を測れないので、退行防止としてはクラス名のリテラル比較まで
    // （実測は e2e:design の担当）。
    const badge = within(link).getByText('3')
    expect(badge.className).toContain('text-foreground')
    expect(badge.className).not.toContain('text-muted-foreground')
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

  /**
   * デバウンスは M7-1（issue #234）で削除した --- 以前は「通り過ぎたチャンネル」の
   * probe / セッションを起こさないための緩和として 400ms 挟んでいたが、選択自体が
   * probe もセッションも起こさなくなった今、デバウンスする対象が消えた。ここでは
   * 「クリック直後に URL 側の選択（`aria-current`）が即座に切り替わる」ことで
   * デバウンスが再導入されていないことを確認する。
   */
  it('チャンネル切り替えはデバウンスせず即座にナビゲートする（issue #234 の含むもの 4）', async () => {
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

    // デバウンスを待たずに、URL 側の選択が既に B へ切り替わっている
    expect(screen.getByRole('link', { name: /チャンネル B/ })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })

  it(
    'チャンネルを選んでもプレイリストを取りに行かず、再生ボタンを押して初めて取りに行く' +
      '（issue #234 の受け入れ「チャンネルタップではプレイリスト要求が飛ばない」）',
    async () => {
      const user = userEvent.setup()
      stubFetch({
        services: [service({ serviceId: 10, name: 'チャンネル A' })],
        programsByServiceId: { 10: [program({ serviceId: 10, name: 'A の番組' })] },
      })
      renderLive()

      expect(await screen.findByText('A の番組')).toBeInTheDocument()
      // 選択（チャンネル一覧の描画・now/next の取得）だけでは probe しない
      expect(playlistFetchCalled()).toBe(false)
      // プレイヤー本体（読み込み中の表示）はまだ無く、「再生」ボタンだけがある
      expect(screen.queryByText('読み込み中…')).not.toBeInTheDocument()
      expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()

      await user.click(screen.getByRole('button', { name: /再生/ }))

      await waitFor(() => expect(playlistFetchCalled()).toBe(true))
      // 再生ボタンは消え、プレイヤー本体に置き換わる
      expect(screen.queryByRole('button', { name: /再生/ })).not.toBeInTheDocument()
    },
  )

  it('?service= の直開きでも選択状態で止まり、再生ボタンを押すまでプレイリストを取りに行かない（issue #234 の含むもの 3）', async () => {
    stubFetch({
      services: [service({ serviceId: 10, name: 'チャンネル A' })],
      programsByServiceId: { 10: [program({ serviceId: 10, name: 'A の番組' })] },
    })
    renderLive('/live?service=100010')

    expect(await screen.findByText('A の番組')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()
    expect(playlistFetchCalled()).toBe(false)
  })

  it('再生中に別チャンネルへ切り替えると選択状態に戻る（同意はチャンネルごとに必要）', async () => {
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
    renderLive('/live?service=100010')
    await screen.findByText('A の番組')

    await user.click(screen.getByRole('button', { name: /再生/ }))
    // LivePlayer がマウントされたことを、probe 失敗（stubFetch が
    // playlist.m3u8 を reject する）後の終端状態で確認する。「読み込み中…」は
    // reject が即座に解決すると一度も観測されない瞬間的な状態なので、判定には
    // 使わない（テスト規律「非同期の空虚な成功に注意する」の逆 --- 早すぎて
    // 見えない状態を待つと flaky になる）
    await screen.findByText(/接続できません/)
    expect(screen.queryByRole('button', { name: /再生/ })).not.toBeInTheDocument()

    // A の再生で飛んだ playlist 要求の件数を基準値にする（0 件ではないことも確認 ---
    // 基準値が既に壊れていたら、以降の「増えていない」判定が何も守らなくなる）
    const playlistCallsAfterA = playlistFetchCallCount()
    expect(playlistCallsAfterA).toBeGreaterThan(0)

    await user.click(screen.getByRole('link', { name: /チャンネル B/ }))

    await waitFor(() => expect(screen.getByText('B の番組')).toBeInTheDocument())
    // B に切り替わったら選択状態（再生ボタン）に戻り、プレイヤー（A のエラー表示）は消える
    expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()
    expect(screen.queryByText(/接続できません/)).not.toBeInTheDocument()
    // **押していない B の playlist を一度も取りに行かない。** レビューでの指摘:
    // 再生状態のリセットを `useEffect` で行うと、`selectedServiceId` が A→B に
    // 変わった直後の 1 コミットだけ古い再生中フラグが残っていて `LivePlayer` が
    // B の serviceId で透過的にマウントされ、その 1 回の probe が実際に飛ぶ
    // （実測: jsdom でもこの fetch モックに `/services/{B の serviceId}/live/
    // playlist.m3u8` への呼び出しが記録される）。判定はレンダー中の調整で防ぐ
    // （`pages/live.tsx` の `playingKey` 参照）。件数が増えていなければ、
    // B 向けの LivePlayer が一度もマウントされなかったと言える
    expect(playlistFetchCallCount()).toBe(playlistCallsAfterA)
  })

  it('再生中に別チャンネルへ切り替えると、離れた側に離脱ヒントが飛ぶ（issue #191）', async () => {
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
    renderLive('/live?service=100010')
    await screen.findByText('A の番組')

    await user.click(screen.getByRole('button', { name: /再生/ }))
    // 再生（LivePlayer のマウント）が実際に起きたことを待ってから判定する ---
    // 起きていなければ「ヒントが飛ばない」ではなく「見てすらいない」になり、
    // 以降の assertion が空虚になる
    await screen.findByText(/接続できません/)
    expect(leaveHintURLs()).toEqual([])

    await user.click(screen.getByRole('link', { name: /チャンネル B/ }))
    await waitFor(() => expect(screen.getByText('B の番組')).toBeInTheDocument())

    // 離れた側（A = networkId 1 / serviceId 10）にだけ飛ぶ。B（serviceId 20）へ
    // 飛ばしてはならない --- そちらは「これから見るかもしれない」チャンネルである
    expect(leaveHintURLs()).toEqual(['/api/sites/default/networks/1/services/10/live/leave'])
  })

  it(
    '再生中に別チャンネルへ切り替えてから元のチャンネルへ戻ると、再度「再生」を押すまで' +
      'プレイヤーが出ない（selectedServiceId が変わるたびに再生状態を落とす reset effect の検証）',
    async () => {
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
      renderLive('/live?service=100010')
      await screen.findByText('A の番組')

      await user.click(screen.getByRole('button', { name: /再生/ }))
      await screen.findByText(/接続できません/)

      await user.click(screen.getByRole('link', { name: /チャンネル B/ }))
      await waitFor(() => expect(screen.getByText('B の番組')).toBeInTheDocument())

      await user.click(screen.getByRole('link', { name: /チャンネル A/ }))
      await waitFor(() => expect(screen.getByText('A の番組')).toBeInTheDocument())

      // A へ戻っても以前の「再生」は引き継がれない --- 選択状態（再生ボタン）で止まる
      expect(screen.getByRole('button', { name: /再生/ })).toBeInTheDocument()
      expect(screen.queryByText(/接続できません/)).not.toBeInTheDocument()
    },
  )
})

describe('LivePage / 録画予約による中断予測（issue #235 M7-2）', () => {
  it('同じチャンネル種別の近い録画予約があるとき、値札（選択状態）にも視聴中画面にも警告が出る', async () => {
    const user = userEvent.setup()
    const startAt = new Date(Date.now() + 30 * 60_000).toISOString()
    stubFetch({
      services: [service({ serviceId: 10, name: 'チャンネル A', channelType: 'GR' })],
      programsByServiceId: {
        10: [
          program({
            programId: 5,
            serviceId: 10,
            startAt,
            endAt: new Date(Date.now() + 90 * 60_000).toISOString(),
          }),
        ],
      },
      reservations: [reservation({ programId: 5, site: 'default', startAt, skip: false })],
    })
    renderLive()

    // 選択状態（値札）
    await screen.findByRole('button', { name: /再生/ })
    expect(await screen.findByText(/から録画予約があります/)).toBeInTheDocument()
    // 「不足すると中断されます」という条件付きの文言（断言しない。issue #235 の「罠」）
    expect(screen.getByText(/チューナーが不足すると視聴は中断されます/)).toBeInTheDocument()

    // 視聴中の画面でも同じ情報欄が出る（LivePlayer / LiveSelectionPreview の外の
    // 共通ブロックに置いたことの検証。`pages/live.tsx` の配置コメント参照）
    await user.click(screen.getByRole('button', { name: /再生/ }))
    await screen.findByText(/接続できません/)
    expect(screen.getByText(/から録画予約があります/)).toBeInTheDocument()
  })

  /**
   * `nowPlayingRefetchMs`（30 秒）の tick を跨いでも警告が消えないことを見る。
   *
   * 判定は `reservation.channelType` と選択中サービスの `channelType` の直接
   * 比較（issue #440）で、EPG への第 2 クエリを経由しない。以前は同種別全
   * サービス × 2 時間の EPG を別クエリで引いて programId 集合を作っており、
   * tick のたびにこのクエリのキーが割れて警告が一時的に消えていた（実測: jsdom
   * で 30038ms 後・実 Chromium で 28258ms 後に消失。10 分グリッド丸めで
   * 抑えていた）。第 2 クエリ自体が無くなったので、判定に使う値
   * （`reservations` / `selectedService.channelType`）は tick で変わらない
   * --- 実時間で 32 秒ポーリングし続けて確かめる必要はもう無い（レビューでの
   * 指摘。消えた失敗モードのために時間を買わない）。
   *
   * `channelType` 比較を無効化する変異（`reservation.channelType !==
   * channelType` に反転する）を当てると `findByText` の時点で落ちることを
   * 確認済み。
   */
  it('予約クエリが解決し切った後も警告が残る（tick で消えない）', async () => {
    const startAt = new Date(Date.now() + 30 * 60_000).toISOString()
    stubFetch({
      services: [service({ serviceId: 10, name: 'チャンネル A', channelType: 'GR' })],
      programsByServiceId: {
        10: [
          program({
            programId: 5,
            serviceId: 10,
            startAt,
            endAt: new Date(Date.now() + 90 * 60_000).toISOString(),
          }),
        ],
      },
      reservations: [
        reservation({ programId: 5, site: 'default', startAt, skip: false, channelType: 'GR' }),
      ],
    })

    const { queryClient } = renderLive()

    expect(await screen.findByText(/から録画予約があります/)).toBeInTheDocument()
    await interruptionSettled(queryClient)
    expect(screen.getByText(/から録画予約があります/)).toBeInTheDocument()
  })

  it('近い録画予約が無いときは何も出さない（沈黙。「安全に見られます」等の肯定文言は無い）', async () => {
    stubFetch({ services: [service({ serviceId: 1, name: 'チャンネル A', channelType: 'GR' })] })
    const { queryClient } = renderLive()

    await screen.findByRole('button', { name: /再生/ })
    // 問い合わせが解決し切るまで待ってから確かめる（非同期の空虚な成功を避ける。
    // `interruptionSettled` 参照）
    await interruptionSettled(queryClient)
    expect(screen.queryByText(/録画予約/)).not.toBeInTheDocument()
  })

  it('skip の予約では警告が出ない（サーバーの需要計算と同じ除外規則）', async () => {
    const startAt = new Date(Date.now() + 30 * 60_000).toISOString()
    stubFetch({
      services: [service({ serviceId: 10, name: 'チャンネル A', channelType: 'GR' })],
      programsByServiceId: {
        10: [
          program({
            programId: 5,
            serviceId: 10,
            startAt,
            endAt: new Date(Date.now() + 90 * 60_000).toISOString(),
          }),
        ],
      },
      reservations: [reservation({ programId: 5, site: 'default', startAt, skip: true })],
    })
    const { queryClient } = renderLive()

    await screen.findByRole('button', { name: /再生/ })
    await interruptionSettled(queryClient)
    expect(screen.queryByText(/録画予約/)).not.toBeInTheDocument()
  })

  it('別チャンネル種別の録画予約では警告が出ない', async () => {
    const startAt = new Date(Date.now() + 30 * 60_000).toISOString()
    stubFetch({
      services: [
        service({ serviceId: 10, name: 'チャンネル A', channelType: 'GR' }),
        service({ serviceId: 20, name: 'チャンネル B', channelType: 'BS' }),
      ],
      // 判定は reservation.channelType と選択中サービスの channelType の直接比較
      // なので（issue #440）、EPG 応答の中身は判定に関与しない --- 予約側の
      // channelType を選択中（GR）と違える（BS）だけで除外されることを見る
      reservations: [
        reservation({ programId: 6, site: 'default', startAt, skip: false, channelType: 'BS' }),
      ],
    })
    const { queryClient } = renderLive('/live?service=100010')

    await screen.findByRole('button', { name: /再生/ })
    await interruptionSettled(queryClient)
    expect(screen.queryByText(/録画予約/)).not.toBeInTheDocument()
  })
})
