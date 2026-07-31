import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { useListRecordingDropStats, type DropStat, type Recording } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { DropStatsTable, RecordingsPage } from '@/pages/recordings'

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

function renderRecordingsPage(fetchImpl: typeof fetch) {
  globalThis.fetch = fetchImpl as typeof fetch
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RecordingsPage />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('RecordingsPage trash', () => {
  it('ライブラリとごみ箱を切り替え、ごみ箱一覧を trash=true で取る', async () => {
    const user = userEvent.setup()
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      const trash = url.searchParams.get('trash') === 'true'
      const body = trash
        ? [sampleRecording({ id: 2, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })]
        : [sampleRecording()]
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    })

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()
    expect(screen.queryByText('捨てた録画')).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))

    expect(await screen.findByText('捨てた録画')).toBeInTheDocument()
    expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument()

    // trash=true で呼ばれたことを確認（空成功にしない）
    const trashCalls = fetchMock.mock.calls.filter((call) => {
      const url = new URL(String(call[0]), 'http://localhost')
      return url.pathname === '/api/recordings' && url.searchParams.get('trash') === 'true'
    })
    expect(trashCalls.length).toBeGreaterThan(0)
  })

  it('「今すぐ完全削除」は確認ダイアログを挟み、確定するまで purge を呼ばない', async () => {
    const user = userEvent.setup()
    const purgeCalls: string[] = []
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'
      if (url.pathname.startsWith('/api/recordings/') && url.pathname.endsWith('/purge')) {
        purgeCalls.push(url.pathname)
        return Promise.resolve(new Response(null, { status: 204 }))
      }
      const trash = url.searchParams.get('trash') === 'true'
      const body =
        trash && method === 'GET'
          ? [sampleRecording({ id: 7, title: '捨てた録画', deletedAt: '2026-01-02T00:00:00Z' })]
          : []
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    })

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てた録画'))

    // ボタンを押しただけでは purge は飛ばない（確認を挟む）
    await user.click(screen.getByRole('button', { name: '今すぐ完全削除' }))
    expect(purgeCalls).toHaveLength(0)

    // ダイアログの確定ボタンを押して初めて purge が飛ぶ
    await user.click(await screen.findByRole('button', { name: '完全削除を予約する' }))
    await waitFor(() => expect(purgeCalls).toHaveLength(1))
    expect(purgeCalls[0]).toBe('/api/recordings/7/purge')
  })

  it('ライブラリでは派生物・原本があればプレイヤーとサムネイルを出す', async () => {
    const user = userEvent.setup()
    const fetchMock = vi.fn(() => {
      const body = [
        sampleRecording({
          id: 3,
          title: '再生できる録画',
          encodedProfiles: ['web'],
          sizeBytes: 1_000_000,
        }),
      ]
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    })

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(await screen.findByText('再生できる録画'))

    // クエリが解決してから見る（非同期の空虚な成功を避ける）
    expect(await screen.findByRole('region', { name: '再生' })).toBeInTheDocument()
    expect(document.querySelector('video')).toBeInTheDocument()
    expect(document.querySelector('img[src="/api/recordings/3/thumbnail"]')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ダウンロード / VLC' })).toBeInTheDocument()
  })

  it('ごみ箱では 404 になるサムネイル・プレイヤー・原本リンクを一切出さない', async () => {
    const user = userEvent.setup()
    const requestedPaths: string[] = []
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      requestedPaths.push(url.pathname + url.search)
      // encodedProfiles と sizeBytes の両方を持つ、つまりライブラリなら
      // プレイヤーとサムネイルの両方が出る録画を、ごみ箱に入れて返す。
      const body = [
        sampleRecording({
          id: 9,
          title: '捨てられた再生可能録画',
          deletedAt: '2026-01-03T00:00:00Z',
          encodedProfiles: ['web'],
          sizeBytes: 1_000_000,
        }),
      ]
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    })

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てられた再生可能録画'))

    // 展開後の内容（削除日時 dt）が出るまで待ってから「無い」ことを確認する
    // （クエリ未解決のうちに queryBy で通ってしまう空虚な成功を避ける）
    await screen.findByText('削除日時')

    expect(screen.queryByRole('region', { name: '再生' })).not.toBeInTheDocument()
    expect(document.querySelector('video')).not.toBeInTheDocument()
    expect(document.querySelector('img')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'ダウンロード / VLC' })).not.toBeInTheDocument()
    expect(screen.queryByText('VLC 等で開く')).not.toBeInTheDocument()

    // 404 の温床になる配信系エンドポイントに一切リクエストしていない
    const mediaRequests = requestedPaths.filter(
      (p) => p.includes('/thumbnail') || p.includes('/file'),
    )
    expect(mediaRequests).toHaveLength(0)
  })
})
