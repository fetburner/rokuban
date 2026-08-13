import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Recording } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { routeTree } from '@/routes'

/**
 * 取り込み（ingest）進捗の表示（issue #212）。
 *
 * 録画単体ページ（`/recordings/{id}`）越しに見る --- ヘッダーのバッジ
 * （`IngestBadge`）と展開部の「取り込み」欄（`RecordingDetail`）の両方が
 * 1 回の描画で観測でき、しかも `RecordingDetail` は一覧の行展開と同じ部品
 * なので、一覧側の表示も同じコードで担保される。
 *
 * ファイルを `recordings.test.tsx` と分けているのは、あちらが一覧
 * （`GET /api/recordings`）専用の大きなフェイクサーバーを持っており、
 * この観点には単体取得だけで足りるため。
 */

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
    title: '取り込みを見る録画',
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

function renderRecording(recording: Recording) {
  window.scrollTo = vi.fn()
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse([]))
    if (url.pathname === `/api/recordings/${recording.id}`) {
      return Promise.resolve(jsonResponse(recording))
    }
    if (/^\/api\/recordings\/\d+\/drop-stats$/.test(url.pathname)) {
      return Promise.resolve(jsonResponse([]))
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  }) as unknown as typeof fetch

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [`/recordings/${recording.id}`] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

/**
 * renderList は録画一覧（`/recordings`）を描画する。
 *
 * 単体ページ（`renderRecording`）と別に要るのは、一覧の行
 * （`RecordingRow`）がバッジを載せる配線がそこにしか無いため --- 展開部
 * （`RecordingDetail`）は両画面で共有されているが、行のメタ行は共有されて
 * いない。
 *
 * 一覧ページはヘッダーのストレージ残高など多くの副次的なクエリを叩くので、
 * 知らないパスは空配列で答える（本題ではない）。
 */
function renderList(recordings: Recording[]) {
  window.scrollTo = vi.fn()
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(['default']))
    if (url.pathname === '/api/recordings') return Promise.resolve(jsonResponse(recordings))
    return Promise.resolve(jsonResponse([]))
  }) as unknown as typeof fetch

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: ['/recordings'] }),
  })
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('録画一覧の取り込みバッジ', () => {
  it('取り込み中の録画は行を展開しなくても分かる', async () => {
    renderList([
      sampleRecording({
        id: 11,
        title: '取り込み中の録画',
        ingest: {
          state: 'transferring',
          writtenBytes: 500_000_000,
          expectedBytes: 1_000_000_000,
          observedAt: new Date().toISOString(),
        },
      }),
      sampleRecording({
        id: 12,
        title: '取り込み済みの録画',
        sizeBytes: 1_000_000,
        ingest: { state: 'committed' },
      }),
    ])

    expect(await screen.findByText('取り込み中の録画')).toBeInTheDocument()
    expect(screen.getByText('取り込み済みの録画')).toBeInTheDocument()
    // 取り込み中の行にだけバッジが 1 つ出る。
    expect(screen.getAllByText('取り込み中 50%')).toHaveLength(1)
  })
})

describe('取り込み進捗の表示', () => {
  it('転送中は割合と実バイト数が出る（sizeBytes には混ざらない）', async () => {
    renderRecording(
      sampleRecording({
        ingest: {
          state: 'transferring',
          writtenBytes: 250_000_000,
          expectedBytes: 1_000_000_000,
          observedAt: new Date().toISOString(),
        },
      }),
    )

    // 描画の完了を待ってからアサートする（未解決のまま「出ていない」を
    // 確かめると必ず通ってしまう）。
    expect(await screen.findByText('取り込みを見る録画')).toBeInTheDocument()
    expect(screen.getByText('取り込み中 25%')).toBeInTheDocument()
    expect(screen.getByText('転送中 238.4 MB / 953.7 MB（25%）')).toBeInTheDocument()
    // 途中ファイルのサイズは公開済みの sizeBytes に混ぜない（不変条件 3）ので、
    // ヘッダーのサイズ表示は出ない。
    expect(screen.queryByText('238.4 MB')).not.toBeInTheDocument()
  })

  it('進捗が古いままなら停滞と言う', async () => {
    renderRecording(
      sampleRecording({
        ingest: {
          state: 'transferring',
          writtenBytes: 1_000,
          observedAt: new Date(Date.now() - 10 * 60_000).toISOString(),
        },
      }),
    )

    expect(await screen.findByText('取り込みを見る録画')).toBeInTheDocument()
    // 分母が無いので % ではなくバイト数を出す。
    expect(screen.getByText('取り込み中 1000 B（停滞）')).toBeInTheDocument()
    expect(screen.getByText('転送中・停滞 1000 B')).toBeInTheDocument()
  })

  it('取り込み待ちは「削除済み」ではなく待機中として出る', async () => {
    renderRecording(sampleRecording({ ingest: { state: 'pending' } }))

    expect(await screen.findByText('取り込みを見る録画')).toBeInTheDocument()
    expect(screen.getByText('取り込み待ち')).toBeInTheDocument()
    expect(screen.getByText('待機中（まだ原本を取り込んでいません）')).toBeInTheDocument()
    expect(screen.queryByText(/原本は削除済み/)).not.toBeInTheDocument()
  })

  // issue #211 の区別。原本 media_asset 行はあるが state='deleted' の録画だけが
  // 「削除済み」を名乗る。
  it('取り込み済みで原本が消えている録画だけが「原本は削除済み」と言う', async () => {
    renderRecording(sampleRecording({ ingest: { state: 'committed' } }))

    expect(await screen.findByText('取り込みを見る録画')).toBeInTheDocument()
    expect(screen.getByText('完了（原本は削除済み）')).toBeInTheDocument()
    // 一覧行のバッジには出さない（展開後の欄が引き受ける）。
    expect(screen.queryByText(/取り込み中|取り込み待ち/)).not.toBeInTheDocument()
  })

  // ingest ジョブが投入されない録画（サーバーは state='unknown' を返す。
  // internal/api の TestListRecordingsIngestNoJobComing）。「取り込み待ち」を
  // 出すと来ない未来を断定することになる（#211 と同じ形）。
  it('失敗した録画には「取り込み待ち」を出さない', async () => {
    renderRecording(
      sampleRecording({ title: '失敗した録画', status: 'failed', ingest: { state: 'unknown' } }),
    )

    expect(await screen.findByText('失敗した録画')).toBeInTheDocument()
    expect(screen.queryByText('取り込み待ち')).not.toBeInTheDocument()
    expect(screen.queryByText('待機中（まだ原本を取り込んでいません）')).not.toBeInTheDocument()
    expect(screen.queryByText('取り込み')).not.toBeInTheDocument()
  })

  it('正常に取り込めた録画には取り込み欄そのものが出ない', async () => {
    renderRecording(sampleRecording({ ingest: { state: 'committed' }, sizeBytes: 1_000_000 }))

    expect(await screen.findByText('取り込みを見る録画')).toBeInTheDocument()
    // 「取り込み」という dt ごと出ない（言うことが無いときに行を並べない）。
    expect(screen.queryByText('取り込み')).not.toBeInTheDocument()
    expect(screen.queryByText(/原本は削除済み/)).not.toBeInTheDocument()
  })
})
