import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { EncodeProfileSummary, Recording } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

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

/**
 * createFakeServer は `GET /api/recordings/{id}` 単体取得とその周辺
 * （削除・復元・完全削除・追加エンコード）を状態を持ってシミュレートする。
 * `recordings.test.tsx` の `createFakeRecordingsServer` は一覧
 * （`GET /api/recordings`）専用なので、単体ページのテストにそのまま使えない
 * （このページは一覧を叩かない）。
 */
function createFakeServer(options: {
  recording: Recording | null
  encodeProfiles?: EncodeProfileSummary[]
}) {
  let recording = options.recording
  const encodeProfiles = options.encodeProfiles ?? []

  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // SiteGate（routes.tsx）が全ルートの手前で待つ（issue #184 M4-12）。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(encodeProfiles))

    const getMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (getMatch && method === 'GET') {
      const id = Number(getMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      return Promise.resolve(jsonResponse(recording))
    }

    const deleteMatch = /^\/api\/recordings\/(\d+)$/.exec(url.pathname)
    if (deleteMatch && method === 'DELETE') {
      const id = Number(deleteMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      recording = { ...recording, deletedAt: '2026-01-05T00:00:00Z' }
      return Promise.resolve(jsonResponse(null, 204))
    }

    const restoreMatch = /^\/api\/recordings\/(\d+)\/restore$/.exec(url.pathname)
    if (restoreMatch && method === 'POST') {
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
      const id = Number(purgeMatch[1])
      if (!recording || recording.id !== id) {
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404))
      }
      return Promise.resolve(jsonResponse(null, 204))
    }

    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse([]))
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

describe('RecordingDetailPage', () => {
  // 受け入れ基準: /recordings/{id} で録画単体が開き、再生・操作が一覧の展開と
  // 同等に機能する（issue #232）。一覧側の同種のテスト
  // （recordings.test.tsx「再生できる録画」）と同じ観測項目を単体ページでも見る。
  it('通常の録画は再生・サムネイル・原本リンク・削除操作が一覧の展開と同等に出る', async () => {
    createFakeServer({
      recording: sampleRecording({ encodedProfiles: ['web'], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    expect(await screen.findByText('単体ページの録画')).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(document.querySelector('img[src="/api/recordings/3/thumbnail"]')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ダウンロード / VLC' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'ごみ箱へ' })).toBeInTheDocument()
  })

  it('存在しない id は「録画が見つかりません」を表示する', async () => {
    createFakeServer({ recording: null })

    renderAt('/recordings/999999')

    expect(await screen.findByText('録画が見つかりません')).toBeInTheDocument()
  })

  // ごみ箱の録画も 200 で返る（getRecording の openapi.yaml description の決定）
  // が、単体ページでも一覧の展開と同じ規律で再生系を一切出さない（M3-18、
  // issue #232 の受け入れ「一覧の規律と一致する」）。encodedProfiles /
  // sizeBytes を敢えて持たせても出ないことを見て、判定が deletedAt の有無で
  // 効いていることを確かめる。
  it('ごみ箱の録画は 200 で開くが再生系を一切出さない', async () => {
    createFakeServer({
      recording: sampleRecording({
        deletedAt: '2026-01-02T00:00:00Z',
        encodedProfiles: ['web'],
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
    expect(screen.queryByRole('link', { name: 'ダウンロード / VLC' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '復元' })).toBeInTheDocument()
  })

  // 単体ページ固有の経路: 一覧の invalidate（'/api/recordings' 前方一致）は
  // このページ自身のクエリキー（'/api/recordings/{id}' という別の 1 要素の
  // 文字列）を捨てない。RecordingDetail に渡した onMutated がここを埋めて
  // いなければ、削除してもこの画面は古い（生きている）表示のまま固まる。
  it('ごみ箱へ移すと、ナビゲーションなしで自分自身が再生系無しの表示に更新される', async () => {
    const user = userEvent.setup()
    createFakeServer({
      recording: sampleRecording({ encodedProfiles: ['web'], sizeBytes: 1_000_000 }),
    })

    renderAt('/recordings/3')

    await screen.findByRole('region', { name: '再生' })

    await user.click(screen.getByRole('button', { name: 'ごみ箱へ' }))

    await waitFor(() => expect(screen.getByText('復元')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
  })
})
