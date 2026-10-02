import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { getGetRecordingChaptersQueryKey, getRecordingChapters } from '@/api/generated'
import type {
  EncodeProfileSummary,
  LiveProfileSummary,
  Recording,
  RecordingChapters,
  DropStat,
  Rule,
} from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { formatTime } from '@/lib/format'
import { cmDetectStageMessage } from '@/lib/cm-detect-stage'
import { routeTree } from '@/routes'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

function sampleRecording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 3,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: 1,
    title: '単体ページの録画',
    startAt: '2026-01-01T12:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    keepOriginal: 'always',
    cmDetection: { state: 'disabled' },
    createdAt: '2026-01-01T12:30:00Z',
    ...overrides,
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: body === null ? undefined : { 'Content-Type': 'application/json' },
  })
}

function sampleRule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 5,
    name: 'サンプルルール',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

/**
 * createFakeServer は `GET /api/recordings/{id}` 単体取得とその周辺
 * （削除・復元・完全削除・追加エンコード）を状態を持ってシミュレートする。
 * `recordings.test.tsx` の `createFakeRecordingsServer` は一覧
 * （`GET /api/recordings`）専用なので、単体ページのテストにそのまま使えない
 * （このページは一覧を叩かない）。
 */
function createFakeServer(options: {
  recording: Recording | null
  sites?: string[]
  encodeProfiles?: EncodeProfileSummary[]
  liveProfiles?: LiveProfileSummary[]
  /** liveProfilesResponse は画質一覧の解決を遅延させるテスト用（issue #874）。 */
  liveProfilesResponse?: () => Promise<Response>
  rules?: Rule[]
  /** cmDetectCapability は `GET /api/capabilities` の cmDetect（cm_detect.enabled）。既定は無効。 */
  cmDetectCapability?: boolean
  /** rulesResponse はルール一覧の解決を遅延させるテスト用。 */
  rulesResponse?: () => Promise<Response>
  // deleteResponse / restoreResponse / purgeResponse / encodePostResponse は
  // 各操作の応答を差し替える（既定は成功）。失敗トーストや 409 翻訳の確認用。
  deleteResponse?: () => Response
  restoreResponse?: () => Response
  purgeResponse?: () => Response
  encodePostResponse?: () => Response
  // encodePolicyResponse は Promise 版も許す --- PATCH が解決する前の中間状態
  // （保存中…の表示）を確認するテストが、呼び出し側で自分の Promise を渡して
  // 解決タイミングを制御できるようにするため。
  encodePolicyResponse?: () => Response | Promise<Response>
  /** playbackState は別ページをまたぐサーバー再開位置を共有するテスト用。 */
  playbackState?: { positionMs?: number }
  chapters?: RecordingChapters
  chaptersResponse?: () => Promise<Response>
  dropStats?: DropStat[]
  /**
   * seriesRecordings は `GET /api/recordings?seriesOf=` に返す行（「次の
   * エピソード」の探索。M8-6）。既定は空。
   */
  seriesRecordings?: Recording[]
}) {
  let recording = options.recording
  const sites = options.sites ?? ['default']
  const encodeProfiles = options.encodeProfiles ?? []
  const liveProfiles = options.liveProfiles ?? []
  const liveProfilesResponse = options.liveProfilesResponse
  const rules = options.rules ?? []
  const rulesResponse = options.rulesResponse
  const deleteResponse = options.deleteResponse
  const restoreResponse = options.restoreResponse
  const purgeResponse = options.purgeResponse
  const encodePostResponse = options.encodePostResponse
  const encodePolicyResponse = options.encodePolicyResponse
  const playbackState = options.playbackState ?? {}
  const chapters = options.chapters ?? {
    version: 'chapters-v1',
    detectionPending: false,
    source: 'auto' as const,
    spans: [],
  }

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/capabilities') return Promise.resolve(jsonResponse({ live: true, cmDetect: options.cmDetectCapability ?? false }))
    // サイトレジストリを先に解決する。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(sites))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(encodeProfiles))
    if (url.pathname === '/api/live-profiles') {
      return liveProfilesResponse ? liveProfilesResponse() : Promise.resolve(jsonResponse(liveProfiles))
    }
    if (url.pathname === '/api/rules' && method === 'GET') {
      return rulesResponse ? rulesResponse() : Promise.resolve(jsonResponse(rules))
    }

    // 「次のエピソード」の探索（`?seriesOf=`）。他の経路がこの一覧を叩かないので、
    // ハンドラを足しても既存のテストの「unexpected fetch」は変わらない。
    if (url.pathname === '/api/recordings' && method === 'GET') {
      return Promise.resolve(jsonResponse(options.seriesRecordings ?? []))
    }

    const getMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (getMatch && method === 'GET') {
      const id = Number(getMatch[1])
      const requested = recording?.id === id
        ? recording
        : options.seriesRecordings?.find((item) => item.id === id)
      if (!requested) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      return Promise.resolve(
        jsonResponse(
          playbackState.positionMs === undefined
            ? requested
            : { ...requested, resumePositionMs: playbackState.positionMs },
        ),
      )
    }

    const deleteMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (deleteMatch && method === 'DELETE') {
      if (deleteResponse) return Promise.resolve(deleteResponse())
      const id = Number(deleteMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      recording = { ...recording, deletedAt: '2026-01-05T00:00:00Z' }
      return Promise.resolve(jsonResponse(null, 204))
    }

    const restoreMatch = /^\/api\/recordings\/(\d+)\/restore$/.exec(url.pathname)
    if (restoreMatch && method === 'POST') {
      if (restoreResponse) return Promise.resolve(restoreResponse())
      const id = Number(restoreMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      const { deletedAt: _deletedAt, ...rest } = recording
      recording = rest
      return Promise.resolve(jsonResponse(null, 204))
    }

    const purgeMatch = /^\/api\/recordings\/(\d+)\/purge$/.exec(url.pathname)
    if (purgeMatch && method === 'POST') {
      if (purgeResponse) return Promise.resolve(purgeResponse())
      const id = Number(purgeMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      // 完全削除予約後の再取得では tombstone が 404 になる API 契約を模す。
      recording = null
      return Promise.resolve(jsonResponse(null, 204))
    }

    const encodeMatch = /^\/api\/recordings\/(\d+)\/encode-profiles$/.exec(url.pathname)
    if (encodeMatch && method === 'POST') {
      if (encodePostResponse) return Promise.resolve(encodePostResponse())
      const id = Number(encodeMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      const body: { profiles?: string[] } = init?.body ? JSON.parse(String(init.body)) : {}
      recording = {
        ...recording,
        encodeProfiles: [...(recording.encodeProfiles ?? []), ...(body.profiles ?? [])],
      }
      return Promise.resolve(jsonResponse(null, 204))
    }

    const policyMatch = /^\/api\/recordings\/(\d+)\/encode-policy$/.exec(url.pathname)
    if (policyMatch && method === 'PATCH') {
      if (encodePolicyResponse) return Promise.resolve(encodePolicyResponse())
      const id = Number(policyMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      const body: { keepOriginal?: Recording['keepOriginal'] } = init?.body
        ? JSON.parse(String(init.body))
        : {}
      if (body.keepOriginal !== undefined) {
        recording = { ...recording, keepOriginal: body.keepOriginal }
      }
      return Promise.resolve(jsonResponse(null, 204))
    }

    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse(options.dropStats ?? []))
    }
    if (/^\/api\/recordings\/\d+\/chapters$/.test(url.pathname)) {
      return options.chaptersResponse
        ? options.chaptersResponse()
        : Promise.resolve(jsonResponse(chapters))
    }
    const playbackPositionMatch = /^\/api\/recordings\/(\d+)\/playback-position$/.exec(url.pathname)
    if (playbackPositionMatch && (method === 'PUT' || method === 'DELETE')) {
      playbackState.positionMs =
        method === 'PUT' && init?.body
          ? (JSON.parse(String(init.body)) as { positionMs: number }).positionMs
          : undefined
      return Promise.resolve(jsonResponse(null, 204))
    }
    const watchedMatch = /^\/api\/recordings\/(\d+)\/watched$/.exec(url.pathname)
    if (watchedMatch && (method === 'PUT' || method === 'DELETE')) {
      const id = Number(watchedMatch[1])
      if (recording?.id === id) {
        recording = {
          ...recording,
          watchedAt: method === 'PUT' ? '2026-10-01T00:00:00Z' : undefined,
        }
      }
      return Promise.resolve(jsonResponse(null, 204))
    }
    if (
      /^\/api\/sites\/[^/]+\/recordings\/\d+\/chase(?:\/offset\/\d+)?\/playlist\.m3u8$/.test(
        url.pathname,
      )
    ) {
      return Promise.resolve(new Response('#EXTM3U\n', { status: 200 }))
    }
    if (
      /^\/api\/sites\/[^/]+\/recordings\/\d+\/chase(?:\/offset\/\d+)?\/leave$/.test(url.pathname) &&
      method === 'POST'
    ) {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (/^\/api\/sites\/[^/]+\/recordings\/\d+\/original-vod\/playlist\.m3u8$/.test(url.pathname)) {
      return Promise.resolve(
        new Response('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nhd.0.m3u8\n', { status: 200 }),
      )
    }
    if (
      /^\/api\/sites\/[^/]+\/recordings\/\d+\/original-vod\/leave$/.test(url.pathname) &&
      method === 'POST'
    ) {
      return Promise.resolve(new Response(null, { status: 204 }))
    }

    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  })

  globalThis.fetch = fetchMock as unknown as typeof fetch
  return { fetchMock }
}

function renderAt(path: string) {
  window.scrollTo = vi.fn()
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

async function selectDetailTab(name: '番組' | '版' | '記録') {
  const tab = await screen.findByRole('tab', { name })
  fireEvent.click(tab)
  return tab
}

async function openRecordingMenu() {
  fireEvent.click(await screen.findByRole('button', { name: '録画のその他の操作' }))
  return screen.findByRole('menu')
}

async function expandKeepOriginalEditor() {
  fireEvent.click(await screen.findByRole('button', { name: 'この回だけ変える' }))
  return screen.findByLabelText('原本の保持')
}

/** jsdom の video 要素は再生位置と長さを自動更新しないので、テストから設定する。 */
function setMediaProps(video: HTMLVideoElement, props: { currentTime?: number; duration?: number }) {
  for (const [key, value] of Object.entries(props)) {
    Object.defineProperty(video, key, { value, writable: true, configurable: true })
  }
}

describe('RecordingDetailPage', () => {
  // 受け入れ基準: /recordings/{id} で録画単体が開き、再生・操作が機能する
  // （issue #232。issue #311 で一覧のインライン展開を廃止したため、ここが唯一の着地先）。
  it('通常の録画は再生・版と値札を出し、管理操作はページ見出しのメニューに置く', async () => {
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(document.querySelector('img[src="/api/media/recordings/3/thumbnail"]')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ダウンロード / VLC' })).toBeInTheDocument()
    expect(screen.getByText('976.6 KB')).toBeInTheDocument()
    await openRecordingMenu()
    expect(screen.getByRole('menuitem', { name: 'ごみ箱へ' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '今すぐ完全削除' })).not.toBeInTheDocument()
  })

  it('fromBeginning がある録画詳細は保存済みの再生位置を復元しない', async () => {
    createFakeServer({
      recording: sampleRecording({
        resumePositionMs: 90_000,
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
      }),
    })

    renderAt('/recordings/3?fromBeginning=true')
    await screen.findByRole('region', { name: '再生' })
    const video = document.querySelector('video')!
    Object.defineProperty(video, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(video)
    expect(video.currentTime).toBe(0)
  })

  it('視聴済み PUT が通ると録画一覧クエリを無効化する', async () => {
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'h264', sizeBytes: 123 }] }),
    })
    const { queryClient } = renderAt('/recordings/3')
    const invalidateQueries = vi.spyOn(queryClient, 'invalidateQueries')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    const video = document.querySelector('video')!
    setMediaProps(video, { currentTime: 0, duration: 300 })
    fireEvent.loadedMetadata(video)
    setMediaProps(video, { currentTime: 270 })
    fireEvent.timeUpdate(video)

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/recordings/3/watched',
        expect.objectContaining({ method: 'PUT' }),
      )
      expect(invalidateQueries).toHaveBeenCalledWith({ queryKey: [recordingsQueryKeyPrefix] })
    })
  })

  it('4層のうちメタデータと記録はタブで切り替え、内容を重ねて出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        sizeBytes: 1_000_000,
        series: '作品X',
        ruleId: 5,
        source: 'rule',
        startedAt: '2026-01-01T12:02:00Z',
        endedAt: '2026-01-01T12:30:00Z',
        ingest: { state: 'pending' },
        qualityEvents: [{ event: 'write-error' }],
        dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
      }),
    })

    const { router } = renderAt('/recordings/3?source=layout#rough')
    const pushStateSpy = vi.spyOn(window.history, 'pushState')
    const replaceStateSpy = vi.spyOn(window.history, 'replaceState')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    const initialHref = router.history.location.href
    const initialHistoryLength = router.history.length
    expect(screen.getAllByRole('tab')).toHaveLength(3)
    expect(screen.getAllByRole('tabpanel')).toHaveLength(1)
    expect(screen.queryByTestId('recording-program-group')).not.toBeInTheDocument()
    expect(screen.queryByTestId('recording-observations')).not.toBeInTheDocument()
    await selectDetailTab('番組')
    expect(screen.getByTestId('recording-program-group')).toBeInTheDocument()
    expect(screen.queryByTestId('recording-observations')).not.toBeInTheDocument()
    await selectDetailTab('記録')
    expect(screen.getByTestId('recording-observations')).toBeInTheDocument()
    expect(screen.queryByTestId('recording-program-group')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '続き' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '資産' })).not.toBeInTheDocument()
    expect(router.history.location.href).toBe(initialHref)
    expect(router.history.length).toBe(initialHistoryLength)
    expect(pushStateSpy).not.toHaveBeenCalled()
    expect(replaceStateSpy).not.toHaveBeenCalled()
  })

  it('ごみ箱では番組タブと復元だけを出し、完全削除は確認付きメニューに置く', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-05T00:00:00Z',
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
      }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByRole('button', { name: '復元' })).toBeInTheDocument()
    expect(screen.queryByTestId('recording-playback-group')).not.toBeInTheDocument()
    expect(screen.queryByTestId('recording-assets-group')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /ダウンロード \/ VLC/ })).not.toBeInTheDocument()
    expect(screen.getAllByRole('tab').map((tab) => tab.textContent)).toEqual(['番組'])
    await openRecordingMenu()
    expect(screen.getByRole('menuitem', { name: '今すぐ完全削除' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: 'ごみ箱へ' })).not.toBeInTheDocument()
  })

  it('詳細ヘッダーは状態・取り込み・エンコードの後にドロップ信号を並べ、観測にも要約を出す', async () => {
    createFakeServer({
      recording: sampleRecording({
        ingest: { state: 'pending' },
        encodeStatus: [{ profile: 'h264', state: 'failed' }],
        dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        qualityEvents: [{ event: 'write-error' }],
      }),
      dropStats: [{ pid: 256, pidType: 'video', packets: 1000, drops: 12, errors: 0, scrambled: 3, positions: [{ byteOffset: 1880, elapsedMs: 30000 }] }],
    })

    renderAt('/recordings/3')

    expect(await screen.findByRole('button', { name: 'ドロップ状態を記録タブで見る' })).toBeInTheDocument()
    await selectDetailTab('記録')
    const observation = screen.getByTestId('recording-observations')
    expect(within(observation).getAllByTestId('recording-diagnostic-row')).toHaveLength(4)
    const pidDetails = await within(observation).findByTestId('drop-stats-details')
    expect(pidDetails).toBeInTheDocument()
    expect((pidDetails as HTMLDetailsElement).open).toBe(false)
    expect(within(observation).getByTestId('recording-quality-events-details')).toBeInTheDocument()
    expect(within(observation).getAllByTestId('recording-diagnostic-row').at(-1)).toHaveTextContent('ドロップ')
  })

  it('異常がない記録は CM 検出の一行だけで、PID 表を出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        sizeBytes: 1_000_000,
        cmDetection: { state: 'disabled' },
        dropSummary: { packets: 1000, drops: 0, errors: 0, scrambled: 0 },
      }),
    })

    renderAt('/recordings/3')
    await selectDetailTab('記録')

    const observation = screen.getByTestId('recording-observations')
    expect(within(observation).getAllByTestId('recording-diagnostic-row')).toHaveLength(1)
    expect(within(observation).getByTestId('recording-diagnostic-row')).toHaveTextContent('CM 検出無効')
    expect(within(observation).queryByTestId('drop-stats-details')).not.toBeInTheDocument()
  })

  it('タイトル行の状態・取り込み・エンコード・ドロップバッジを押すと記録タブが開く', async () => {
    createFakeServer({
      recording: sampleRecording({
        sizeBytes: 1_000_000,
        ingest: { state: 'pending' },
        encodeStatus: [{ profile: 'h264', state: 'failed' }],
        dropSummary: { packets: 1000, drops: 1, errors: 0, scrambled: 0 },
      }),
    })

    renderAt('/recordings/3')
    for (const badge of [
      '録画状態を記録タブで見る',
      '取り込み状態を記録タブで見る',
      'エンコード状態を記録タブで見る',
      'ドロップ状態を記録タブで見る',
    ]) {
      await selectDetailTab('版')
      fireEvent.click(await screen.findByRole('button', { name: badge }))
      expect(screen.getByRole('tab', { name: '記録' })).toHaveAttribute('aria-selected', 'true')
      expect(screen.getByTestId('recording-observations')).toBeInTheDocument()
    }
    const observation = screen.getByTestId('recording-observations')
    expect(within(observation).getAllByTestId('recording-diagnostic-row').at(-1)).toHaveTextContent('ドロップ')
  })

  it('ドロップ値がすべて0ならヘッダーにバッジも観測にドロップ節も出さない', async () => {
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({
        dropSummary: { packets: 1000, drops: 0, errors: 0, scrambled: 0 },
        // 取り込み観測で外側の観測グループを表示し、内訳表だけを条件確認する。
        ingest: { state: 'pending' },
      }),
      // 行があっても、要約がすべて 0 なら内訳表を取得・表示しない。
      dropStats: [{ pid: 256, packets: 1000, drops: 0, errors: 0, scrambled: 0, positions: [] }],
    })

    const { queryClient } = renderAt('/recordings/3')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    await waitFor(() => expect(queryClient.isFetching()).toBe(0))
    expect(screen.queryByRole('button', { name: 'ドロップ状態を記録タブで見る' })).not.toBeInTheDocument()
    await selectDetailTab('記録')
    expect(screen.queryByTestId('drop-stats-details')).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalledWith('/api/recordings/3/drop-stats', expect.anything())
  })

  it('90% 到達後の視聴済み PUT 成功で録画一覧クエリを invalidate する', async () => {
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({
        durationMs: 100_000,
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
      }),
    })
    const { queryClient } = renderAt('/recordings/3')
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')

    const video = (await screen.findByLabelText('録画映像')) as HTMLVideoElement
    Object.defineProperty(video, 'duration', { value: 100, configurable: true })
    Object.defineProperty(video, 'currentTime', { value: 90, writable: true, configurable: true })
    fireEvent.timeUpdate(video)

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/recordings/3/watched',
        expect.objectContaining({ method: 'PUT' }),
      )
      expect(invalidate).toHaveBeenCalledWith({ queryKey: [recordingsQueryKeyPrefix] })
    })
  })

  // 操作バーの ✓ は固定の名前「視聴済み」+ aria-pressed のトグル。プレイヤー外のボタンは
  // 押した後の動作を名前にする（aria-pressed を持たない）。
  const barToggle = (watched: boolean) => ({ name: '視聴済み', pressed: watched })
  const outsideButton = (watched: boolean) => ({ name: watched ? '未視聴に戻す' : '視聴済みにする' })
  describe.each([
    [
      'プレイヤーの操作バー',
      { encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 },
      barToggle,
    ],
    ['原本のみ（プレイヤー外）', { sizeBytes: 1_000_000 }, outsideButton],
    ['資産なし（プレイヤー外）', {}, outsideButton],
  ] as [string, Partial<Recording>, (watched: boolean) => { name: string; pressed?: boolean }][])(
    '視聴済みボタン: %s',
    (_name, overrides, button) => {
      it('押すと PUT、もう一度押すと DELETE が飛ぶ', async () => {
        const { fetchMock } = createFakeServer({ recording: sampleRecording({ ...overrides }) })
        const user = userEvent.setup()
        renderAt('/recordings/3')

        await user.click(await screen.findByRole('button', button(false)))
        await waitFor(() =>
          expect(fetchMock).toHaveBeenCalledWith('/api/recordings/3/watched', expect.objectContaining({ method: 'PUT' })),
        )
        await user.click(await screen.findByRole('button', button(true)))
        await waitFor(() =>
          expect(fetchMock).toHaveBeenCalledWith(
            '/api/recordings/3/watched',
            expect.objectContaining({ method: 'DELETE' }),
          ),
        )
      })

      it('完了していない録画には出さない', async () => {
        createFakeServer({ recording: sampleRecording({ ...overrides, status: 'failed' }) })
        renderAt('/recordings/3')
        await screen.findByText('単体ページの録画')
        expect(screen.queryByRole('button', { name: button(false).name })).not.toBeInTheDocument()
        expect(screen.queryByRole('button', { name: button(true).name })).not.toBeInTheDocument()
      })
    },
  )

  // M8-6: シリーズの導線。起点の実効シリーズが null の録画には出さない
  // （ハブも「次回」も 0 件になるので、押した先が無い導線を置かない）。
  it('実効シリーズが無い録画にはシリーズの導線を出さない', async () => {
    createFakeServer({ recording: sampleRecording() })

    renderAt('/recordings/3')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'このシリーズへ' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /次のエピソード/ })).not.toBeInTheDocument()
  })

  // M8-6: 「次のエピソード」は再生できる行に限る。開始時刻がずれて supersede
  // されなかった failed 行（原本も encoded も無い）を指すと、押した先の再生が
  // 404 になる（メディア配信の契約）。
  it('「次のエピソード」は再生できない failed 行を飛ばし、起点の後を昇順で引く', async () => {
    const origin = sampleRecording({
      id: 3,
      title: 'アニメ　作品X　第1話',
      series: '作品X',
      sizeBytes: 1000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 900 }],
    })
    const { fetchMock } = createFakeServer({
      recording: origin,
      seriesRecordings: [
        origin,
        sampleRecording({
          id: 4,
          title: 'アニメ　作品X　第2話',
          series: '作品X',
          startAt: '2026-01-08T12:00:00Z',
          status: 'failed',
        }),
        sampleRecording({
          id: 5,
          title: 'アニメ　作品X　第3話',
          series: '作品X',
          startAt: '2026-01-15T12:00:00Z',
          sizeBytes: 2000,
        }),
      ],
    })

    renderAt('/recordings/3')

    expect(
      within(await screen.findByTestId('recording-series-links')).getByRole('link', { name: /^このシリーズへ/ }),
    ).toHaveAttribute('href', '/recordings/3/series')
    expect(
      await screen.findByRole('link', { name: '次のエピソード: アニメ　作品X　第3話' }),
    ).toHaveAttribute('href', '/recordings/5')

    // 探索は `?seriesOf=` + 昇順 + 起点の時刻から。降順で引くと「次の回」が
    // 最初のページに入らない（記録として URL を固定する）。
    const listURLs = fetchMock.mock.calls
      .map(([input]) => String(input))
      .filter((target) => target.startsWith('/api/recordings?'))
    const nextURL = listURLs.find((target) => target.includes('order=asc'))
    expect(nextURL).toContain('seriesOf=3')
    expect(nextURL).toContain('from=2026-01-01T12%3A00%3A00Z')
    // 棚は過去の回を含めて新しい順に 1 本だけ引く（次のエピソードの窓を使い回さない）。
    const shelfURLs = listURLs.filter((target) => target.includes('order=desc'))
    expect(shelfURLs).toHaveLength(1)
    expect(shelfURLs[0]).toContain('seriesOf=3')
    expect(shelfURLs[0]).not.toContain('from=')
    expect(listURLs).toHaveLength(2)
  })

  it('desktop のシリーズ棚から選んだ行はその録画の詳細を開く', async () => {
    const user = userEvent.setup()
    const origin = sampleRecording({
      id: 3,
      title: '作品X 第1話',
      series: '作品X',
      sizeBytes: 1000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 900 }],
    })
    const next = sampleRecording({
      id: 4,
      title: '作品X 第2話',
      series: '作品X',
      sizeBytes: 1200,
      encodedAssets: [{ profile: 'h264', sizeBytes: 1000 }],
      startAt: '2026-01-08T12:00:00Z',
    })
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })

    renderAt('/recordings/3')

    const shelf = await screen.findByTestId('recording-series-shelf')
    await waitFor(() => expect(shelf.querySelector('a[href="/recordings/4"]')).not.toBeNull())
    const nextLink = shelf.querySelector<HTMLAnchorElement>('a[href="/recordings/4"]')
    expect(nextLink).not.toBeNull()
    await user.click(nextLink!)
    expect(await screen.findByRole('heading', { name: '作品X 第2話' })).toBeInTheDocument()
  })

  it('実録画時刻は開始・終了が番組時刻より前後した場合だけ一言で出す', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        startedAt: '2026-01-01T11:59:30Z',
        endedAt: '2026-01-01T12:30:30Z',
      }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByTestId('recording-actual-time-difference')).toHaveTextContent(
      '30秒早く開始・30秒遅れて終了',
    )
    await selectDetailTab('番組')
    // 30 秒の差が番組の開始・終了と同じ「分」表示に潰れないよう、秒まで出す。
    expect(screen.getByText('実録画開始').nextElementSibling).toHaveTextContent(/:59:30$/)
    expect(screen.getByText('実録画終了').nextElementSibling).toHaveTextContent(/:30:30$/)
    const beforeProgramme = await screen.findByTestId('recorded-before-program')
    const afterProgramme = screen.getByTestId('recorded-after-program')
    // 破線は repeating-linear-gradient で描く（両端の縦線だけにならない）。実ブラウザの描画は
    // web/e2e/recording-detail-layout.mjs が測る。
    expect(beforeProgramme.className).toContain('repeating-linear-gradient')
    expect(afterProgramme.className).toContain('repeating-linear-gradient')
    expect(parseFloat(beforeProgramme.getAttribute('style')!.match(/width:\s*([^;]+)/)![1])).toBeCloseTo(
      (30 / 1860) * 100,
      2,
    )
    expect(parseFloat(afterProgramme.getAttribute('style')!.match(/left:\s*([^;]+)/)![1])).toBeCloseTo(
      (1830 / 1860) * 100,
      2,
    )
  })

  it('カット版の再生中は番組外区間を描かない（原本の時間軸の割合をカット版に当てない）', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'cut', cut: true, sizeBytes: 500_000 }],
        startedAt: '2026-01-01T11:59:30Z',
        endedAt: '2026-01-01T12:30:30Z',
      }),
    })

    renderAt('/recordings/3')

    await screen.findByTestId('player-controls')
    expect(screen.queryByTestId('recorded-outside-program-range')).not.toBeInTheDocument()
  })

  it('実録画時刻が番組時刻と一致すると差分ラベルを出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        startedAt: '2026-01-01T12:00:00Z',
        endedAt: '2026-01-01T12:30:00Z',
      }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByTestId('recording-actual-time-difference')).not.toBeInTheDocument()
    expect(screen.queryByTestId('recorded-before-program')).not.toBeInTheDocument()
    expect(screen.queryByTestId('recorded-after-program')).not.toBeInTheDocument()
    await selectDetailTab('番組')
    expect(screen.queryByText('実録画開始')).not.toBeInTheDocument()
    expect(screen.queryByText('実録画終了')).not.toBeInTheDocument()
  })

  it('実録画時刻が番組より遅れて始まり早く終わる差分もタイトル行に出す', async () => {
    createFakeServer({
      recording: sampleRecording({
        startedAt: '2026-01-01T12:00:30Z',
        endedAt: '2026-01-01T12:29:30Z',
      }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByTestId('recording-actual-time-difference')).toHaveTextContent(
      '30秒遅れて開始・30秒早く終了',
    )
    await selectDetailTab('番組')
    expect(screen.getByText('実録画開始').nextElementSibling).toHaveTextContent(/:00:30$/)
    expect(screen.getByText('実録画終了').nextElementSibling).toHaveTextContent(/:29:30$/)
  })

  it('終端カードに次がないときは再視聴とごみ箱を示す', async () => {
    const user = userEvent.setup()
    const playSpy = vi.spyOn(window.HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    createFakeServer({
      recording: sampleRecording({
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
      }),
    })

    renderAt('/recordings/3')
    const video = (await screen.findByLabelText('録画映像')) as HTMLVideoElement
    Object.defineProperty(video, 'currentTime', { value: 120, writable: true, configurable: true })
    fireEvent.ended(video)

    const card = await screen.findByTestId('recording-end-card')
    expect(within(card).getByRole('button', { name: 'もう一度見る' })).toBeInTheDocument()
    expect(within(card).getByRole('button', { name: 'この回をごみ箱へ' })).toBeInTheDocument()
    expect(within(card).queryByText(/秒後に再生/)).not.toBeInTheDocument()
    expect(within(card).getByText('最後のエピソードです')).toBeInTheDocument()

    await user.click(within(card).getByRole('button', { name: 'もう一度見る' }))
    expect(video.currentTime).toBe(0)
    expect(playSpy).toHaveBeenCalled()
    fireEvent.ended(video)
    await user.click(await screen.findByRole('button', { name: 'この回をごみ箱へ' }))
    expect(await screen.findByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  it('終端カードの「今すぐ再生」は待たずに次のエピソードへ移る', async () => {
    const user = userEvent.setup()
    const origin = sampleRecording({
      id: 3,
      title: '作品X 第1話',
      series: '作品X',
      sizeBytes: 1_000_000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
    })
    const next = sampleRecording({
      id: 4,
      title: '作品X 第2話',
      series: '作品X',
      sizeBytes: 1_000_000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
      startAt: '2026-01-08T12:00:00Z',
    })
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    const { router } = renderAt('/recordings/3')
    const video = await screen.findByLabelText('録画映像')

    fireEvent.ended(video)
    const card = await screen.findByTestId('recording-end-card')
    await user.click(within(card).getByRole('button', { name: '今すぐ再生' }))

    expect(await screen.findByRole('heading', { name: '作品X 第2話' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/recordings/4')
  })

  it('終端カードの取り消しで自動遷移を止め、取り消さない場合は次へ進む', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
      const origin = sampleRecording({
        id: 3,
        title: '作品X 第1話',
        series: '作品X',
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
      })
      const next = sampleRecording({
        id: 4,
        title: '作品X 第2話',
        series: '作品X',
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
        startAt: '2026-01-08T12:00:00Z',
      })
      createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
      const { router } = renderAt('/recordings/3')
      const video = await screen.findByLabelText('録画映像')

      fireEvent.ended(video)
      const card = await screen.findByTestId('recording-end-card')
      expect(within(card).getByText('次のエピソード · 3 秒後に再生')).toBeInTheDocument()
      await user.click(within(card).getByRole('button', { name: '取り消す' }))
      await act(async () => vi.advanceTimersByTimeAsync(3000))
      expect(router.state.location.pathname).toBe('/recordings/3')
      expect(screen.queryByTestId('recording-end-card')).not.toBeInTheDocument()

      fireEvent.ended(await screen.findByLabelText('録画映像'))
      await screen.findByTestId('recording-end-card')
      await act(async () => vi.advanceTimersByTimeAsync(3000))
      expect(await screen.findByRole('heading', { name: '作品X 第2話' })).toBeInTheDocument()
      expect(router.state.location.pathname).toBe('/recordings/4')
    } finally {
      vi.useRealTimers()
    }
  })

  it('終端カード中にページを離れると自動遷移タイマーを破棄する', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const origin = sampleRecording({
        id: 3,
        title: '作品X 第1話',
        series: '作品X',
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
      })
      const next = sampleRecording({
        id: 4,
        title: '作品X 第2話',
        series: '作品X',
        sizeBytes: 1_000_000,
        encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
        startAt: '2026-01-08T12:00:00Z',
      })
      createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
      const { router } = renderAt('/recordings/3')
      fireEvent.ended(await screen.findByLabelText('録画映像'))
      expect(await screen.findByText('次のエピソード · 3 秒後に再生')).toBeInTheDocument()

      cleanup()
      await act(async () => vi.advanceTimersByTimeAsync(3000))
      expect(router.state.location.pathname).toBe('/recordings/3')
    } finally {
      vi.useRealTimers()
    }
  })

  // issue #467: PageHeader の leading スロットに乗せても「戻る」は
  // history.back ではなくリンク（一覧へ）のまま変えない。
  it('「戻る」は /recordings へのリンク（history.back ではない）', async () => {
    createFakeServer({ recording: sampleRecording() })

    renderAt('/recordings/3')

    expect(await screen.findByRole('link', { name: '戻る' })).toHaveAttribute('href', '/recordings')
  })

  it('詳細を開いただけでは video の再生を開始しない（.play() を呼ばない）', async () => {
    const playSpy = vi
      .spyOn(window.HTMLMediaElement.prototype, 'play')
      .mockResolvedValue(undefined)
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }] }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('region', { name: '再生' })
    expect(playSpy).not.toHaveBeenCalled()
  })

  // issue #283: 多サイトのときは詳細のチャンネル欄にも site を出す。
  it('多サイトのときは詳細のチャンネル欄に site を出す', async () => {
    createFakeServer({
      sites: ['default', 'site2'],
      recording: sampleRecording({ site: 'site2' }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    await selectDetailTab('番組')
    expect(screen.getByText(/ＯＨＫ（GR 27）.*site2/)).toBeInTheDocument()
  })

  it('単一サイトのときは詳細に site を出さない', async () => {
    createFakeServer({ sites: ['default'], recording: sampleRecording() })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    await selectDetailTab('番組')
    expect(screen.getByText(/ＯＨＫ（GR 27）/)).toBeInTheDocument()
    expect(screen.queryByText(/default/)).not.toBeInTheDocument()
  })

  it('レジストリから消えた site の録画も詳細のチャンネル欄に site を出す', async () => {
    createFakeServer({
      sites: ['default'],
      recording: sampleRecording({ site: 'site2' }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    await selectDetailTab('番組')
    expect(screen.getByText(/ＯＨＫ（GR 27）.*site2/)).toBeInTheDocument()
  })

  it('存在しない id は「録画が見つかりません」を表示する', async () => {
    createFakeServer({ recording: null })

    renderAt('/recordings/999999')

    expect(await screen.findByText('録画が見つかりません')).toBeInTheDocument()
  })

  it('追っかけタイムラインは番組時刻を表示し、録画開始からの範囲でキー確定する', async () => {
    const now = Date.now()
    const startAt = new Date(now - 60 * 60_000).toISOString()
    const startedAt = new Date(now - 2 * 60_000).toISOString()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({
        startAt,
        durationMs: 2 * 60 * 60_000,
        status: 'recording',
        startedAt,
      }),
    })

    renderAt('/recordings/3#chase')

    const slider = await screen.findByRole('slider', { name: '追っかけ再生の位置' })
    expect(slider).toHaveAttribute('max', '7200')
    expect(Number(slider.getAttribute('aria-valuemax'))).toBeLessThan(300)
    expect(slider).toHaveAttribute('aria-valuetext', `${formatTime(startAt)}（開始から0秒）`)
    expect(screen.queryByRole('button', { name: '最新' })).not.toBeInTheDocument()

    fireEvent.change(slider, { target: { value: '30' } })
    expect(slider).toHaveAttribute('aria-valuenow', '30')
    const offsetPlaylistRequested = () =>
      fetchMock.mock.calls.some(([input]) =>
        new URL(String(input), 'http://localhost').pathname.endsWith(
          '/chase/offset/30/playlist.m3u8',
        ),
      )
    expect(offsetPlaylistRequested()).toBe(false)

    fireEvent.keyUp(slider, { key: 'ArrowRight' })
    await waitFor(() => expect(offsetPlaylistRequested()).toBe(true))
  })

  // ごみ箱の録画も 200 で返る（getRecording の openapi.yaml description の決定）
  // が、単体ページでは再生系を一切出さない（M3-18）。encodedAssets /
  // sizeBytes を敢えて持たせても出ないことを見て、判定が deletedAt の有無で
  // 効いていることを確かめる。
  it('ごみ箱の録画は 200 で開くが再生系を一切出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-02T00:00:00Z',
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        sizeBytes: 1_000_000,
      }),
    })

    renderAt('/recordings/3')

    // 展開内容（削除日時）が出るまで待ってから「無い」ことを確認する
    // （クエリ未解決のうちに queryBy で通ってしまう空虚な成功を避ける）
    await screen.findByText('削除日時')

    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
    expect(document.querySelector('img')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /ダウンロード \/ VLC/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  // 単体ページ固有の経路: 単体ページ自身のクエリキー（recordingDetailQueryKey）
  // は先頭要素を一覧と同じ '/api/recordings' に揃えてあるので、RecordingActions
  // の invalidate（'/api/recordings' 前方一致）がこのページのキャッシュも
  // 自動的に巻き込む。ここが効いていなければ、削除してもこの画面は古い
  // （生きている）表示のまま固まる。
  it('ごみ箱へ移すと、ナビゲーションなしで自分自身が再生系無しの表示に更新される', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('region', { name: '再生' })

    await openRecordingMenu()
    await user.click(screen.getByRole('menuitem', { name: 'ごみ箱へ' }))

    await waitFor(() => expect(screen.getByText('復元')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
  })

  // 回帰テスト（issue #232 のレビューで実機再現）: RecordingDetail の下には
  // 削除系（RecordingActions）だけでなく事後エンコード追加
  // （AddEncodeProfilesAction）というもう 1 人の mutater がいる。単体ページの
  // 再検証を「一覧の invalidate に前方一致するクエリキー」で構造的に解決して
  // いれば、AddEncodeProfilesAction 側を一切変更しなくてもここが効くはず ---
  // それを確かめる。
  it('事後エンコードを依頼すると、単体ページ自身を再検証し追加ボタンが消える', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000 }), // encodeProfiles 未指定 = 追加済み無し
      encodeProfiles: [{ name: 'web' }],
    })

    renderAt('/recordings/3')

    await user.click(await screen.findByRole('button', { name: /エンコードを追加/ }))
    const checkbox = await screen.findByRole('checkbox', { name: 'web' })
    await user.click(checkbox)
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    expect(await screen.findByText('エンコードを依頼しました')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('button', { name: /エンコードを追加/ })).not.toBeInTheDocument())
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/api/recordings/3')).toHaveLength(2)
  })
})

describe('RecordingDetailPage 原本保持ポリシー (issue #697)', () => {
  it('until_encoded への変更は不可逆操作の確認後に PATCH する', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    const select = await expandKeepOriginalEditor()
    expect(select).toHaveValue('always')
    await user.selectOptions(select, 'until_encoded')
    const saveButton = screen.getByRole('button', { name: '保持ポリシーを保存' })
    await user.click(saveButton)

    const dialog = await screen.findByRole('alertdialog', { name: 'エンコード後に原本を削除しますか？' })
    expect(dialog).toHaveTextContent(/原本の削除は\s+取り消せず、削除後は再エンコードできません。/)
    expect(
      fetchMock.mock.calls.filter(
        (call) =>
          String(call[0]).includes('/encode-policy') &&
          (call[1] as RequestInit | undefined)?.method === 'PATCH',
      ),
    ).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: 'エンコード後に削除する' }))

    expect(await screen.findByText('原本の保持ポリシーを変更しました')).toBeInTheDocument()
    await waitFor(() => expect(select).toHaveValue('until_encoded'))
    const patchCall = fetchMock.mock.calls.find(
      (call) =>
        String(call[0]).includes('/encode-policy') &&
        (call[1] as RequestInit | undefined)?.method === 'PATCH',
    )
    expect(patchCall).toBeDefined()
    if (!patchCall) throw new Error('encode policy PATCH was not sent')
    expect(JSON.parse(String((patchCall[1] as RequestInit).body))).toEqual({
      keepOriginal: 'until_encoded',
    })
  })

  it('desired プロファイルが空なら until_encoded を保存できない', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: [] }),
      encodeProfiles: [],
    })

    renderAt('/recordings/3')

    const select = await expandKeepOriginalEditor()
    await user.selectOptions(select, 'until_encoded')

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'エンコード後に原本を削除するには、プロファイルを 1 つ以上選んでください',
    )
    expect(screen.getByRole('button', { name: '保持ポリシーを保存' })).toBeDisabled()
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  // 回帰テスト: AlertDialogAction の onClick は commit() を呼んだ直後に Radix が
  // 同じイベントの中で onOpenChange(false) を発火させる。そのクロージャの時点
  // では setPolicy.isPending がまだ false（mutate の状態更新は次のレンダーでしか
  // 反映されない）なので、!setPolicy.isPending だけを条件に巻き戻すと、確定
  // 直後（PATCH が解決する前）に選択が current（変更前の値）へ戻ってしまう
  // （保存中… ボタンも一緒に消える）。PATCH をまだ解決させない状態で、選択が
  // until_encoded のままであることを確認する。
  it('確定した PATCH が未解決の間も、選択は変更後の値のまま巻き戻らない', async () => {
    const user = userEvent.setup()
    let resolvePatch!: (response: Response) => void
    const pendingPatch = new Promise<Response>((resolve) => {
      resolvePatch = resolve
    })
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }],
      encodePolicyResponse: () => pendingPatch,
    })

    renderAt('/recordings/3')

    const select = await expandKeepOriginalEditor()
    await user.selectOptions(select, 'until_encoded')
    await user.click(screen.getByRole('button', { name: '保持ポリシーを保存' }))
    await user.click(await screen.findByRole('button', { name: 'エンコード後に削除する' }))

    // PATCH がまだ解決していない中間状態。
    expect(select).toHaveValue('until_encoded')
    expect(screen.getByRole('button', { name: '保存中…' })).toBeInTheDocument()

    resolvePatch(jsonResponse(null, 204))
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '保存中…' })).not.toBeInTheDocument(),
    )
    expect(select).toHaveValue('until_encoded')
  })

  // 原本が無い録画には保持操作を出さず、版タブも空なら表示しない。
  it('原本が無い（sizeBytes 省略）録画では保持ポリシーの選択肢を出さない', async () => {
    createFakeServer({
      recording: sampleRecording({ encodeProfiles: [] }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByLabelText('原本の保持')).not.toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: '版' })).not.toBeInTheDocument()
  })
})

describe('RecordingDetailPage CM 検出の有効化導線', () => {
  it('cm_detect.enabled が false のデプロイでは、無効な録画に「検出を有効化」を出さない', async () => {
    const { fetchMock } = createFakeServer({ recording: sampleRecording({ sizeBytes: 1_000_000 }) })

    renderAt('/recordings/3')

    // 能力 API の応答が反映されるまで待つ（解決前の「まだ無い」で空虚に通さない）
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/capabilities'), expect.anything()))
    await openRecordingMenu()
    expect(screen.queryByRole('menuitem', { name: 'CM 検出を有効にする' })).not.toBeInTheDocument()
  })

  it('cm_detect.enabled が true なら「検出を有効化」が出る', async () => {
    createFakeServer({ recording: sampleRecording({ sizeBytes: 1_000_000 }), cmDetectCapability: true })

    renderAt('/recordings/3')

    await openRecordingMenu()
    expect(screen.getByRole('menuitem', { name: 'CM 検出を有効にする' })).toBeInTheDocument()
  })

  it.each([
    ['logo', true],
    ['area', true],
    ['setup', false],
    [undefined, false],
  ] as const)(
    '失敗工程 %s は記録タブに説明を出し、枠の導線を工程に合わせ、やり直しは見出しメニューに置く',
    async (stage, linkExpected) => {
    createFakeServer({
      recording: sampleRecording({
        sizeBytes: 1_000_000,
        cmDetection: { state: 'failed', ...(stage === undefined ? {} : { stage }) },
      }),
      cmDetectCapability: true,
    })

    renderAt('/recordings/3')

    await selectDetailTab('記録')
    const cmRow = screen.getAllByTestId('recording-diagnostic-row').find((row) => row.textContent?.includes('CM 検出'))
    expect(cmRow).toHaveTextContent(cmDetectStageMessage(stage))
    // logo / area は枠を教えるのが直し方（ロゴ登録画面へ録画の局と録画 id を渡す）。それ以外には出さない。
    const logoLink = within(cmRow!).queryByRole('link', { name: 'CM 検出のロゴを教える' })
    if (linkExpected) {
      expect(logoLink).toHaveAttribute('href', '/cm-logos?network=32678&service=5168&recording=3')
    } else {
      expect(logoLink).not.toBeInTheDocument()
    }
    await openRecordingMenu()
    expect(screen.getByRole('menuitem', { name: 'CM 検出をやり直す' })).toBeInTheDocument()
    },
  )

  it('失敗の技術的な詳細は既定で閉じ、開くと改行を保ったまま表示する', async () => {
    const user = userEvent.setup()
    const error = 'ffmpeg: first line\nlogoframe: second line'
    createFakeServer({
      recording: sampleRecording({
        sizeBytes: 1_000_000,
        cmDetection: { state: 'failed', stage: 'logo', error },
      }),
      cmDetectCapability: true,
    })

    renderAt('/recordings/3')

    await selectDetailTab('記録')
    const details = await screen.findByTestId('cm-detection-technical-details')
    expect((details as HTMLDetailsElement).open).toBe(false)
    expect(details.querySelector('pre')).not.toBeNull()

    await user.click(screen.getByText('技術的な詳細'))
    expect((details as HTMLDetailsElement).open).toBe(true)
    expect(details.querySelector('pre')?.textContent).toBe(error)
  })
})

describe('RecordingDetailPage の検出器結果', () => {
  const rawRanges = [{ startMs: 10_000, endMs: 20_000 }]
  const autoChapters: RecordingChapters = {
    version: 'chapters-v1',
    detectionPending: false,
    source: 'auto',
    spans: [],
  }

  it('検出器の生区間が空なら一文だけを出して詳細を作らない', async () => {
    createFakeServer({ recording: sampleRecording({ cmDetection: { state: 'detected', ranges: [] } }) })
    renderAt('/recordings/3')
    await selectDetailTab('記録')
    expect(await screen.findByText('0 区間')).toBeInTheDocument()
    expect(screen.queryByTestId('cm-detector-results-details')).not.toBeInTheDocument()
  })

  it('チャプターを編集できない原本だけの録画では検出器の区間を表示する', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 500, cmDetection: { state: 'detected', ranges: rawRanges } }),
    })
    renderAt('/recordings/3')
    await selectDetailTab('記録')
    const details = await screen.findByTestId('cm-detector-results-details')
    expect(details.querySelector('summary')).toHaveTextContent('検出器の結果')
    expect(details.textContent).toContain('00:00:10 – 00:00:20')
  })

  it('チャプター取得中は検出器の詳細を表示しない', async () => {
    let resolveChapters!: (response: Response) => void
    const response = new Promise<Response>((resolve) => {
      resolveChapters = resolve
    })
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500 }],
        cmDetection: { state: 'detected', ranges: rawRanges },
      }),
      chaptersResponse: () => response,
    })
    renderAt('/recordings/3')
    await screen.findByTestId('recording-assets-group')
    await selectDetailTab('記録')
    expect(screen.queryByTestId('cm-detector-results-details')).not.toBeInTheDocument()
    await act(async () => resolveChapters(jsonResponse(autoChapters)))
    expect(await screen.findByTestId('cm-detector-results-details')).toBeInTheDocument()
  })

  it('チャプター取得エラーでは検出器の区間を表示する', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500 }],
        cmDetection: { state: 'detected', ranges: rawRanges },
      }),
      chaptersResponse: () => Promise.resolve(jsonResponse({ error: 'unavailable' }, 500)),
    })
    const { queryClient } = renderAt('/recordings/3')
    await waitFor(() => {
      expect(queryClient.getQueryState(getGetRecordingChaptersQueryKey(3))?.status).toBe('error')
    })
    await selectDetailTab('記録')
    expect(await screen.findByTestId('cm-detector-results-details')).toBeInTheDocument()
  })

  it('ユーザーが保存したチャプターがあれば検出器の区間を表示する', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500 }],
        cmDetection: { state: 'detected', ranges: rawRanges },
      }),
      chapters: { ...autoChapters, source: 'user', spans: [{ startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }] },
    })
    renderAt('/recordings/3')
    await selectDetailTab('記録')
    expect(await screen.findByTestId('cm-detector-results-details')).toBeInTheDocument()
  })

  it('自動チャプターが空なら検出器の区間を表示する', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500 }],
        cmDetection: { state: 'detected', ranges: rawRanges },
      }),
      chapters: autoChapters,
    })
    renderAt('/recordings/3')
    await selectDetailTab('記録')
    expect(await screen.findByTestId('cm-detector-results-details')).toBeInTheDocument()
  })

  it('自動チャプターに区間があれば検出器の詳細を省く', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500 }],
        cmDetection: { state: 'detected', ranges: rawRanges },
      }),
      chapters: {
        ...autoChapters,
        spans: [{ startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }],
      },
    })
    const { queryClient } = renderAt('/recordings/3')
    await waitFor(() => {
      expect(queryClient.getQueryState(getGetRecordingChaptersQueryKey(3))?.status).toBe('success')
    })
    await selectDetailTab('記録')
    expect(screen.queryByTestId('cm-detector-results-details')).not.toBeInTheDocument()
  })

  it('検出状態でなければ検出器結果を表示しない', async () => {
    createFakeServer({ recording: sampleRecording({ cmDetection: { state: 'disabled' } }) })
    renderAt('/recordings/3')
    await selectDetailTab('記録')
    expect(await screen.findByText(/無効/)).toBeInTheDocument()
    expect(screen.queryByTestId('cm-detector-results-details')).not.toBeInTheDocument()
    expect(screen.queryByText('0 区間')).not.toBeInTheDocument()
  })
})

describe('RecordingDetailPage の追加エンコード導線', () => {
  it('版タブの保持と追加エンコード操作は、押したときだけ詳細フォームを開く', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: [] }),
      encodeProfiles: [{ name: 'h264' }],
    })
    const { router } = renderAt('/recordings/3#review')
    const assets = await screen.findByTestId('recording-assets-group')
    expect(screen.getByText('原本: 常に保持')).toBeInTheDocument()
    expect(screen.queryByLabelText('原本の保持')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'h264' })).not.toBeInTheDocument()

    const keepToggle = screen.getByRole('button', { name: 'この回だけ変える' })
    await user.click(keepToggle)
    const keepOriginal = await screen.findByRole('combobox', { name: '原本の保持' })
    await user.selectOptions(keepOriginal, 'until_encoded')
    expect(keepOriginal).toHaveValue('until_encoded')

    const originalHash = router.state.location.hash
    await user.click(await screen.findByRole('button', { name: /エンコードを追加/ }))
    expect(await screen.findByRole('checkbox', { name: 'h264' })).toBeInTheDocument()
    expect(assets).toBeInTheDocument()
    expect(router.state.location.hash).toBe(originalHash)
  })

  it('プロファイル未設定なら空の追加エンコード枠を作らない', async () => {
    createFakeServer({ recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: [] }) })
    renderAt('/recordings/3')
    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByRole('button', { name: /エンコードを追加/ })).not.toBeInTheDocument()
    expect(screen.queryByText('すべてのエンコードプロファイルが追加済みです。')).not.toBeInTheDocument()
  })

  it.each([
    ['設定済み', { encodeProfiles: ['h264'] }, []],
    ['変換済み資産あり', { encodeProfiles: [] }, [{ profile: 'h264', sizeBytes: 500 }]],
  ])('%s の録画では追加できるプロファイルが無く、空の枠を出さない', async (_name, overrides, encodedAssets) => {
    createFakeServer({
      recording: sampleRecording({
        ...overrides,
        encodedAssets,
        sizeBytes: 1_000_000,
      }),
      encodeProfiles: [{ name: 'h264' }],
    })
    renderAt('/recordings/3')
    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByRole('button', { name: /エンコードを追加/ })).not.toBeInTheDocument()
    expect(screen.queryByText('すべてのエンコードプロファイルが追加済みです。')).not.toBeInTheDocument()
  })
})

// 一覧の常時「再生」列とインライン展開を廃し、視聴・削除・エンコードは詳細ページに
// 寄せた（issue #311）。従来これらは録画一覧の行展開で試していたが、展開が無くなった
// ので、共有部品（RecordingActions / AddEncodeProfilesAction / RuleSection /
// RecordingPlayer）のテストは唯一の呼び出し元になった詳細ページへ移した。
describe('RecordingDetailPage 削除・復元のトースト (issue #297)', () => {
  it('ごみ箱へ移すと Undo 付きトーストが出て、「元に戻す」でライブラリ表示に戻る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    await openRecordingMenu()
    await user.click(screen.getByRole('menuitem', { name: 'ごみ箱へ' }))

    // 自己 invalidate（recordingDetailQueryKey の前方一致）で trash 表示に変わる
    await screen.findByRole('button', { name: '復元' })
    // ごみ箱送りは復元で即座に取り消せる安価な操作なので Undo 付きトーストにする。
    expect(await screen.findByText('ごみ箱に移しました')).toBeInTheDocument()

    // Undo（復元）でライブラリ表示（再生・ごみ箱へ）に戻る
    await user.click(screen.getByRole('button', { name: '元に戻す' }))
    await openRecordingMenu()
    expect(screen.getByRole('menuitem', { name: 'ごみ箱へ' })).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
  })

  it('復元しても成功トーストは出ない（効果は表示の切替で常に見える）', async () => {
    const user = userEvent.setup()
    createFakeServer({ recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }) })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: '復元' }))

    await waitFor(() => expect(screen.getByRole('button', { name: '録画のその他の操作' })).toBeInTheDocument())
    expect(screen.queryByText('復元しました')).not.toBeInTheDocument()
  })

  // issue #457: 失敗時にサーバー本文（`apiErrorMessage`）を汎用文言に付ける。
  // 本文を含まない `{ error: 'server error' }` を返すと「削除に失敗しました」
  // だけに戻ってしまい、この揃えを壊しても検知できない ---
  // 期待値の本文（'server error'）は実際に応答へ載せているものと同じにする。
  it('ごみ箱へ移す操作が失敗すれば、サーバー本文つきの失敗トーストが出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording(),
      deleteResponse: () => jsonResponse({ error: 'server error' }, 500),
    })

    renderAt('/recordings/3')
    await openRecordingMenu()
    await user.click(screen.getByRole('menuitem', { name: 'ごみ箱へ' }))

    expect(await screen.findByText('削除に失敗しました: server error')).toBeInTheDocument()
    // 失敗したので表示は変わらない
    expect(screen.getByRole('button', { name: '録画のその他の操作' })).toBeInTheDocument()
  })

  /**
   * 呼び出し側の `kind: 'error'` を固定するテスト（U-5 とのマージで
   * `toaster.tsx` の機構だけが残り呼び出し側の `kind: 'error'` が静かに
   * 消えても、`pnpm test` は緑のままになりうるため）。`kind` を落とす
   * 変異（`toast({ message })` に戻す）で実際に落ちることを確認済み
   * （報告参照）。
   */
  it('ごみ箱へ移す失敗トーストは時間が経っても自動で消えない（kind: "error" の固定）', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
      createFakeServer({
        recording: sampleRecording(),
        deleteResponse: () => jsonResponse({ error: 'server error' }, 500),
      })

      renderAt('/recordings/3')
      await openRecordingMenu()
      await user.click(screen.getByRole('menuitem', { name: 'ごみ箱へ' }))
      expect(await screen.findByText('削除に失敗しました: server error')).toBeInTheDocument()

      // info/成功の表示時間（6 秒）を大きく超えても消えない
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000)
      })
      expect(screen.getByText('削除に失敗しました: server error')).toBeInTheDocument()
    } finally {
      vi.useRealTimers()
    }
  })

  it('復元操作が失敗すれば、サーバー本文つきの失敗トーストが出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
      restoreResponse: () => jsonResponse({ error: 'server error' }, 500),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: '復元' }))

    expect(await screen.findByText('復元に失敗しました: server error')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  // 本文が無い失敗（ネットワーク断・JSON でない応答）では末尾に「: 」を
  // 残さない（両方向の確認。issue #457 の受け入れ基準）。
  it('本文の無い失敗では末尾に「: 」を残さない', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording(),
      deleteResponse: () => new Response(null, { status: 500 }),
    })

    renderAt('/recordings/3')
    await openRecordingMenu()
    await user.click(screen.getByRole('menuitem', { name: 'ごみ箱へ' }))

    expect(await screen.findByText('削除に失敗しました')).toBeInTheDocument()
    expect(screen.queryByText(/削除に失敗しました: /)).not.toBeInTheDocument()
  })

  // 完全削除（purge）は他の 2 操作と違い確認ダイアログの確定操作から
  // 呼ばれる。issue #457 の揃え先 7 箇所のうち残る 1 箇所。
  it('完全削除の予約が失敗すれば、サーバー本文つきの失敗トーストが出る', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
      purgeResponse: () => jsonResponse({ error: 'server error' }, 500),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: '録画のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '今すぐ完全削除' }))
    await user.click(await screen.findByRole('button', { name: '完全削除を予約する' }))

    expect(
      await screen.findByText('完全削除の予約に失敗しました: server error'),
    ).toBeInTheDocument()
  })

  // 完全削除（purge）は破壊的で、issue #311 以降は詳細ページからしか到達できない。
  // issue #467 で稀・破壊的な操作として overflow メニュー（⋮）へ寄せた ---
  // メニューに入れても確認 AlertDialog は残ることと、確定するまで purge を
  // 呼ばないことを固定する。
  it('「今すぐ完全削除」は overflow メニュー経由・確認ダイアログを挟み、確定するまで purge を呼ばない', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ deletedAt: '2026-01-02T00:00:00Z' }),
    })

    renderAt('/recordings/3')

    const purgeCalls = () =>
      fetchMock.mock.calls.filter((c) => String(c[0]).includes('/purge'))

    // 露出ボタンとしては出ない（overflow の中）。読み込みが解決してから
    // 「無い」ことを確認する（非同期の空虚な成功を避ける --- 未解決のうちは
    // ListSkeleton でどのボタンも出ておらず、この assertion が常に通ってしまう）。
    await screen.findByRole('button', { name: '復元' })
    expect(screen.queryByRole('button', { name: '今すぐ完全削除' })).not.toBeInTheDocument()

    await user.click(await screen.findByRole('button', { name: '録画のその他の操作' }))
    const purgeItem = await screen.findByRole('menuitem', { name: '今すぐ完全削除' })
    await user.click(purgeItem)
    // ボタンを押しただけでは purge は飛ばない（確認を挟む）
    expect(purgeCalls()).toHaveLength(0)
    // 確認ダイアログの説明文は「reconcile」等の内部実装用語を出さず、
    // 利用者が見るものの名前（原本・変換後のファイル・サムネイル）で言う
    // （issue #454）。
    expect(
      screen.getByText('この録画の原本・変換後のファイル・サムネイルを削除します。取り消せません。'),
    ).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    expect(screen.queryByRole('button', { name: '完全削除を予約する' })).not.toBeInTheDocument()
    expect(purgeCalls()).toHaveLength(0)
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: '録画のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '今すぐ完全削除' }))

    // 確定ボタンは取り返しがつかない操作の規約どおり destructive
    // （issue #467、alert-dialog.tsx の規約。variant を外すと落ちる）。
    const confirmButton = screen.getByRole('button', { name: '完全削除を予約する' })
    expect(confirmButton).toHaveClass('text-destructive')

    // ダイアログの確定ボタンで初めて purge が飛ぶ
    await user.click(confirmButton)
    await waitFor(() => expect(purgeCalls()).toHaveLength(1))
    expect(await screen.findByText('完全削除を予約しました')).toBeInTheDocument()
    // invalidate 後の単体 GET が 404 になり、詳細表示も消える
    expect(await screen.findByText('録画が見つかりません')).toBeInTheDocument()
    // 確定後はダイアログが閉じる
    expect(screen.queryByRole('button', { name: '完全削除を予約する' })).not.toBeInTheDocument()
  })
})

describe('RecordingDetailPage サイズが取れない資産（値札、issue #236）', () => {
  it('encoded 資産の sizeBytes が省略されていても、プロファイル名は出るがサイズは出さない', async () => {
    createFakeServer({ recording: sampleRecording({ encodedAssets: [{ profile: 'web' }] }) })
    const user = userEvent.setup()

    renderAt('/recordings/3')

    const region = await screen.findByRole('region', { name: '再生' })
    expect(document.querySelector('video')).toBeInTheDocument()
    await user.click(within(region).getByRole('button', { name: '再生設定' }))
    await user.click(within(region).getByRole('menuitem', { name: '画質' }))
    expect(within(region).getByRole('menuitemradio', { name: 'web' })).toBeInTheDocument()
    expect(region.textContent).not.toMatch(/\d+(\.\d+)? (B|KB|MB|GB|TB)/)
  })
})

// 事後追加のエンコード依頼（issue #133、凍結の例外）。原本の有無 / 追加済みの
// 除外 / 409 翻訳 / ごみ箱で出さない、をそれぞれ固定する（送信成功は上の
// 「事後エンコードを依頼すると…」で確認済み）。
describe('RecordingDetailPage 事後エンコード追加 (AddEncodeProfilesAction)', () => {
  it('encodeProfiles（desired）にあるものは選択肢から外し、開いたフォームにだけ表示する', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }, { name: 'h265' }],
    })

    renderAt('/recordings/3')

    await userEvent.setup().click(await screen.findByRole('button', { name: /エンコードを追加/ }))
    expect(screen.getByText('追加済み: h264')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'h264' })).not.toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'h265' })).toBeInTheDocument()
  })

  it('全プロファイルが追加済みなら、内容の無い追加枠を出さない', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodeProfiles: ['h264'] }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.queryByText('すべてのエンコードプロファイルが追加済みです。')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /エンコードを追加/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '追加エンコードを依頼' })).not.toBeInTheDocument()
  })

  it('原本が無い録画では版タブを作らず、追加エンコードも出さない', async () => {
    createFakeServer({
      recording: sampleRecording({ encodeProfiles: [] }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    await screen.findByRole('heading', { name: '単体ページの録画' })
    expect(screen.queryByRole('tab', { name: '版' })).not.toBeInTheDocument()
    expect(screen.queryByText(/原本/)).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  // issue #271: 409 は「原本が active でない」の hedge 文言で、サーバーは英語のまま
  // 返す。`hasOriginal` の近似が破れて 409 になっても、英語文字列を出さず日本語に
  // 翻訳することを固定する。
  it('409 応答は英語のサーバー文字列を出さず、日本語の文言に翻訳する', async () => {
    const user = userEvent.setup()
    const rawEnglishMessage =
      'original media asset not active (deleted, deleting, or not yet ingested); cannot add encode profiles'
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 500, encodeProfiles: [] }),
      encodeProfiles: [{ name: 'h264' }],
      encodePostResponse: () => jsonResponse({ error: rawEnglishMessage }, 409),
    })

    renderAt('/recordings/3')
    await user.click(await screen.findByRole('button', { name: /エンコードを追加/ }))
    await user.click(await screen.findByRole('checkbox', { name: 'h264' }))
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    expect(
      await screen.findByText(
        '原本の状態が変わったため追加できませんでした（削除済み・削除処理中・未取り込みのいずれか）。画面を更新してから再度お試しください。',
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText(rawEnglishMessage)).not.toBeInTheDocument()
    expect(screen.queryByText(/deleted, deleting, or not yet ingested/)).not.toBeInTheDocument()
  })

  it('ごみ箱では追加エンコードのコントロールを一切出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-05T00:00:00Z',
        sizeBytes: 500,
        encodeProfiles: [],
      }),
      encodeProfiles: [{ name: 'h264' }],
    })

    renderAt('/recordings/3')

    await screen.findByText('削除日時')
    expect(screen.queryByRole('button', { name: /エンコードを追加/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })
})

// issue #230（M6-2）: 録画 → ルールの導線。ruleId の有無で出し分け、ルール一覧に
// まだ ruleId が載っていない一時的な状態でも #N に落ちて壊れないことを固定する。
describe('RecordingDetailPage ルール導線 (issue #230)', () => {
  it('ruleId がある録画は「ルール」セクションを出し、ルール名がリンクになる', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rules: [sampleRule({ id: 5, name: 'ニュース全部' })],
    })

    renderAt('/recordings/3')

    await selectDetailTab('番組')
    // 録画のしかたの行に「ルール「名前」」の形で出る（見出しにしない）。
    expect((await screen.findByText('録画のしかた')).nextElementSibling).toHaveTextContent('ルール「ニュース全部」')
    expect(await screen.findByRole('link', { name: 'ニュース全部' })).toHaveAttribute(
      'href',
      '/search?ruleId=5',
    )
    expect(screen.getByRole('link', { name: 'このルールの録画で絞る' })).toHaveAttribute(
      'href',
      '/recordings?ruleId=5',
    )
  })

  it('同名のルールは詳細のリンクでも id を添えて押し分ける', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rules: [sampleRule({ id: 5, name: '同名ルール' }), sampleRule({ id: 6, name: '同名ルール' })],
    })

    renderAt('/recordings/3')

    await selectDetailTab('番組')
    expect(await screen.findByRole('link', { name: '同名ルール (#5)' })).toHaveAttribute(
      'href',
      '/search?ruleId=5',
    )
  })

  it('ruleId が無い録画には「ルール」セクションを出さない（手動予約由来）', async () => {
    createFakeServer({ recording: sampleRecording({ source: 'manual' }) })

    renderAt('/recordings/3')

    await selectDetailTab('番組')
    await screen.findByText('録画のしかた')
    expect(screen.queryByText(/^ルール「/)).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'このルールの録画で絞る' })).not.toBeInTheDocument()
  })

  // ルール一覧にその id が無い（新規作成直後でキャッシュが追いついていない等の
  // 一時的な状態。ルール削除では FK の ON DELETE SET NULL で ruleId 自体が省略され
  // セクションごと消えるので、これは削除の経路ではない）。`rules.find` が空を
  // 返す間は #N 表記に落ちる。
  it('ルール一覧にまだ載っていない ruleId でも #N 表記に落ちて壊れない', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 99, source: 'rule' }),
      rules: [sampleRule({ id: 1, name: '既知のルール' })],
    })

    renderAt('/recordings/3')

    await selectDetailTab('番組')
    await screen.findByText('録画のしかた')
    expect(await screen.findByRole('link', { name: '#99' })).toHaveAttribute(
      'href',
      '/search?ruleId=99',
    )
  })

  it('ルール一覧が未解決の間は #N を出し、解決後にルール名へ差し替わる', async () => {
    let resolveRules!: (response: Response) => void
    const pendingRules = new Promise<Response>((resolve) => {
      resolveRules = resolve
    })
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rulesResponse: () => pendingRules,
    })

    renderAt('/recordings/3')

    await selectDetailTab('番組')
    // 録画単体は解決済みだがルール一覧は未解決の状態を明示的に作る。
    await screen.findByText('単体ページの録画')
    expect(screen.getByRole('link', { name: '#5' })).toHaveAttribute('href', '/search?ruleId=5')

    resolveRules(jsonResponse([sampleRule({ id: 5, name: '後から出るルール' })]))

    expect(await screen.findByRole('link', { name: '後から出るルール' })).toHaveAttribute(
      'href',
      '/search?ruleId=5',
    )
    expect(screen.queryByRole('link', { name: '#5' })).not.toBeInTheDocument()
  })
})

/**
 * 追っかけ再生の画質（プロファイル）切替（issue #874）。
 *
 * **置き場所は `/recordings/$id` の search（`?liveProfile=`）である。**
 * 追っかけは `/recordings/{id}#chase` にあり、`#chase` 側（ハッシュ）に持たせると
 * `pages/recording-detail.tsx` の `key={`${recording.id}:${location.hash}`}` が
 * 変わり、`RecordingDetail` ごと作り直されて再生位置が先頭に戻る。`profile` という
 * 名前を使わないのは、この画面に `encode.profiles`（VOD）と `live.profiles`
 * （追っかけ）の 2 軸が同居していて、素の `profile` ではどちらか読めないためである。
 *
 * ここで見るのは配線だけである --- 実再生と「切替で位置が巻き戻らないこと」は
 * `components/live-player.test.tsx` と `web/e2e/chase.mjs` の担い。
 */
describe('RecordingDetailPage / 追っかけの画質（issue #874）', () => {
  const LIVE_PROFILES: LiveProfileSummary[] = [
    { name: 'hd', height: 720 },
    { name: 'sd', height: 480 },
  ]

  /** chasePlaylistURLs は追っかけのプレイリスト要求 URL（呼ばれた順）。 */
  type FetchMock = { mock: { calls: [string | URL | Request, RequestInit?][] } }

  function chasePlaylistURLs(fetchMock: FetchMock): string[] {
    return fetchMock.mock.calls
      .map(([url]) => String(url))
      .filter((url) => url.includes('/chase') && url.includes('playlist.m3u8'))
  }

  /**
   * chaseLeaveURLs は離脱ヒント（`POST .../chase/leave`）の宛先。jsdom には
   * `navigator.sendBeacon` が無いので、`keepalive` つきの POST にフォールバックする。
   */
  function chaseLeaveURLs(fetchMock: FetchMock): string[] {
    return fetchMock.mock.calls
      .filter(([, init]) => init?.method === 'POST')
      .map(([url]) => String(url))
      .filter((url) => url.endsWith('/leave'))
  }

  /** chaseRecording は追っかけを出せる録画（録画中 + 追っかけの VOD プロファイル）。 */
  function chaseRecording(): Recording {
    const now = Date.now()
    return sampleRecording({
      startAt: new Date(now - 60 * 60_000).toISOString(),
      startedAt: new Date(now - 2 * 60_000).toISOString(),
      durationMs: 2 * 60 * 60_000,
      status: 'recording',
      // VOD 側のプロファイル。再生位置のキーはこちらで作る（画質とは別の軸）
      encodeProfiles: ['vod-h264'],
    })
  }

  /**
   * 切替の配線（受け入れ 6）。`?profile=` の値が `chasePlaylistURL` に届くこと、
   * 切替で離脱ヒントが飛ばないこと、再開位置が recording 単位で API に保存されることを見る。
   */
  it('画質を切り替えると ?profile= が要求に載り、離脱ヒントも位置のキーも変えない', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: chaseRecording(),
      liveProfiles: LIVE_PROFILES,
    })

    renderAt('/recordings/3#chase')

    const select = await screen.findByLabelText('画質')
    // 既定はサーバー側と同じ先頭。表示名は height を添える
    expect(select).toHaveValue('hd')
    expect(screen.getByRole('option', { name: 'hd（720p）' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'sd（480p）' })).toBeInTheDocument()

    // 既定は URL に書き戻さない（`?profile=` を付けずサーバー側の先頭に任せる）
    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(chasePlaylistURLs(fetchMock)[0]).not.toContain('profile=')

    await user.selectOptions(select, 'sd')

    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(2))
    expect(chasePlaylistURLs(fetchMock)[1]).toContain('profile=sd')
    // **切替はセッションを手放す合図ではない** --- 追っかけのセッション鍵は
    // `(recordingID, offset)` でプロファイルを含まない（同じセッションの
    // 別プレイリストを取るだけ）。
    expect(chaseLeaveURLs(fetchMock)).toEqual([])

    // 再開位置は画質ではなく recording 単位で保存する。
    const video = document.querySelector('video')!
    fireEvent.canPlay(video)
    video.currentTime = 12
    fireEvent.pause(video)
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url) === '/api/recordings/3/playback-position'),
      ).toHaveLength(1)
    })
    const positionWrite = fetchMock.mock.calls.find(
      ([url]) => String(url) === '/api/recordings/3/playback-position',
    )
    expect(positionWrite?.[1]).toEqual(
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ positionMs: 12_000 }) }),
    )
  })

  /** 選ぶ余地が無いのに出すと「機能しないコントロール」に戻る（issue #209 の規律）。 */
  it('一覧が 1 件ならセレクタを出さず、既定のプロファイルで再生する', async () => {
    const { fetchMock } = createFakeServer({
      recording: chaseRecording(),
      liveProfiles: [{ name: 'hd', height: 720 }],
    })

    renderAt('/recordings/3#chase')

    await screen.findByRole('region', { name: '追っかけ再生' })
    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(screen.queryByLabelText('画質')).not.toBeInTheDocument()
    expect(chasePlaylistURLs(fetchMock)[0]).not.toContain('profile=')
  })

  /**
   * **一覧は選択肢を出すためだけのもので、再生の前提条件ではない。** 取得失敗
   * （0 件）でも追っかけは既定のプロファイルで動き続ける。
   */
  it('一覧が 0 件でもセレクタを出さず、既定のプロファイルで再生する', async () => {
    const { fetchMock } = createFakeServer({ recording: chaseRecording(), liveProfiles: [] })

    renderAt('/recordings/3#chase')

    await screen.findByRole('region', { name: '追っかけ再生' })
    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(screen.queryByLabelText('画質')).not.toBeInTheDocument()
  })

  /** 直リンク（受け入れ: 復元の両方向）。有効な `?liveProfile=` は選択状態として復元される。 */
  it('直リンクの ?liveProfile= が選択状態として復元される', async () => {
    const { fetchMock } = createFakeServer({
      recording: chaseRecording(),
      liveProfiles: LIVE_PROFILES,
    })

    renderAt('/recordings/3?liveProfile=sd#chase')

    expect(await screen.findByLabelText('画質')).toHaveValue('sd')
    // **要求に実際に載ることまで見る。** セレクタの表示だけだと、URL の値を
    // そのまま握って選択肢に無い値でも「先頭が選ばれて見える」状態と区別できない
    // （React の controlled `<select>` は一致しない値で先頭に落ちるだけ）。
    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(chasePlaylistURLs(fetchMock)[0]).toContain('profile=sd')
  })

  /**
   * **未知の名前は落ちて既定（先頭）になる。** streamer は一覧に無い名前を
   * 400（`unknown chase profile`）で返すので、落とさないと綴り違いの共有リンク・
   * 古いブックマークがエラー画面になる（`lib/live.ts` の `validLiveProfile`）。
   */
  it('未知の ?liveProfile= は既定に落ちる（400 を踏まない）', async () => {
    const { fetchMock } = createFakeServer({
      recording: chaseRecording(),
      liveProfiles: LIVE_PROFILES,
    })

    renderAt('/recordings/3?liveProfile=does-not-exist#chase')

    expect(await screen.findByLabelText('画質')).toHaveValue('hd')
    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(chasePlaylistURLs(fetchMock)[0]).not.toContain('profile=')
    expect(chasePlaylistURLs(fetchMock)[0]).not.toContain('does-not-exist')
  })

  /**
   * **URL が画質を名指ししているときは、一覧の到着を待ってから再生させる。**
   * 名指しされた値の実在は一覧が無いと確かめられないので、待たずに再生を始めると
   * `undefined → 'sd'` の変化で `LivePlayer` の effect が再実行され、追っかけが
   * 先頭からやり直しになる（`pages/live.tsx` の `waitingForProfileList` と同じ窓）。
   */
  it('URL が画質を名指ししているときは一覧の到着を待ち、届いても取り直さない', async () => {
    let releaseLiveProfiles!: (response: Response) => void
    const pending = new Promise<Response>((resolve) => {
      releaseLiveProfiles = resolve
    })
    const { fetchMock } = createFakeServer({
      recording: chaseRecording(),
      liveProfilesResponse: () => pending,
    })

    renderAt('/recordings/3?liveProfile=sd#chase')

    // 一覧が未解決の間はプレイリストを要求しない
    await screen.findByRole('slider', { name: '追っかけ再生の位置' })
    expect(chasePlaylistURLs(fetchMock)).toEqual([])

    await act(async () => {
      releaseLiveProfiles(jsonResponse(LIVE_PROFILES))
    })

    await waitFor(() => expect(chasePlaylistURLs(fetchMock)).toHaveLength(1))
    expect(chasePlaylistURLs(fetchMock)[0]).toContain('profile=sd')
    expect(chaseLeaveURLs(fetchMock)).toEqual([])
  })
})

describe('RecordingDetailPage / 原本 VOD HLS（issue #920）', () => {
  // アサーションが落ちても保存位置を次のテストへ持ち越さない
  afterEach(() => {
    localStorage.clear()
  })

  const LIVE_PROFILES: LiveProfileSummary[] = [
    { name: 'hd', height: 720 },
    { name: 'sd', height: 480 },
  ]

  function originalVODURLs(fetchMock: { mock: { calls: [string | URL | Request, RequestInit?][] } }) {
    return fetchMock.mock.calls
      .map(([url]) => String(url))
      .filter((url) => url.includes('/original-vod/playlist.m3u8'))
  }

  it('エンコードの無い完成録画は HLS で原本を再生し、プロファイルを同じ recording URL に渡す', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({
        startedAt: '2026-01-01T12:02:00Z',
        endedAt: '2026-01-01T12:30:00Z',
        durationMs: 60_000,
        sizeBytes: 1_000_000,
        encodeProfiles: ['vod-h264'],
        encodedAssets: [],
      }),
      liveProfiles: LIVE_PROFILES,
    })

    renderAt('/recordings/3')

    expect(await screen.findByRole('region', { name: '原本 TS をブラウザ再生' })).toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: '再生設定' }))
    expect(await screen.findByLabelText('画質')).toHaveValue('hd')
    expect(screen.getByRole('slider', { name: 'シークバー' })).toHaveAttribute('aria-valuemax', '1680')
    await waitFor(() => expect(originalVODURLs(fetchMock)).toHaveLength(1))
    expect(originalVODURLs(fetchMock)[0]).toBe(
      '/api/sites/default/recordings/3/original-vod/playlist.m3u8',
    )
    expect(screen.getByRole('link', { name: /ダウンロード \/ VLC/ })).toHaveAttribute(
      'href',
      '/api/media/recordings/3/file',
    )
    expect(screen.queryByText('ブラウザ再生用のエンコードがまだありません。')).not.toBeInTheDocument()

    await user.selectOptions(screen.getByLabelText('画質'), 'sd')
    await waitFor(() => expect(originalVODURLs(fetchMock)).toHaveLength(2))
    expect(originalVODURLs(fetchMock)[1]).toContain('profile=sd')
  })

  it('encode profile が無い録画でも再開位置は画質によらず recording 単位で保存する', async () => {
    const user = userEvent.setup()
    const { fetchMock } = createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodedAssets: [] }),
      liveProfiles: LIVE_PROFILES,
    })
    renderAt('/recordings/3')
    await screen.findByRole('region', { name: '原本 TS をブラウザ再生' })
    await user.click(await screen.findByRole('button', { name: '再生設定' }))
    await screen.findByLabelText('画質')
    await waitFor(() => expect(document.querySelector('video')).toBeInTheDocument())

    const video = document.querySelector('video')!
    Object.defineProperty(video, 'currentTime', { value: 30, writable: true, configurable: true })
    fireEvent.pause(video)
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url) === '/api/recordings/3/playback-position'),
      ).toHaveLength(1)
    })

    await user.selectOptions(screen.getByLabelText('画質'), 'sd')
    await waitFor(() => expect(screen.getByLabelText('画質')).toHaveValue('sd'))
    const after = document.querySelector('video')!
    Object.defineProperty(after, 'currentTime', { value: 40, writable: true, configurable: true })
    fireEvent.pause(after)
    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url) === '/api/recordings/3/playback-position'),
      ).toHaveLength(2)
    })
    const writes = fetchMock.mock.calls.filter(([url]) => String(url) === '/api/recordings/3/playback-position')
    expect(writes.map(([url, init]) => [String(url), init?.body])).toEqual([
      ['/api/recordings/3/playback-position', JSON.stringify({ positionMs: 30_000 })],
      ['/api/recordings/3/playback-position', JSON.stringify({ positionMs: 40_000 })],
    ])
  })

  /**
   * 受け入れ「追っかけ / 原本 VOD の切替で録画 ID と再生位置の対応が壊れない」。
   * encode profile が無い録画でも、録画中に追っかけで保存した位置を録画終了後の
   * 原本 VOD が読む。キーの文字列ではなく、保存と復元が噛み合うことを見る。
   */
  it('encode profile の構成に依らず、追っかけで保存した位置を原本 VOD が復元する', async () => {
    const now = Date.now()
    const playbackState: { positionMs?: number } = {}
    const recording = sampleRecording({
      startAt: new Date(now - 60 * 60_000).toISOString(),
      startedAt: new Date(now - 2 * 60_000).toISOString(),
      durationMs: 2 * 60 * 60_000,
      sizeBytes: 1_000_000,
      encodedAssets: [],
      encodeProfiles: ['cut', 'h264'],
    })
    createFakeServer({
      recording: { ...recording, status: 'recording' },
      liveProfiles: LIVE_PROFILES,
      playbackState,
    })
    renderAt('/recordings/3#chase')
    await screen.findByRole('region', { name: '追っかけ再生' })
    await waitFor(() => expect(document.querySelector('video')).toBeInTheDocument())
    const chaseVideo = document.querySelector('video')!
    Object.defineProperty(chaseVideo, 'currentTime', { value: 42, writable: true, configurable: true })
    fireEvent.pause(chaseVideo)
    await waitFor(() => expect(playbackState.positionMs).toBe(42_000))
    cleanup()

    createFakeServer({
      recording: { ...recording, status: 'finished' },
      liveProfiles: LIVE_PROFILES,
      playbackState,
    })
    renderAt('/recordings/3')
    await screen.findByRole('region', { name: '原本 TS をブラウザ再生' })
    await waitFor(() => expect(document.querySelector('video')).toBeInTheDocument())
    const vodVideo = document.querySelector('video')!
    Object.defineProperty(vodVideo, 'currentTime', { value: 0, writable: true, configurable: true })
    fireEvent.loadedMetadata(vodVideo)
    expect(vodVideo.currentTime).toBe(42)
  })

  it('live profile が無い場合は HLS player を作らず、VLC リンクを残す', async () => {
    createFakeServer({
      recording: sampleRecording({ sizeBytes: 1_000_000, encodedAssets: [] }),
      liveProfiles: [],
    })

    renderAt('/recordings/3')

    expect(await screen.findByText(/HLS 再生プロファイルを利用できません/)).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /VLC 等で開く/ })).toHaveAttribute(
      'href',
      '/api/media/recordings/3/file',
    )
  })
})

// #1018 の見直し: シリーズの導線・棚・終端カードの移動・メニュー・版タブの細部。
describe('RecordingDetailPage シリーズの導線と終端カードの移動 (#1018)', () => {
  const seriesEpisode = (id: number, startAt: string, overrides: Partial<Recording> = {}) =>
    sampleRecording({
      id,
      title: `作品X 第${id}話`,
      series: '作品X',
      startAt,
      sizeBytes: 1_000_000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 900_000 }],
      ...overrides,
    })

  // 操作バーも棚も出ない状態（原本のみ・録画中・資産なし・ごみ箱）でも、シリーズへの導線は 1 つ出る。
  it.each([
    ['エンコード版のプレイヤー', {}],
    ['原本のみ', { encodedAssets: undefined }],
    ['録画中', { encodedAssets: undefined, sizeBytes: undefined, status: 'recording' as const }],
    ['資産なし', { encodedAssets: undefined, sizeBytes: undefined }],
    ['ごみ箱', { deletedAt: '2026-01-05T00:00:00Z' }],
  ] as [string, Partial<Recording>][])('シリーズの導線をタイトル行に出す: %s', async (_name, overrides) => {
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', overrides)
    createFakeServer({ recording: origin, seriesRecordings: [origin] })

    renderAt('/recordings/3')

    // 棚が無い幅（とごみ箱）向けのタイトル下の行。見出し語「シリーズ」で、タイトルの繰り返しに見せない。
    const row = await screen.findByTestId('recording-series-links')
    expect(within(row).getByRole('link', { name: 'このシリーズへ: 作品X' })).toHaveAttribute(
      'href',
      '/recordings/3/series',
    )
    expect(within(row).getByText('シリーズ')).toBeInTheDocument()
    // 棚がある状態では、棚の見出しのシリーズ名がシリーズ画面へのリンク（どの幅で見えるかは e2e ⑦ が測る）。
    const trash = overrides.deletedAt !== undefined
    const shelf = screen.queryByTestId('recording-series-shelf')
    if (trash) {
      expect(shelf).toBeNull()
    } else {
      expect(
        within(within(shelf!).getByRole('heading', { name: /作品X/ })).getByRole('link', { name: 'このシリーズへ: 作品X' }),
      ).toHaveAttribute('href', '/recordings/3/series')
    }
  })

  it('操作バーが無い状態では次のエピソードをシリーズの行に出し、バーがあるときは重ねて出さない', async () => {
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z')
    const originalOnly = seriesEpisode(3, '2026-01-01T12:00:00Z', { encodedAssets: undefined })
    createFakeServer({ recording: originalOnly, seriesRecordings: [originalOnly, next] })
    renderAt('/recordings/3')
    const row = await screen.findByTestId('recording-series-links')
    expect(await within(row).findByRole('link', { name: '次のエピソード: 作品X 第4話' })).toHaveAttribute(
      'href',
      '/recordings/4',
    )
    cleanup()

    const withPlayer = seriesEpisode(3, '2026-01-01T12:00:00Z')
    createFakeServer({ recording: withPlayer, seriesRecordings: [withPlayer, next] })
    renderAt('/recordings/3')
    await screen.findByTestId('next-episode-link')
    expect(screen.getAllByRole('link', { name: '次のエピソード: 作品X 第4話' })).toHaveLength(1)
    expect(within(screen.getByTestId('recording-series-links')).queryByRole('link', { name: /次のエピソード/ })).toBeNull()
  })

  it('棚は過去の回を含めて新しい順に並べ、本数と合計サイズを見出しに出す', async () => {
    const gib = 1024 * 1024 * 1024
    const big = { sizeBytes: gib, encodedAssets: [{ profile: 'h264', sizeBytes: gib }] }
    const past = seriesEpisode(2, '2025-12-25T12:00:00Z', big)
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', big)
    const later = seriesEpisode(4, '2026-01-08T12:00:00Z', big)
    createFakeServer({ recording: origin, seriesRecordings: [later, origin, past] })

    renderAt('/recordings/3')

    const summary = await screen.findByTestId('series-shelf-summary')
    // 3 本 × (原本 1 GB + エンコード 1 GB)
    expect(summary).toHaveTextContent('3 本 · 6.0 GB')
    const shelf = screen.getByTestId('recording-series-shelf')
    const hrefs = within(within(shelf).getByRole('list')).getAllByRole('link').map((link) => link.getAttribute('href'))
    expect(hrefs).toEqual(['/recordings/4', '/recordings/3', '/recordings/2'])
    expect(within(shelf).getByRole('link', { name: /再生中/ })).toHaveAttribute('href', '/recordings/3')
  })

  it('録画中の録画は棚で「再生中」ではなく「録画中」と出す', async () => {
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', { status: 'recording', encodedAssets: undefined, sizeBytes: undefined })
    createFakeServer({ recording: origin, seriesRecordings: [origin] })

    renderAt('/recordings/3')

    const shelf = await screen.findByTestId('recording-series-shelf')
    expect(shelf).toHaveTextContent('録画中')
    expect(shelf).not.toHaveTextContent('再生中')
  })

  it('終端カードから次の回へ移ると、履歴を積み、プレイヤーの DOM を作り直さず、移った先を再生する', async () => {
    const user = userEvent.setup()
    const playSpy = vi.spyOn(window.HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z')
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z')
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    const { router } = renderAt('/recordings/3')
    const frame = await screen.findByTestId('recording-player-frame')
    await screen.findByTestId('next-episode-link')

    fireEvent.ended(await screen.findByLabelText('録画映像'))
    await user.click(within(await screen.findByTestId('recording-end-card')).getByRole('button', { name: '今すぐ再生' }))

    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    // 全画面を保つため、プレイヤーの枠（全画面の対象）は同じ DOM 要素のまま中身だけが替わる。
    expect(screen.getByTestId('recording-player-frame')).toBe(frame)
    // 移った先の video が読み込まれたら再生を始める（カードの文言「N 秒後に再生」どおり）。
    expect(playSpy).not.toHaveBeenCalled()
    fireEvent.loadedMetadata(screen.getByLabelText('録画映像'))
    expect(playSpy).toHaveBeenCalledTimes(1)
    // 履歴を積んでいるので「戻る」で見ていた回へ戻れる。
    act(() => router.history.back())
    expect(await screen.findByRole('heading', { name: '作品X 第3話' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/recordings/3')
  })

  it('バーの「次のエピソード」で移っても再生は始めない', async () => {
    const user = userEvent.setup()
    const playSpy = vi.spyOn(window.HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z')
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z')
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    renderAt('/recordings/3')

    await user.click(await screen.findByTestId('next-episode-link'))
    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    fireEvent.loadedMetadata(screen.getByLabelText('録画映像'))
    expect(playSpy).not.toHaveBeenCalled()
  })

  it('終端カードに次のエピソードの題・放送日時とカウントダウンを出す', async () => {
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z')
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z')
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    renderAt('/recordings/3')

    fireEvent.ended(await screen.findByLabelText('録画映像'))

    const card = await screen.findByTestId('recording-end-card')
    expect(within(card).getByText('次のエピソード · 3 秒後に再生')).toBeInTheDocument()
    expect(card).toHaveTextContent('作品X 第4話')
    expect(card).toHaveTextContent(formatTime('2026-01-08T12:00:00Z'))
    expect(within(card).getByTestId('end-card-countdown-ring')).toBeInTheDocument()
    expect(card.querySelector('img')).toHaveAttribute('src', '/api/media/recordings/4/thumbnail')
  })

  // 次の回へ移ってもページ（プレイヤーの枠）は作り直さない（全画面を保つため）。録画ごとの状態は
  // RecordingDetail とプレイヤーが録画 id の変化で既定へ戻す。戻さないと前の回の状態が次の回に漏れる。
  it('次の回へ移ると、開いていたタブと説明の展開を既定に戻す', async () => {
    const user = userEvent.setup()
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', { description: '第3話の説明' })
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z', { description: '第4話の説明' })
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    renderAt('/recordings/3')

    await selectDetailTab('記録')
    await user.click(await screen.findByTestId('recording-description'))
    expect(screen.getByTestId('recording-description')).toHaveAttribute('aria-expanded', 'true')

    await user.click(await screen.findByTestId('next-episode-link'))
    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    // 既定のタブ（デスクトップは「版」）。jsdom の matchMedia は無いのでデスクトップ扱い。
    expect(screen.getByRole('tab', { name: '版' })).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tab', { name: '記録' })).toHaveAttribute('aria-selected', 'false')
    expect(screen.getByTestId('recording-description')).toHaveAttribute('aria-expanded', 'false')
  })

  it('次の回へ移ると、選んだ画質を既定に戻し、映像と版タブの「再生中」を一致させる', async () => {
    const user = userEvent.setup()
    const assets = [
      { profile: 'h264-720p', sizeBytes: 900_000 },
      { profile: 'h264-cut', sizeBytes: 700_000, cut: true },
    ]
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', { encodedAssets: assets })
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z', { encodedAssets: assets })
    createFakeServer({ recording: origin, seriesRecordings: [origin, next] })
    const { router } = renderAt('/recordings/3')
    await screen.findByTestId('next-episode-link')

    await user.click(screen.getByRole('button', { name: '再生設定' }))
    await user.click(screen.getByRole('menuitem', { name: '画質' }))
    const cut = screen.getAllByRole('menuitemradio').find((item) => item.textContent?.includes('h264-cut'))
    await user.click(cut!)
    expect(screen.getByLabelText('録画映像')).toHaveAttribute('src', '/api/media/recordings/3/file?profile=h264-cut')

    await user.click(screen.getByTestId('next-episode-link'))
    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    expect(screen.getByLabelText('録画映像')).toHaveAttribute('src', '/api/media/recordings/4/file?profile=h264-720p')
    await selectDetailTab('版')
    const playing = within(screen.getByRole('list', { name: '録画の版' }))
      .getAllByTestId('recording-version-row')
      .filter((row) => within(row).queryByText('再生中') !== null)
      .map((row) => row.textContent)
    expect(playing).toHaveLength(1)
    expect(playing[0]).toMatch(/^h264-720p再生中/)

    // 「戻る」で前の回へ戻っても、プレイヤーだけが前の選択（カット版）に戻らない。
    act(() => router.history.back())
    expect(await screen.findByRole('heading', { name: '作品X 第3話' })).toBeInTheDocument()
    expect(screen.getByLabelText('録画映像')).toHaveAttribute('src', '/api/media/recordings/3/file?profile=h264-720p')
    await selectDetailTab('版')
    expect(
      within(screen.getByRole('list', { name: '録画の版' }))
        .getAllByTestId('recording-version-row')
        .filter((row) => within(row).queryByText('再生中') !== null)
        .map((row) => row.textContent?.split('再生中')[0]),
    ).toEqual(['h264-720p'])
  })

  it('次の回へ移ると、チャプター編集の下書きと開閉を捨てる（移動先のチャプターが取得済みでも）', async () => {
    const user = userEvent.setup()
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z')
    const next = seriesEpisode(4, '2026-01-08T12:00:00Z')
    createFakeServer({
      recording: origin,
      seriesRecordings: [origin, next],
      chapters: {
        version: 'chapters-v1',
        detectionPending: false,
        source: 'auto',
        spans: [{ startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }],
      },
    })
    const { queryClient } = renderAt('/recordings/3')
    // 移動先のチャプターを先に取得しておく（取得待ちで編集器が一度消える経路に頼らない）。
    await queryClient.fetchQuery({
      queryKey: getGetRecordingChaptersQueryKey(4),
      queryFn: () => getRecordingChapters(4),
    })

    const details = (await screen.findByTestId('chapter-editor-details')) as HTMLDetailsElement
    await user.click(details.querySelector('summary')!)
    const label = within(details).getByLabelText('ラベル')
    await user.clear(label)
    await user.type(label, '前の回の下書き')
    expect(details).toHaveTextContent('未保存の変更があります')

    await user.click(screen.getByTestId('next-episode-link'))
    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    const nextDetails = (await screen.findByTestId('chapter-editor-details')) as HTMLDetailsElement
    expect(nextDetails.open).toBe(false)
    expect(within(nextDetails).getByLabelText('ラベル')).toHaveValue('CM')
    expect(nextDetails).not.toHaveTextContent('未保存の変更があります')
    expect(nextDetails).not.toHaveTextContent('サーバー側の内容が変わりました')
  })

  it('次の回へ移ると、追っかけ再生で選んでいた位置を先頭に戻す（キー操作でも前の位置を使わない）', async () => {
    const user = userEvent.setup()
    const now = Date.now()
    const live = (id: number, minutesAgo: number) =>
      seriesEpisode(id, new Date(now - minutesAgo * 60_000).toISOString(), {
        status: 'recording',
        startedAt: new Date(now - minutesAgo * 60_000).toISOString(),
        durationMs: 2 * 60 * 60_000,
        encodedAssets: undefined,
        sizeBytes: undefined,
      })
    const origin = live(3, 10)
    const other = live(4, 5)
    const { fetchMock } = createFakeServer({ recording: origin, seriesRecordings: [other, origin] })
    renderAt('/recordings/3')

    await user.click(await screen.findByRole('button', { name: '追っかけ再生' }))
    fireEvent.change(await screen.findByRole('slider', { name: '追っかけ再生の位置' }), { target: { value: '30' } })
    expect(screen.getByRole('slider', { name: '追っかけ再生の位置' })).toHaveAttribute('aria-valuenow', '30')

    const shelf = screen.getByTestId('recording-series-shelf')
    await user.click(within(within(shelf).getByRole('list')).getAllByRole('link').find((link) => link.getAttribute('href') === '/recordings/4')!)
    expect(await screen.findByRole('heading', { name: '作品X 第4話' })).toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: '追っかけ再生' }))
    const slider = await screen.findByRole('slider', { name: '追っかけ再生の位置' })
    expect(slider).toHaveAttribute('aria-valuenow', '0')
    fireEvent.keyUp(slider, { key: 'ArrowRight' })
    // 前の回の 30 秒を確定していれば、次の回の offset/30 のプレイリストを取りに行く。
    await new Promise((resolve) => setTimeout(resolve, 50))
    const paths = fetchMock.mock.calls.map(([input]) => new URL(String(input), 'http://localhost').pathname)
    expect(paths.filter((path) => path.includes('/recordings/4/chase/offset/'))).toEqual([])
  })

  it('棚のサムネイルに視聴の進み線を重ねる（視聴済みは全幅、途中は保存位置の割合、未視聴は出さない）', async () => {
    const watched = seriesEpisode(2, '2025-12-25T12:00:00Z', { watchedAt: '2026-01-02T00:00:00Z' })
    const origin = seriesEpisode(3, '2026-01-01T12:00:00Z', { resumePositionMs: 450_000, durationMs: 1_800_000 })
    const unwatched = seriesEpisode(4, '2026-01-08T12:00:00Z')
    createFakeServer({ recording: origin, seriesRecordings: [unwatched, origin, watched] })
    renderAt('/recordings/3')

    const shelf = await screen.findByTestId('recording-series-shelf')
    await waitFor(() => expect(within(within(shelf).getByRole('list')).getAllByRole('link')).toHaveLength(3))
    const widthOf = (href: string) => {
      const link = within(within(shelf).getByRole('list')).getAllByRole('link').find((item) => item.getAttribute('href') === href)!
      const line = link.querySelector<HTMLElement>('[data-testid="series-shelf-progress-line"] > div')
      return line?.style.width
    }
    expect(widthOf('/recordings/2')).toBe('100%')
    expect(widthOf('/recordings/3')).toBe('25%')
    expect(widthOf('/recordings/4')).toBeUndefined()
  })
})

describe('RecordingDetailPage メニューと版タブの細部 (#1018)', () => {
  it('⋮ の「ルールを開く」にルール名を添える', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule' }),
      rules: [sampleRule({ id: 5, name: 'ニュース全部' })],
    })
    renderAt('/recordings/3')

    const menu = await openRecordingMenu()
    await waitFor(() => expect(within(menu).getByTestId('menu-rule-name')).toHaveTextContent('ニュース全部'))
  })

  it('原本が無い録画では「CM 検出をやり直す」を押せない形で出す', async () => {
    createFakeServer({
      recording: sampleRecording({
        encodedAssets: [{ profile: 'web', sizeBytes: 500_000 }],
        cmDetection: { state: 'failed', stage: 'logo' },
      }),
      cmDetectCapability: true,
    })
    renderAt('/recordings/3')

    await openRecordingMenu()
    await waitFor(() => expect(screen.getByRole('menuitem', { name: 'CM 検出をやり直す' })).toHaveAttribute('aria-disabled', 'true'))
  })

  it('原本の保持が既定どおりなら「ルールの既定」と添える', async () => {
    createFakeServer({
      recording: sampleRecording({ ruleId: 5, source: 'rule', sizeBytes: 1_000_000, keepOriginal: 'always' }),
      rules: [sampleRule({ id: 5, keepOriginal: 'always' })],
    })
    renderAt('/recordings/3')

    await selectDetailTab('版')
    expect(await screen.findByText('原本: 常に保持（ルールの既定）')).toBeInTheDocument()
  })

  it('説明は番組タブに繰り返さず、タイトル行の 1 か所だけに出す（押すと全文を開く）', async () => {
    const user = userEvent.setup()
    createFakeServer({ recording: sampleRecording({ description: '説明の本文です' }) })
    renderAt('/recordings/3')

    const description = await screen.findByTestId('recording-description')
    await selectDetailTab('番組')
    expect(screen.getAllByText('説明の本文です')).toHaveLength(1)
    expect(description).toHaveAttribute('aria-expanded', 'false')
    await user.click(description)
    expect(description).toHaveAttribute('aria-expanded', 'true')
  })

  it('版タブは中身の無い区切り枠を先頭に出さない', async () => {
    // 版が 1 つも無い（エンコード待ちだけ）なら、行の無い枠そのものを出さない。
    createFakeServer({ recording: sampleRecording({ encodeProfiles: ['web'] }) })
    renderAt('/recordings/3')

    await selectDetailTab('版')
    await screen.findByTestId('recording-assets-group')
    expect(screen.queryByRole('list', { name: '録画の版' })).not.toBeInTheDocument()
  })
})
