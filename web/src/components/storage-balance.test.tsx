import { screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Recording, Reservation, StorageRoot } from '@/api/generated'
import { StorageBalance } from '@/components/storage-balance'
import { renderInRouter } from '@/test/router'

/**
 * StorageBalance の表示分岐（issue #239 M7-6）。導出そのもの（母数・線形外挿・
 * 境界値）は `lib/storage-forecast.test.ts` が担当するので、ここでは
 * 「3 つの沈黙」と「表示に出るべき数字」を実際の DOM で確認する
 * （CLAUDE.md テスト規律「CI が緑でも実バイナリ・実ブラウザを起動して確かめる」の
 * 手前 --- まずユニットテストで DOM に出るところまで固定する）。
 *
 * **タイマーは fake にしない。** コンポーネントは `Date.now()` を直接呼ぶため、
 * `vi.useFakeTimers()` と組み合わせると `findByText` 等の非同期待機
 * （testing-library 内部のポーリング）が固まって全テストがタイムアウトする
 * （実際に踏んだ --- `setSystemTime` を使った最初の実装は 7 件全滅した）。
 * 代わりに実行時の `Date.now()` からの相対オフセットで日時を組む。
 */

/** iso は「実行時から offsetMs だけ先/前」の ISO 文字列を返す。 */
function iso(offsetMs: number): string {
  return new Date(Date.now() + offsetMs).toISOString()
}

function mediaRoot(overrides: Partial<StorageRoot> = {}): StorageRoot {
  return {
    root: 'media',
    path: '/media',
    totalBytes: 1_000_000_000_000,
    usedBytes: 0,
    availableBytes: 1_000_000_000_000,
    observedAt: iso(0),
    ...overrides,
  }
}

function recording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 1,
    site: 'default',
    source: 'manual',
    serviceName: 'test',
    channelType: 'GR',
    channel: '1',
    networkId: 1,
    serviceId: 1,
    eventId: 1,
    title: 'test',
    startAt: '2026-08-01T00:00:00Z',
    durationMs: 1_800_000, // 30分
    status: 'finished',
    createdAt: '2026-08-01T00:00:00Z',
    sizeBytes: 900_000_000, // 900MB / 30分
    ...overrides,
  }
}

function reservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    id: 1,
    site: 'default',
    programId: 1,
    source: 'manual',
    state: 'active',
    title: 'test',
    startAt: iso(60 * 60 * 1000), // 1時間後
    durationMs: 1_800_000,
    createdAt: '2026-08-01T00:00:00Z',
    updatedAt: '2026-08-01T00:00:00Z',
    skip: false,
    ...overrides,
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** mockApis は StorageBalance が発行する 3 つの GET を差し替える。 */
function mockApis(options: {
  storage?: StorageRoot[]
  recordings?: Recording[]
  reservations?: Reservation[]
}) {
  const storage = options.storage ?? [mediaRoot()]
  const recordings = options.recordings ?? []
  const reservations = options.reservations ?? []

  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')

    if (url.pathname === '/api/storage') return Promise.resolve(jsonResponse(storage))
    if (url.pathname === '/api/recordings') {
      const status = url.searchParams.get('status')
      const limit = Number(url.searchParams.get('limit') ?? '50')
      const filtered = recordings.filter((r) => status === null || r.status === status)
      return Promise.resolve(jsonResponse(filtered.slice(0, limit)))
    }
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))

    throw new Error(`unexpected fetch: ${url.pathname}`)
  }) as unknown as typeof fetch
}

describe('StorageBalance', () => {
  it('観測なし（media root が無い）ときは何も描かない', async () => {
    mockApis({ storage: [] })

    const { container } = renderInRouter(<StorageBalance />)

    // クエリの解決を待ってから「無い」ことを見る（非同期の空虚な成功を避ける）
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    await waitFor(() => expect(container.textContent).toBe(''))
  })

  it('録画実績が 0 件のときは空きだけ出し、見込みは出さない', async () => {
    mockApis({ recordings: [], reservations: [reservation()] })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/空き/)).toBeInTheDocument()
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })

  it('見込み消費が残量に収まるときは満杯見込みを出さない（下界主義）', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 1_000_000_000_000 })], // 1TB
      recordings: [recording()],
      // 1 件・30分。900MB/30分の実測なので今後 7 日の見込み消費は
      // 900MB 程度 --- 1TB の残量に対して十分に収まる
      reservations: [reservation()],
    })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/の見込み/)).toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })

  it('見込み消費が残量を超えるときは満杯見込み日を出す', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 100_000_000 })], // 100MB しか無い
      recordings: [recording()], // 900MB/30分の実測
      // 7 日間、1 日 1 本（30分）の予約 --- 7 * 900MB ≈ 6.3GB の見込み消費
      reservations: Array.from({ length: 7 }, (_, i) =>
        reservation({
          id: i + 1,
          programId: i + 1,
          startAt: iso(i * 24 * 60 * 60 * 1000 + 1000),
        }),
      ),
    })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/満杯見込み/)).toBeInTheDocument()
  })

  it('観測が古い（1 時間超）ときは古い可能性を表示する', async () => {
    mockApis({
      storage: [mediaRoot({ observedAt: iso(-2 * 60 * 60 * 1000) })],
    })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/古い可能性/)).toBeInTheDocument()
  })

  it('観測が新しい（1 時間以内）ときは古い可能性を表示しない', async () => {
    mockApis({
      storage: [mediaRoot({ observedAt: iso(-5 * 60 * 1000) })],
    })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/空き/)).toBeInTheDocument()
    expect(screen.queryByText(/古い可能性/)).not.toBeInTheDocument()
  })

  it('skip=true の予約は消費見込みに数えない', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 100_000_000 })], // 100MB
      recordings: [recording()],
      // skip=true の予約が 7 件あっても、reconciler が mirakc に同期しないので
      // 消費に数えない → 見込み消費 0（recording 標本はあるので見込み自体は
      // 出る）で、0 は残量を超えないため満杯見込みは出ない
      reservations: Array.from({ length: 7 }, (_, i) =>
        reservation({
          id: i + 1,
          programId: i + 1,
          startAt: iso(i * 24 * 60 * 60 * 1000 + 1000),
          skip: true,
        }),
      ),
    })

    renderInRouter(<StorageBalance />)

    expect(await screen.findByText(/の見込み/)).toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })
})
