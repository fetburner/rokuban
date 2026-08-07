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
  services?: Service[]
  encodeProfiles?: EncodeProfileSummary[]
}) {
  let library = [...(options.library ?? [])]
  let trash = [...(options.trash ?? [])]
  const services = options.services ?? [sampleService()]
  const encodeProfiles = options.encodeProfiles ?? []

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
    if (url.pathname === '/api/sites/default/services') return Promise.resolve(jsonResponse(services))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(encodeProfiles))

    if (url.pathname === '/api/recordings' && method === 'GET') {
      const trashParam = url.searchParams.get('trash') === 'true'
      const page = paginate(url, trashParam ? trash : library)
      return Promise.resolve(jsonResponse(page))
    }

    const deleteMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (deleteMatch && method === 'DELETE') {
      const id = Number(deleteMatch[1])
      const idx = library.findIndex((r) => r.id === id)
      if (idx === -1) return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      const [moved] = library.splice(idx, 1)
      trash = [...trash, { ...moved, deletedAt: '2026-01-05T00:00:00Z' }]
      return Promise.resolve(jsonResponse(null, 204))
    }

    const restoreMatch = /^\/api\/recordings\/(\d+)\/restore$/.exec(url.pathname)
    if (restoreMatch && method === 'POST') {
      const id = Number(restoreMatch[1])
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
      trash = trash.filter((r) => r.id !== id)
      return Promise.resolve(jsonResponse(null, 204))
    }

    const encodeMatch = /^\/api\/recordings\/(\d+)\/encode-profiles$/.exec(url.pathname)
    if (encodeMatch && method === 'POST') {
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

describe('RecordingsPage タブ', () => {
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
      expect(last?.searchParams.getAll('serviceId')).toEqual(['5168'])
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

describe('RecordingsPage invalidate', () => {
  it('ごみ箱へ移す操作の後、ライブラリ一覧から消える（invalidate の前方一致が効く）', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({ library: [sampleRecording({ id: 3, title: '消える録画' })] })

    renderPage()
    await user.click(await screen.findByText('消える録画'))
    await user.click(screen.getByRole('button', { name: 'ごみ箱へ' }))

    await waitFor(() => expect(screen.queryByText('消える録画')).not.toBeInTheDocument())
    expect(await screen.findByText('録画がありません')).toBeInTheDocument()
  })

  it('復元の後、ごみ箱一覧から消えてライブラリに戻る', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      trash: [sampleRecording({ id: 4, title: '復元される録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    renderPage()
    await user.click(await screen.findByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('復元される録画'))
    await user.click(screen.getByRole('button', { name: '復元' }))

    await waitFor(() => expect(screen.queryByText('復元される録画')).not.toBeInTheDocument())
    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ライブラリ' }))
    expect(await screen.findByText('復元される録画')).toBeInTheDocument()
  })

  it('「今すぐ完全削除」は確認ダイアログを挟み、確定するまで purge を呼ばない。確定後は一覧からも消える', async () => {
    const user = userEvent.setup()
    const server = createFakeRecordingsServer({
      trash: [sampleRecording({ id: 7, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })],
    })

    renderPage()
    await user.click(await screen.findByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てた録画'))

    // ボタンを押しただけでは purge は飛ばない（確認を挟む）
    await user.click(screen.getByRole('button', { name: '今すぐ完全削除' }))
    const purgeCallsBefore = server.fetchMock.mock.calls.filter((call) =>
      String(call[0]).includes('/purge'),
    )
    expect(purgeCallsBefore).toHaveLength(0)

    // ダイアログの確定ボタンを押して初めて purge が飛ぶ
    await user.click(await screen.findByRole('button', { name: '完全削除を予約する' }))
    await waitFor(() => expect(screen.queryByText('捨てた録画')).not.toBeInTheDocument())
    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()

    // 確定後はダイアログが閉じる（開いたまま残らない）
    expect(screen.queryByRole('button', { name: '完全削除を予約する' })).not.toBeInTheDocument()
    expect(await screen.findByText(/完全削除を予約しました/)).toBeInTheDocument()
  })

  it('ライブラリでは派生物・原本があればプレイヤーとサムネイルを出す', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [
        sampleRecording({
          id: 3,
          title: '再生できる録画',
          encodedProfiles: ['web'],
          sizeBytes: 1_000_000,
        }),
      ],
    })

    renderPage()

    await user.click(await screen.findByText('再生できる録画'))

    // クエリが解決してから見る（非同期の空虚な成功を避ける）
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(document.querySelector('img[src="/api/recordings/3/thumbnail"]')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ダウンロード / VLC' })).toBeInTheDocument()
  })

  it('ごみ箱では 404 になるサムネイル・プレイヤー・原本リンクを一切出さない', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      trash: [
        sampleRecording({
          id: 9,
          title: '捨てられた再生可能録画',
          deletedAt: '2026-01-03T00:00:00Z',
          encodedProfiles: ['web'],
          sizeBytes: 1_000_000,
        }),
      ],
    })

    renderPage()

    await user.click(await screen.findByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てられた再生可能録画'))

    // 展開後の内容（削除日時 dt）が出るまで待ってから「無い」ことを確認する
    // （クエリ未解決のうちに queryBy で通ってしまう空虚な成功を避ける）
    await screen.findByText('削除日時')

    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
    expect(document.querySelector('img')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'ダウンロード / VLC' })).not.toBeInTheDocument()
    expect(screen.queryByText('VLC 等で開く')).not.toBeInTheDocument()
  })
})

// 事後追加のエンコード依頼（issue #133、凍結の例外。docs/storage.md §6「凍結の
// 例外: 事後追加」）。RecordingActions に足した AddEncodeProfilesAction の
// 判定分岐 --- 原本の有無 / 追加済みの除外 / 送信 / ごみ箱で出さない、をそれぞれ
// 固定する。
describe('AddEncodeProfilesAction', () => {
  it('encodeProfiles（desired）にあるものは選択肢から外し「追加済み」に出す', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [
        sampleRecording({
          id: 11,
          title: '一部追加済み',
          sizeBytes: 1_000_000,
          encodeProfiles: ['h264'],
        }),
      ],
      encodeProfiles: [{ name: 'h264' }, { name: 'h265' }],
    })

    renderPage()
    await user.click(await screen.findByText('一部追加済み'))

    expect(await screen.findByText('事後エンコードの追加')).toBeInTheDocument()
    expect(screen.getByText('追加済み: h264')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'h264' })).not.toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'h265' })).toBeInTheDocument()
  })

  it('全プロファイルが追加済みなら、選択肢もボタンも出さず案内だけ出す', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [
        sampleRecording({
          id: 15,
          title: '全部追加済み',
          sizeBytes: 1_000_000,
          encodeProfiles: ['h264'],
        }),
      ],
      encodeProfiles: [{ name: 'h264' }],
    })

    renderPage()
    await user.click(await screen.findByText('全部追加済み'))

    expect(await screen.findByText('すべてのエンコードプロファイルが追加済みです。')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '追加エンコードを依頼' })).not.toBeInTheDocument()
  })

  it('選択したプロファイルを POST し、成功したらトーストを出して選択を空に戻す', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [
        sampleRecording({ id: 12, title: '追加できる録画', sizeBytes: 500, encodeProfiles: [] }),
      ],
      encodeProfiles: [{ name: 'h264' }],
    })

    renderPage()
    await user.click(await screen.findByText('追加できる録画'))
    await user.click(await screen.findByRole('checkbox', { name: 'h264' }))
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    expect(await screen.findByText('エンコードを依頼しました')).toBeInTheDocument()
    // 一覧の再取得後、依頼した h264 が「追加済み」に反映される（invalidate が効く。
    // 行は展開されたままなので、再度クリックしない --- クリックし直すと折り畳まれてしまう）
    expect(await screen.findByText('追加済み: h264')).toBeInTheDocument()
  })

  it('原本削除済み（sizeBytes 省略）では追加できない旨を出し、チェックボックスを出さない', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      library: [sampleRecording({ id: 13, title: '原本削除済み', encodeProfiles: [] })],
      encodeProfiles: [{ name: 'h264' }],
    })

    renderPage()
    await user.click(await screen.findByText('原本削除済み'))

    expect(
      await screen.findByText('原本が削除済みのため、追加のエンコードは依頼できません。'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  it('ごみ箱では追加エンコードのコントロールを一切出さない', async () => {
    const user = userEvent.setup()
    createFakeRecordingsServer({
      trash: [
        sampleRecording({
          id: 14,
          title: '捨てた録画・エンコード確認',
          deletedAt: '2026-01-05T00:00:00Z',
          sizeBytes: 500,
          encodeProfiles: [],
        }),
      ],
      encodeProfiles: [{ name: 'h264' }],
    })

    renderPage()
    await user.click(await screen.findByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てた録画・エンコード確認'))

    await screen.findByText('削除日時')
    expect(screen.queryByText('事後エンコードの追加')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })
})
