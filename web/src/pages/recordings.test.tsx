import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import {
  useListRecordingDropStats,
  type DropStat,
  type EncodeProfileSummary,
  type Recording,
  type Rule,
  type Service,
} from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { DropStatsTable } from '@/pages/recordings'
import { routeTree } from '@/routes'

/**
 * Harness は DropStatsTable と同じクエリキーを共有する監視用の隣接要素を描画する。
 * 「種別が無い行に何も出ない」系の確認は、クエリが解決したあとの状態を見る必要が
 * あるため（解決前だと stats が空で通ってしまう）。
 */
function Harness({ recordingId }: { recordingId: number }) {
  const query = useListRecordingDropStats(recordingId)
  return (
    <>
      <div data-testid="query-status">{query.status}</div>
      <DropStatsTable recordingId={recordingId} />
    </>
  )
}

function renderTable(stats: DropStat[], recordingId = 7) {
  globalThis.fetch = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify(stats), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  ) as unknown as typeof fetch

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <Harness recordingId={recordingId} />
    </QueryClientProvider>,
  )
}

const stat = (pid: number, pidType?: string): DropStat => ({
  pid,
  packets: 100,
  drops: 0,
  errors: 0,
  scrambled: 0,
  ...(pidType === undefined ? {} : { pidType }),
})

describe('DropStatsTable', () => {
  it('PID 種別が日本語のラベルで出る', async () => {
    renderTable([stat(0x100, 'video'), stat(0x110, 'audio'), stat(0x0, 'pat')])

    expect(await screen.findByText('映像')).toBeInTheDocument()
    expect(screen.getByText('音声')).toBeInTheDocument()
    expect(screen.getByText('PAT')).toBeInTheDocument()
  })

  it('種別が無い PID は空欄扱いになり、PID 自体は出る', async () => {
    renderTable([stat(0x200)])

    await screen.findByText('success')
    expect(screen.getByText('0x0200')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.queryByText('映像')).not.toBeInTheDocument()
  })

  it('知らない種別はそのまま表示する（値の権威は Go 側）', async () => {
    renderTable([stat(0x300, 'ecm')])

    expect(await screen.findByText('ecm')).toBeInTheDocument()
  })
})

const sampleRecording = (overrides: Partial<Recording> = {}): Recording => ({
  id: 1,
  site: 'default',
  source: 'manual',
  serviceName: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'ライブラリの録画',
  startAt: '2026-01-01T12:00:00Z',
  durationMs: 1_800_000,
  status: 'finished',
  createdAt: '2026-01-01T12:30:00Z',
  ...overrides,
})

const sampleService = (overrides: Partial<Service> = {}): Service => ({
  id: (overrides.networkId ?? 32736) * 100_000 + (overrides.serviceId ?? 5168),
  networkId: 32736,
  serviceId: 5168,
  name: 'ＯＨＫ',
  channelType: 'GR',
  channel: '27',
  remoteControlKeyId: 1,
  hasLogoData: false,
  hasPrograms: true,
  ...overrides,
})

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: body === null ? undefined : { 'Content-Type': 'application/json' },
  })
}

/**
 * FakeRecordingsServer は `/api/recordings` 周りの一連の操作を状態を持って
 * シミュレートする。invalidate 経由の再取得（削除・復元・完全削除の後に
 * 一覧が変わること）を「トーストが出た」ではなく「一覧が実際に変わった」で
 * 確かめるために、GET のたびに現在の状態を返す必要がある。
 *
 * 絞り込みは `q`（title 部分一致）・`status`・`source`・`ruleId` だけを実際に
 * 反映する。`genre` / `serviceId` / `from` / `to` は Recording 型に無い
 * （または実サーバーの絞り込みロジックを複製する価値が無い）ため、
 * リクエストの URL パラメータを見て「正しく送られたか」だけを確認する
 * （録画一覧側のテストで実施）。
 */
function createFakeRecordingsServer(options: {
  library?: Recording[]
  trash?: Recording[]
  sites?: string[]
  services?: Service[]
  encodeProfiles?: EncodeProfileSummary[]
  encodeQueue?: { queued: number; running: number }
  rules?: Rule[]
  // encodePostResponse は POST /api/recordings/{id}/encode-profiles の応答を
  // 差し替える（既定は 204 成功）。409 のときサーバーの英語文字列を UI が
  // そのまま出さないことを確認するテスト用。
  encodePostResponse?: () => Response
  // deleteResponse / restoreResponse / purgeResponse は各 mutation の応答を ID ごとに
  // 差し替える（既定は成功）。一括操作の部分成功を確認するため ID を渡す。
  deleteResponse?: (id: number) => Response | undefined
  restoreResponse?: (id: number) => Response | undefined
  purgeResponse?: (id: number) => Response | undefined
}) {
  let library = [...(options.library ?? [])]
  let trash = [...(options.trash ?? [])]
  const sites = options.sites ?? ['default']
  const services = options.services ?? [sampleService()]
  const encodeProfiles = options.encodeProfiles ?? []
  const encodeQueue = options.encodeQueue ?? { queued: 0, running: 0 }
  const rules = options.rules ?? []
  const encodePostResponse = options.encodePostResponse
  const deleteResponse = options.deleteResponse
  const restoreResponse = options.restoreResponse
  const purgeResponse = options.purgeResponse

  function paginate(url: URL, all: Recording[]): Recording[] {
    const q = url.searchParams.get('q')
    const status = url.searchParams.get('status')
    const source = url.searchParams.get('source')
    const ruleId = url.searchParams.get('ruleId')
    const order = url.searchParams.get('order') ?? 'desc'
    const limit = Number(url.searchParams.get('limit') ?? '50')
    const before = url.searchParams.get('before')
    const beforeId = url.searchParams.get('beforeId')

    let items = all.filter((r) => {
      if (q !== null && !r.title.includes(q)) return false
      if (status !== null && r.status !== status) return false
      if (source !== null && r.source !== source) return false
      if (ruleId !== null && String(r.ruleId ?? '') !== ruleId) return false
      return true
    })

    items = [...items].sort((a, b) => {
      const diff = new Date(a.startAt).getTime() - new Date(b.startAt).getTime()
      const primary = order === 'asc' ? diff : -diff
      if (primary !== 0) return primary
      return order === 'asc' ? a.id - b.id : b.id - a.id
    })

    if (before !== null && beforeId !== null) {
      const beforeTime = new Date(before).getTime()
      const beforeIdNum = Number(beforeId)
      items = items.filter((r) => {
        const t = new Date(r.startAt).getTime()
        if (order === 'asc') return t > beforeTime || (t === beforeTime && r.id > beforeIdNum)
        return t < beforeTime || (t === beforeTime && r.id < beforeIdNum)
      })
    }

    return items.slice(0, limit)
  }

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))
    if (/^\/api\/sites\/[^/]+\/services$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse(services))
    }
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(encodeProfiles))
    if (url.pathname === '/api/encode-queue') return Promise.resolve(jsonResponse(encodeQueue))
    if (url.pathname === '/api/rules' && method === 'GET') return Promise.resolve(jsonResponse(rules))

    if (url.pathname === '/api/recordings' && method === 'GET') {
      const trashParam = url.searchParams.get('trash') === 'true'
      const page = paginate(url, trashParam ? trash : library)
      return Promise.resolve(jsonResponse(page))
    }

    const deleteMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (deleteMatch && method === 'DELETE') {
      const id = Number(deleteMatch[1])
      const overridden = deleteResponse?.(id)
      if (overridden) return Promise.resolve(overridden)
      const idx = library.findIndex((r) => r.id === id)
      if (idx === -1) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      const [moved] = library.splice(idx, 1)
      trash = [...trash, { ...moved, deletedAt: '2026-01-05T00:00:00Z' }]
      return Promise.resolve(jsonResponse(null, 204))
    }

    const restoreMatch = /^\/api\/recordings\/(\d+)\/restore$/.exec(url.pathname)
    if (restoreMatch && method === 'POST') {
      const id = Number(restoreMatch[1])
      const overridden = restoreResponse?.(id)
      if (overridden) return Promise.resolve(overridden)
      const idx = trash.findIndex((r) => r.id === id)
      if (idx === -1) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      const [moved] = trash.splice(idx, 1)
      const { deletedAt: _deletedAt, ...rest } = moved
      library = [...library, rest]
      return Promise.resolve(jsonResponse(null, 204))
    }

    const purgeMatch = /^\/api\/recordings\/(\d+)\/purge$/.exec(url.pathname)
    if (purgeMatch && method === 'POST') {
      const id = Number(purgeMatch[1])
      const overridden = purgeResponse?.(id)
      if (overridden) return Promise.resolve(overridden)
      trash = trash.filter((r) => r.id !== id)
      return Promise.resolve(jsonResponse(null, 204))
    }

    const encodeMatch = /^\/api\/recordings\/(\d+)\/encode-profiles$/.exec(url.pathname)
    if (encodeMatch && method === 'POST') {
      if (encodePostResponse) return Promise.resolve(encodePostResponse())
      const id = Number(encodeMatch[1])
      const body: { profiles?: string[] } = init?.body ? JSON.parse(String(init.body)) : {}
      library = library.map((r) =>
        r.id === id
          ? { ...r, encodeProfiles: [...(r.encodeProfiles ?? []), ...(body.profiles ?? [])] }
          : r,
      )
      return Promise.resolve(jsonResponse(null, 204))
    }

    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse([]))
    }

    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  })

  globalThis.fetch = fetchMock as unknown as typeof fetch

  return {
    fetchMock,
    get library() {
      return library
    },
    get trash() {
      return trash
    },
  }
}

/**
 * renderPage は本物の routeTree（`@/routes`）で `RecordingsPage` を描く。
 *
 * `useSearch` / `useNavigate` / `Link` を使うため、`renderInRouter`
 * （最小限のアドホックなルート木）ではなく実際の `/recordings` ルート定義
 * （`validateSearch` を含む）を使う必要がある --- 検索条件の URL 往復を
 * 検証したいので、検証ロジックそのものを持たないルートでは意味がない。
 */
function renderPage(path = '/recordings') {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { queryClient, router }
}

/** recordingsRequests は `/api/recordings` への GET 呼び出しの URL 一覧を返す。 */
function recordingsRequests(fetchMock: ReturnType<typeof vi.fn>): URL[] {
  return fetchMock.mock.calls
    .map((call) => new URL(String(call[0]), 'http://localhost'))
    .filter((url) => url.pathname === '/api/recordings')
}

function mutationIds(fetchMock: ReturnType<typeof vi.fn>, suffix = '', method = 'POST'): number[] {
  const pattern = new RegExp(`^/api/recordings/(\\d+)${suffix}$`)
  return fetchMock.mock.calls.flatMap((call) => {
    const match = pattern.exec(new URL(String(call[0]), 'http://localhost').pathname)
    const init = call[1] as RequestInit | undefined
    return match && (init?.method ?? 'GET') === method ? [Number(match[1])] : []
  })
}

describe('RecordingsPage タブ', () => {
  it('/recordings?tab=trash で開くとごみ箱を表示する', async () => {
    createFakeRecordingsServer({
      library: [sampleRecording()],
      trash: [sampleRecording({ id: 2, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    renderPage('/recordings?tab=trash')

    expect(await screen.findByText('捨てた録画')).toBeInTheDocument()
    expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument()
  })

  it('ライブラリとごみ箱を切り替え、ごみ箱一覧を trash=true で取る', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      library: [sampleRecording()],
      trash: [sampleRecording({ id: 2, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    renderPage()

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()
    expect(screen.queryByText('捨てた録画')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))

    expect(await screen.findByText('捨てた録画')).toBeInTheDocument()
    expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument()

    const trashCalls = recordingsRequests(server.fetchMock).filter(
      (url) => url.searchParams.get('trash') === 'true',
    )
    expect(trashCalls.length).toBeGreaterThan(0)
  })

  it('タブ切替を URL に replace し、検索条件を保ったまま既定のライブラリは省略する', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [sampleRecording({ title: 'マッチする録画' })],
      trash: [sampleRecording({ id: 9, title: 'マッチする録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    const { router } = renderPage('/recordings?q=マッチ')
    await screen.findByText('マッチする録画')
    const initialHistoryLength = router.history.length

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))
    await waitFor(() => {
      expect(router.state.location.search).toMatchObject({ q: 'マッチ', tab: 'trash' })
    })
    expect(router.history.length).toBe(initialHistoryLength)

    await user.click(screen.getByRole('button', { name: 'ライブラリ' }))
    await waitFor(() => {
      expect(router.state.location.search.q).toBe('マッチ')
      expect(router.state.location.search.tab).toBeUndefined()
    })
    expect(router.history.length).toBe(initialHistoryLength)
  })

  // M3-25（#137）で useInfiniteQuery に置き換えた。繋ぎの固定 limit（200、
  // M3-24/#136 の申し送り）は外れ、既定ページサイズ（50）を明示的に渡す形になる。
  it('limit は 200 に固定しない（頭打ちの繋ぎを外した後の形）', async () => {
    const server = createFakeRecordingsServer({ library: [sampleRecording()] })

    renderPage()

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()

    const calls = recordingsRequests(server.fetchMock)
    expect(calls.length).toBeGreaterThan(0)
    for (const url of calls) {
      expect(url.searchParams.get('limit')).not.toBe('200')
    }
  })

  it('ごみ箱の GET が空配列を返すと「ごみ箱は空です」を出す', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({ library: [sampleRecording()], trash: [] })

    renderPage()

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))

    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()
    expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument()
  })

  it('ライブラリ/ごみ箱を切り替えても検索条件は保持される', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [sampleRecording({ title: 'マッチする録画' })],
      trash: [sampleRecording({ id: 9, title: 'マッチする録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    const { router } = renderPage()
    await screen.findByText('マッチする録画')

    const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })
    await user.type(input, 'マッチ')

    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ q: 'マッチ' }),
    )

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))
    await screen.findByText('マッチする録画')

    expect(router.state.location.search).toMatchObject({ q: 'マッチ' })
  })
})

// issue #311: 録画一覧の常時「再生」列をやめ、行本体を詳細（/recordings/$id）への
// リンクにする。視聴・削除・エンコードは詳細ページに寄せる（一覧はインライン展開も
// プレイヤーも持たない）。詳細ページ側の同等の挙動は recording-detail.test.tsx。
describe('RecordingsPage 行は詳細へのリンク (issue #311)', () => {
  it('再生可能な行は常時の再生ボタンを持たず、/recordings/$id へのリンクになる', async () => {
    createFakeRecordingsServer({
      library: [
        sampleRecording({
          id: 3,
          title: '再生できる録画',
          encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
          sizeBytes: 1_000_000,
        }),
      ],
    })

    renderPage()

    const link = await screen.findByRole('link', { name: '再生できる録画' })
    expect(link).toHaveAttribute('href', '/recordings/3')
    // 常時の「再生」列（`${title}を再生`）は無い
    expect(screen.queryByRole('button', { name: /を再生$/ })).not.toBeInTheDocument()
    // 一覧はプレイヤーを抱えない（詳細に寄せる。.play() 相当も走らない）
    expect(document.querySelector('video')).not.toBeInTheDocument()
  })

  it('ごみ箱の行も詳細へリンクする', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      trash: [sampleRecording({ id: 9, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    renderPage()

    await user.click(await screen.findByRole('button', { name: 'ごみ箱' }))

    const link = await screen.findByRole('link', { name: '捨てた録画' })
    expect(link).toHaveAttribute('href', '/recordings/9')
  })

  it('encoded が無い行も詳細へリンクする（原本だけでも）', async () => {
    createFakeRecordingsServer({
      library: [
        sampleRecording({ id: 32, title: 'エンコード無し録画', encodedAssets: [], sizeBytes: 1_000_000 }),
      ],
    })

    renderPage()

    const link = await screen.findByRole('link', { name: 'エンコード無し録画' })
    expect(link).toHaveAttribute('href', '/recordings/32')
  })
})

describe('RecordingsPage 複数選択と一括操作', () => {
  it('編集モード中だけ checkbox を出し、行と checkbox で 1 回ずつ選択を切り替える', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [sampleRecording(), sampleRecording({ id: 2, title: '二つ目の録画' })],
    })

    renderPage()
    await screen.findByText('ライブラリの録画')

    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '選択' }))

    const checkbox = screen.getByRole('checkbox', { name: 'ライブラリの録画を選択' })
    const row = checkbox.closest('[role="option"]')
    expect(row).toHaveAttribute('aria-selected', 'false')
    expect(screen.queryByRole('link', { name: 'ライブラリの録画' })).not.toBeInTheDocument()

    await user.click(row as HTMLElement)
    expect(checkbox).toBeChecked()
    expect(row).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByText('1 件を選択中')).toBeInTheDocument()

    await user.click(checkbox)
    expect(checkbox).not.toBeChecked()
    expect(row).toHaveAttribute('aria-selected', 'false')
    expect(screen.getByText('0 件を選択中')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ライブラリの録画' })).toBeInTheDocument()
  })

  it('読み込み済みと明記した全選択で現在の行だけを選ぶ', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [sampleRecording(), sampleRecording({ id: 2, title: '二つ目の録画' })],
    })

    renderPage()
    await screen.findByText('二つ目の録画')
    await user.click(screen.getByRole('button', { name: '選択' }))
    await user.click(screen.getByRole('button', { name: '読み込み済みの 2 件を選択' }))

    expect(screen.getByText('2 件を選択中')).toBeInTheDocument()
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(screen.getAllByRole('checkbox').every((checkbox) => (checkbox as HTMLInputElement).checked)).toBe(true)
  })

  it('ごみ箱送りは全件を試し、成功分だけ Undo して失敗本文を別に出す', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      library: [
        sampleRecording(),
        sampleRecording({ id: 2, title: '失敗する録画' }),
        sampleRecording({ id: 3, title: '三つ目の録画' }),
      ],
      deleteResponse: (id) =>
        id === 2 ? jsonResponse({ error: 'disk busy' }, 500) : undefined,
      restoreResponse: (id) =>
        id === 3 ? jsonResponse({ error: 'recording conflict' }, 409) : undefined,
    })

    renderPage()
    await screen.findByText('三つ目の録画')
    await user.click(screen.getByRole('button', { name: '選択' }))
    await user.click(screen.getByRole('button', { name: '読み込み済みの 3 件を選択' }))
    await user.click(screen.getByRole('button', { name: 'ごみ箱へ' }))

    await waitFor(() => expect(mutationIds(server.fetchMock, '', 'DELETE').sort()).toEqual([1, 2, 3]))
    await waitFor(() => expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument())
    expect(screen.queryByText('三つ目の録画')).not.toBeInTheDocument()
    expect(screen.getByText('失敗する録画')).toBeInTheDocument()
    expect(screen.getByText('2 件をごみ箱へ移動')).toBeInTheDocument()
    expect(screen.getByText(/1 件をごみ箱へ移動できませんでした: disk busy/)).toBeInTheDocument()
    expect(screen.getByText('1 件を選択中')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '元に戻す' }))

    await waitFor(() => expect(mutationIds(server.fetchMock, '/restore').sort()).toEqual([1, 3]))
    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()
    expect(screen.queryByText('三つ目の録画')).not.toBeInTheDocument()
    expect(screen.getByText(/1 件を元に戻せませんでした: recording conflict/)).toBeInTheDocument()
  })

  it('ごみ箱の選択分をまとめて復元する', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      trash: [
        sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
        sampleRecording({ id: 2, title: '二つ目の録画', deletedAt: '2026-01-02T00:00:00Z' }),
      ],
    })

    renderPage('/recordings?tab=trash')
    await screen.findByText('二つ目の録画')
    await user.click(screen.getByRole('button', { name: '選択' }))
    await user.click(screen.getByRole('button', { name: '読み込み済みの 2 件を選択' }))
    await user.click(screen.getByRole('button', { name: '復元' }))

    await waitFor(() => expect(mutationIds(server.fetchMock, '/restore').sort()).toEqual([1, 2]))
    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()
  })

  it('完全削除は件数付き dialog で確認してから全件へ送る', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      trash: [
        sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
        sampleRecording({ id: 2, title: '二つ目の録画', deletedAt: '2026-01-02T00:00:00Z' }),
      ],
    })

    renderPage('/recordings?tab=trash')
    await screen.findByText('二つ目の録画')
    await user.click(screen.getByRole('button', { name: '選択' }))
    await user.click(screen.getByRole('button', { name: '読み込み済みの 2 件を選択' }))
    await user.click(screen.getByRole('button', { name: '完全削除' }))

    expect(await screen.findByRole('alertdialog', { name: '2 件を完全削除しますか？' })).toBeInTheDocument()
    expect(mutationIds(server.fetchMock, '/purge')).toEqual([])

    // 取り返しがつかない操作の確定は destructive（issue #467、
    // alert-dialog.tsx の規約。variant を外すと落ちる）。
    const confirmButton = screen.getByRole('button', { name: '完全削除を予約する' })
    expect(confirmButton).toHaveClass('text-destructive')

    await user.click(confirmButton)

    await waitFor(() => expect(mutationIds(server.fetchMock, '/purge').sort()).toEqual([1, 2]))
    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()
  })
})

// issue #283: 多サイトでは行に site を出す（同じ (networkId, serviceId) を
// 2 サイトで受けたとき、行を見分ける材料が site しか無い）。単一サイトでは
// 「default」がノイズになるだけなので出さない。
describe('RecordingsPage 行の site 表示 (issue #283)', () => {
  it('複数サイトのときは各行に site を出す', async () => {
    createFakeRecordingsServer({
      sites: ['default', 'site2'],
      library: [sampleRecording({ id: 5, title: 'site2 の録画', site: 'site2' })],
    })

    renderPage()

    const row = (await screen.findByRole('link', { name: 'site2 の録画' })).closest('div')
    expect(within(row as HTMLElement).getByText('site2')).toBeInTheDocument()
  })

  it('単一サイトのときは site を出さない（default はノイズ）', async () => {
    createFakeRecordingsServer({
      sites: ['default'],
      library: [sampleRecording({ id: 6, title: '単一サイトの録画', site: 'default' })],
    })

    renderPage()

    await screen.findByRole('link', { name: '単一サイトの録画' })
    expect(screen.queryByText('default')).not.toBeInTheDocument()
  })
})

describe('RecordingsPage エンコード待機列', () => {
  it('ジョブ件数を表示し、待機中・実行中で録画一覧を絞り込める', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      library: [sampleRecording()],
      encodeQueue: { queued: 3, running: 1 },
    })
    const { router } = renderPage()

    await screen.findByText('ライブラリの録画')
    const queued = await screen.findByRole('button', { name: '待機中 3件' })
    expect(screen.getByRole('button', { name: '実行中 1件' })).toBeInTheDocument()

    await user.click(queued)

    await waitFor(() => expect(router.state.location.search).toMatchObject({ encodeState: 'queued' }))
    await waitFor(() => {
      expect(
        recordingsRequests(server.fetchMock).some(
          (url) => url.searchParams.get('encodeState') === 'queued',
        ),
      ).toBe(true)
    })
  })
})

describe('RecordingsPage 検索条件', () => {
  it('条件ありの 0 件と条件なしの 0 件で文言が違う', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({ library: [sampleRecording({ title: '別の録画' })] })

    renderPage()
    await screen.findByText('別の録画')

    const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })
    await user.type(input, 'ヒットしない語')

    expect(await screen.findByText('条件に一致する録画がありません')).toBeInTheDocument()
    expect(screen.queryByText('録画がありません')).not.toBeInTheDocument()

    // 「条件をクリア」を押すと条件が外れ、元の 1 件が戻る（空虚な成功ではなく、
    // クエリの解決を待ってから確認する）。
    await user.click(screen.getByRole('button', { name: '条件をクリア' }))
    expect(await screen.findByText('別の録画')).toBeInTheDocument()
  })

  it('条件を指定していない 0 件は「録画がありません」', async () => {
    createFakeRecordingsServer({ library: [] })

    renderPage()

    expect(await screen.findByText('録画がありません')).toBeInTheDocument()
    expect(screen.queryByText('条件に一致する録画がありません')).not.toBeInTheDocument()
  })

  it('キーワードは 300ms 経ってから絞り込む（デバウンス）', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
      const server = createFakeRecordingsServer({
        library: [sampleRecording({ title: 'ニュース7' }), sampleRecording({ id: 2, title: 'ドラマ' })],
      })

      renderPage()
      await screen.findByText('ニュース7')
      const initialCalls = recordingsRequests(server.fetchMock).length

      const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })
      await user.type(input, 'ニュース')

      // 300ms 経つ前は絞り込みリクエストが増えていない
      expect(recordingsRequests(server.fetchMock).length).toBe(initialCalls)

      await vi.advanceTimersByTimeAsync(300)

      await waitFor(() =>
        expect(recordingsRequests(server.fetchMock).length).toBeGreaterThan(initialCalls),
      )
      expect(await screen.findByText('ニュース7')).toBeInTheDocument()
      expect(screen.queryByText('ドラマ')).not.toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  // issue #137 の罠: 「キーワードの 1 文字ごとに push すると戻るボタンが使えなく
  // なる。replace を使う」。debounce の確定（キーワード）とチップ操作の両方が
  // history を積まないことを、実際の history の長さで固定する --- トーストや
  // 表示結果だけでは push/replace の違いは見えない。
  it('debounce の確定・チップ操作はいずれも履歴を積まない（push ではなく replace）', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
      createFakeRecordingsServer({
        library: [sampleRecording({ title: 'ニュース7', status: 'finished' })],
      })

      const { router } = renderPage()
      await screen.findByText('ニュース7')
      const lengthAfterInitialLoad = router.history.length

      const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })
      await user.type(input, 'ニ')
      await vi.advanceTimersByTimeAsync(300)
      await waitFor(() => expect(router.state.location.search).toMatchObject({ q: 'ニ' }))
      expect(router.history.length).toBe(lengthAfterInitialLoad)

      // 続けて別のキーワードに確定させる（debounce をもう 1 サイクル回す）。
      // push だとここで history が積まれてしまう。
      await user.clear(input)
      await user.type(input, 'ドラマ')
      await vi.advanceTimersByTimeAsync(300)
      await waitFor(() => expect(router.state.location.search).toMatchObject({ q: 'ドラマ' }))
      expect(router.history.length).toBe(lengthAfterInitialLoad)

      // チップ操作（絞り込みパネルの状態チップ）も同様。
      await user.click(screen.getByRole('button', { name: /絞り込み/ }))
      const panel = await screen.findByRole('dialog', { name: '絞り込み' })
      const statusGroup = within(panel).getByRole('group', { name: '状態' })
      await user.click(within(statusGroup).getByRole('button', { name: '完了' }))
      await waitFor(() =>
        expect(router.state.location.search).toMatchObject({ status: 'finished' }),
      )
      expect(router.history.length).toBe(lengthAfterInitialLoad)
    } finally {
      vi.useRealTimers()
    }
  })

  it('絞り込みパネルの状態チップが status を反映し、チップから外せる', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      library: [
        sampleRecording({ title: '完了した録画', status: 'finished' }),
        sampleRecording({ id: 2, title: '失敗した録画', status: 'failed' }),
      ],
    })

    renderPage()
    await screen.findByText('完了した録画')

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const statusGroup = within(panel).getByRole('group', { name: '状態' })
    await user.click(within(statusGroup).getByRole('button', { name: '失敗' }))

    await waitFor(() => expect(screen.queryByText('完了した録画')).not.toBeInTheDocument())
    expect(await screen.findByText('失敗した録画')).toBeInTheDocument()

    const lastStatusCall = recordingsRequests(server.fetchMock).at(-1)
    expect(lastStatusCall?.searchParams.get('status')).toBe('failed')

    // チップから外すと両方また出る
    await user.click(screen.getByText('状態: 失敗'))
    expect(await screen.findByText('完了した録画')).toBeInTheDocument()
    expect(screen.getByText('失敗した録画')).toBeInTheDocument()
  })

  it('ジャンル・チャンネルの選択が GET のクエリに乗る', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({ library: [sampleRecording()] })

    renderPage()
    await screen.findByText('ライブラリの録画')

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    await user.click(within(panel).getByRole('button', { name: 'ドラマ' }))

    await waitFor(() => {
      const last = recordingsRequests(server.fetchMock).at(-1)
      expect(last?.searchParams.getAll('genre')).toEqual(['3'])
    })

    await user.click(within(panel).getByRole('button', { name: /チャンネル/ }))
    const channelDialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await user.click(within(channelDialog).getByRole('button', { name: /ＯＨＫ/ }))

    await waitFor(() => {
      const last = recordingsRequests(server.fetchMock).at(-1)
      expect(last?.searchParams.getAll('service')).toEqual(['3273605168'])
    })
  })

  it('「条件をクリア」は絞り込みを全部外すが並び順は保持する', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      library: [sampleRecording({ title: 'ソート確認用録画', status: 'finished' })],
    })

    renderPage()
    await screen.findByText('ソート確認用録画')

    // 並び順を「古い順」にしてから状態を絞り込む
    await user.selectOptions(screen.getByRole('combobox', { name: '並び順' }), '古い順')
    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    await user.click(within(panel).getByRole('button', { name: '完了' }))

    await waitFor(() => {
      const last = recordingsRequests(server.fetchMock).at(-1)
      expect(last?.searchParams.get('status')).toBe('finished')
      expect(last?.searchParams.get('order')).toBe('asc')
    })

    await user.click(screen.getByRole('button', { name: '条件をクリア' }))

    await waitFor(() => {
      const last = recordingsRequests(server.fetchMock).at(-1)
      expect(last?.searchParams.get('status')).toBeNull()
      expect(last?.searchParams.get('order')).toBe('asc')
    })
  })

  it('/recordings?ruleId=N でそのルール由来の録画だけが出て、チップから外せる', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [
        sampleRecording({ title: 'ルール由来', ruleId: 5, source: 'rule' }),
        sampleRecording({ id: 2, title: '手動録画', source: 'manual' }),
      ],
    })

    renderPage('/recordings?ruleId=5')

    expect(await screen.findByText('ルール由来')).toBeInTheDocument()
    expect(screen.queryByText('手動録画')).not.toBeInTheDocument()
    expect(screen.getByText('ルール #5')).toBeInTheDocument()

    await user.click(screen.getByText('ルール #5'))

    expect(await screen.findByText('手動録画')).toBeInTheDocument()
    expect(screen.getByText('ルール由来')).toBeInTheDocument()
  })

  it('不正な検索パラメータは落として開く（例外にしない）', async () => {
    const server = createFakeRecordingsServer({ library: [sampleRecording()] })

    renderPage('/recordings?status=bogus&genre=99&genre=1&ruleId=abc&order=sideways')

    // 有効な genre（1）だけが残ってチップになる。他は「その条件なし」に落ちる。
    expect(await screen.findByText('ジャンル: スポーツ')).toBeInTheDocument()
    expect(screen.queryByText(/状態:/)).not.toBeInTheDocument()
    expect(screen.queryByText(/ルール #/)).not.toBeInTheDocument()

    // 「その条件なし」に落ちたことをチップの有無だけで確認すると、fake server 側の
    // 絞り込み（status が残っていれば該当 0 件になり、そもそも一覧の描画が別の理由で
    // 失敗しているだけに見える）に依存してしまう。実際に送られたリクエストの
    // クエリパラメータを直接見て、無効な次元がサーバーに渡っていないことを固定する。
    await waitFor(() => expect(recordingsRequests(server.fetchMock).length).toBeGreaterThan(0))
    const last = recordingsRequests(server.fetchMock).at(-1)
    expect(last?.searchParams.get('status')).toBeNull()
    expect(last?.searchParams.getAll('genre')).toEqual(['1'])
    expect(last?.searchParams.get('ruleId')).toBeNull()
    expect(last?.searchParams.get('order')).toBeNull()
  })

  it('小数の ruleId は落とす（?ruleId=1.5 が recordings.ruleId int64 バインドで 400 にならないように）', async () => {
    const server = createFakeRecordingsServer({ library: [sampleRecording()] })

    renderPage('/recordings?ruleId=1.5')

    await waitFor(() => expect(recordingsRequests(server.fetchMock).length).toBeGreaterThan(0))
    const last = recordingsRequests(server.fetchMock).at(-1)
    expect(last?.searchParams.get('ruleId')).toBeNull()
    expect(screen.queryByText(/ルール #/)).not.toBeInTheDocument()
  })
})

describe('RecordingsPage ページング', () => {
  it('スクロール相当の「さらに読み込む」で次ページが継ぎ足され、終端で止まる', async () => {
    const user = userEvent.setup()
    const many = Array.from({ length: 60 }, (_, i) =>
      sampleRecording({
        id: i + 1,
        title: `録画${i + 1}`,
        startAt: new Date(Date.parse('2026-01-01T00:00:00Z') + i * 60_000).toISOString(),
      }),
    )
    createFakeRecordingsServer({ library: many })

    renderPage()

    // 既定の並びは新しい順（startAt 降順）なので、先頭ページは録画60〜録画11。
    expect(await screen.findByText('録画60')).toBeInTheDocument()
    expect(screen.queryByText('録画10')).not.toBeInTheDocument()

    const loadMore = screen.getByRole('button', { name: 'さらに読み込む' })
    await user.click(loadMore)

    expect(await screen.findByText('録画10')).toBeInTheDocument()
    expect(screen.getByText('録画1')).toBeInTheDocument()
    // 60 件しかないので、これ以上のページは無い
    expect(screen.queryByRole('button', { name: 'さらに読み込む' })).not.toBeInTheDocument()
  })
})

/**
 * 状態色の適用。**色そのものは jsdom では測れない**（Tailwind のクラスは
 * 解決されないし oklch も計算されない）ので、ここで見るのは「どのトークンの
 * クラスが当たっているか」だけ。実際の画素での判定は `web/e2e/design.mjs`
 * （`pnpm e2e:design`）が持つ。
 *
 * それでもここに置くのは、`bg-tally` を `bg-primary/10` に戻すような差し替えを
 * 実ブラウザを起動せずに止められるため（docs/frontend/design.md）。
 */
describe('録画状態の信号色', () => {
  /** badgeFor は一覧の行から状態バッジの要素を引く。 */
  function badgeFor(label: string): HTMLElement {
    const badge = screen.getByText(label)
    expect(badge.tagName).toBe('SPAN')
    return badge
  }

  it('録画中はタリーレッドの「塗り」になる（destructive の淡い地と形で分かれる）', async () => {
    createFakeRecordingsServer({
      library: [sampleRecording({ id: 21, title: '進行中', status: 'recording' })],
    })

    renderPage()
    await screen.findByText('進行中')

    const badge = badgeFor('録画中')
    expect(badge).toHaveClass('bg-tally')
    expect(badge).toHaveClass('text-tally-foreground')
    // 塗りなので淡い地の流儀（`bg-*/10` + 色付きの文字）ではない
    expect(badge.className).not.toMatch(/bg-tally\//)
    expect(badge.className).not.toMatch(/text-tally(?![-\w])/)
  })

  it('失敗は destructive のまま（タリーに置き換えない）', async () => {
    createFakeRecordingsServer({
      library: [sampleRecording({ id: 22, title: '落ちた録画', status: 'failed' })],
    })

    renderPage()
    await screen.findByText('落ちた録画')

    const badge = badgeFor('失敗')
    expect(badge).toHaveClass('bg-destructive/10')
    expect(badge).toHaveClass('text-destructive')
    expect(badge.className).not.toMatch(/tally/)
  })

  it('完了は無彩のまま（信号色を使わない）', async () => {
    createFakeRecordingsServer({
      library: [sampleRecording({ id: 23, title: '終わった録画', status: 'finished' })],
    })

    renderPage()
    await screen.findByText('終わった録画')

    const badge = badgeFor('完了')
    expect(badge).toHaveClass('bg-muted')
    expect(badge.className).not.toMatch(/tally|warning|destructive/)
  })
})

describe('ドロップ統計バッジは日本語ラベル（issue #454）', () => {
  it('drop / error / scrambled の英語生値ではなく日本語のラベルで出る', async () => {
    createFakeRecordingsServer({
      library: [
        sampleRecording({
          id: 24,
          title: 'ドロップのある録画',
          dropSummary: { packets: 1000, drops: 12, errors: 1, scrambled: 3 },
        }),
      ],
    })

    renderPage()
    await screen.findByText('ドロップのある録画')

    expect(screen.getByText('ドロップ 12')).toBeInTheDocument()
    expect(screen.getByText('エラー 1')).toBeInTheDocument()
    expect(screen.getByText('スクランブル 3')).toBeInTheDocument()
    expect(screen.queryByText(/^drop /)).not.toBeInTheDocument()
    expect(screen.queryByText(/^scrambled /)).not.toBeInTheDocument()
  })
})
