import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createMemoryHistory, createRouter } from '@tanstack/react-router'
import { act, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Recording } from '@/api/generated'
import { ToastProvider } from '@/components/toaster'
import { useServerEvents } from '@/lib/events'
import { EncodeStatusBadges } from '@/components/recording-badges'
import { routeTree } from '@/routes'

/**
 * 完了していないエンコードプロファイルの試行状態の表示（issue #316）。
 *
 * `recordings-ingest.test.tsx` と同じ構造 --- 録画単体ページ越しに見ることで
 * ヘッダーのバッジ（`EncodeStatusBadges`）と一覧の行の両方が同じコードで
 * 描かれることを担保する。
 */

class EventSourceStub {
  static last: EventSourceStub | null = null
  private listeners = new Map<string, Set<(event: Event) => void>>()

  constructor(_url: string) {
    EventSourceStub.last = this
  }

  addEventListener(type: string, listener: (event: Event) => void): void {
    const set = this.listeners.get(type) ?? new Set()
    set.add(listener)
    this.listeners.set(type, set)
  }

  removeEventListener(type: string, listener: (event: Event) => void): void {
    this.listeners.get(type)?.delete(listener)
  }

  close(): void {}

  emit(type: string, data: string): void {
    const event = new MessageEvent(type, { data })
    for (const listener of this.listeners.get(type) ?? []) listener(event)
  }
}

function ServerEvents() {
  useServerEvents()
  return null
}

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
    title: 'エンコード状態を見る録画',
    startAt: '2026-01-01T12:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-01T12:30:00Z',
    sizeBytes: 1_000_000,
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
  globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
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
      <ServerEvents />
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

/** renderList は録画一覧（`/recordings`）を描画する（一覧行のバッジ確認用）。 */
function renderList(recordings: Recording[]) {
  window.scrollTo = vi.fn()
  globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
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
      <ServerEvents />
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('エンコード試行状態の表示', () => {
  it('queued / running / failed をそれぞれ文言で出す', async () => {
    renderRecording(
      sampleRecording({
        encodeProfiles: ['h264', 'h265', 'aac'],
        encodeStatus: [
          { profile: 'h264', state: 'queued' },
          { profile: 'h265', state: 'running' },
          { profile: 'aac', state: 'failed' },
        ],
      }),
    )

    expect(await screen.findByText('エンコード状態を見る録画')).toBeInTheDocument()
    expect(screen.getByText('h264: エンコード待ち')).toBeInTheDocument()
    expect(screen.getByText('h265: エンコード中')).toBeInTheDocument()
    expect(screen.getByText('aac: エンコード失敗')).toBeInTheDocument()
  })

  it('途中参加では running を先に出し、次の SSE 進捗から百分率を出す', async () => {
    renderRecording(
      sampleRecording({
        encodeProfiles: ['mobile'],
        encodeStatus: [{ profile: 'mobile', state: 'running' }],
      }),
    )

    expect(await screen.findByText('mobile: エンコード中')).toBeInTheDocument()
    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 3,
          profile: 'mobile',
          progress: 0.429,
        }),
      )
    })
    expect(screen.getByText('mobile: エンコード中 42%')).toBeInTheDocument()
  })

  it('画面未表示中に届いた値を、後から開いた running に流用しない', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <ServerEvents />
      </QueryClientProvider>,
    )

    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 3,
          profile: 'mobile',
          progress: 0.5,
        }),
      )
    })
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <ServerEvents />
        <EncodeStatusBadges
          recording={sampleRecording({
            encodeProfiles: ['mobile'],
            encodeStatus: [{ profile: 'mobile', state: 'running' }],
          })}
        />
      </QueryClientProvider>,
    )

    expect(screen.getByText('mobile: エンコード中')).toBeInTheDocument()
    expect(screen.queryByText('mobile: エンコード中 50%')).not.toBeInTheDocument()
  })

  it('durable 状態が running でなくなったら最後の進捗を破棄する', () => {
    globalThis.EventSource = EventSourceStub as unknown as typeof EventSource
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const running = sampleRecording({
      encodeProfiles: ['mobile'],
      encodeStatus: [{ profile: 'mobile', state: 'running' }],
    })
    const view = render(
      <QueryClientProvider client={queryClient}>
        <ServerEvents />
        <EncodeStatusBadges recording={running} />
      </QueryClientProvider>,
    )

    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 3,
          profile: 'mobile',
          progress: 0.5,
        }),
      )
    })
    expect(screen.getByText('mobile: エンコード中 50%')).toBeInTheDocument()

    view.rerender(
      <QueryClientProvider client={queryClient}>
        <ServerEvents />
        <EncodeStatusBadges
          recording={{ ...running, encodeStatus: [{ profile: 'mobile', state: 'failed' }] }}
        />
      </QueryClientProvider>,
    )
    expect(screen.getByText('mobile: エンコード失敗')).toBeInTheDocument()

    view.rerender(
      <QueryClientProvider client={queryClient}>
        <ServerEvents />
        <EncodeStatusBadges recording={running} />
      </QueryClientProvider>,
    )
    expect(screen.getByText('mobile: エンコード中')).toBeInTheDocument()
    expect(screen.queryByText('mobile: エンコード中 50%')).not.toBeInTheDocument()
  })

  it('encodeStatus が省略されているときは何も出さない（プロファイル未設定・全完了済みの両方に使う）', async () => {
    renderRecording(sampleRecording({ encodeStatus: undefined }))

    expect(await screen.findByText('エンコード状態を見る録画')).toBeInTheDocument()
    expect(screen.queryByText(/エンコード待ち|エンコード中|エンコード失敗/)).not.toBeInTheDocument()
  })

  it('完了済みプロファイル（encodedAssets）は encodeStatus のバッジに出ない', async () => {
    renderRecording(
      sampleRecording({
        encodeProfiles: ['h264', 'h265'],
        encodedAssets: [{ profile: 'h264', sizeBytes: 500_000 }],
        encodeStatus: [{ profile: 'h265', state: 'queued' }],
      }),
    )

    expect(await screen.findByText('エンコード状態を見る録画')).toBeInTheDocument()
    expect(screen.getByText('h265: エンコード待ち')).toBeInTheDocument()
    expect(screen.queryByText(/h264: エンコード/)).not.toBeInTheDocument()
  })
})

describe('録画一覧のエンコードバッジ', () => {
  it('一覧行の running も SSE 進捗で更新する', async () => {
    renderList([
      sampleRecording({
        id: 21,
        title: '進捗がある録画',
        encodeProfiles: ['mobile'],
        encodeStatus: [{ profile: 'mobile', state: 'running' }],
      }),
    ])

    expect(await screen.findByText('mobile: エンコード中')).toBeInTheDocument()
    act(() => {
      EventSourceStub.last?.emit(
        'encode-progress',
        JSON.stringify({
          type: 'encode-progress',
          recordingId: 21,
          profile: 'mobile',
          progress: 0.5,
        }),
      )
    })
    expect(screen.getByText('mobile: エンコード中 50%')).toBeInTheDocument()
  })

  it('失敗しているプロファイルがある録画は行を展開しなくても分かる', async () => {
    renderList([
      sampleRecording({
        id: 21,
        title: '失敗している録画',
        encodeProfiles: ['h264'],
        encodeStatus: [{ profile: 'h264', state: 'failed' }],
      }),
      sampleRecording({
        id: 22,
        title: '正常な録画',
        encodeProfiles: ['h264'],
        encodedAssets: [{ profile: 'h264', sizeBytes: 500_000 }],
      }),
    ])

    expect(await screen.findByText('失敗している録画')).toBeInTheDocument()
    expect(screen.getByText('正常な録画')).toBeInTheDocument()
    // 失敗している行にだけバッジが出る。
    expect(screen.getAllByText('h264: エンコード失敗')).toHaveLength(1)
  })
})
