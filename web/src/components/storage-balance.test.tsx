import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import {
  useGetStorage,
  useListRecordings,
  useListReservations,
  type Recording,
  type Reservation,
  type StorageRoot,
} from '@/api/generated'
import { StorageBalance } from '@/components/storage-balance'
import { recentRecordingSampleLimit } from '@/lib/storage-forecast'
import { renderInRouter } from '@/test/router'

/**
 * StorageBalance の表示分岐（issue #239 M7-6）。導出そのもの（母数・累積曲線・
 * 境界値）は `lib/storage-forecast.test.ts` が担当するので、ここでは
 * 「4 つの沈黙」と「表示に出るべき数字」を実際の DOM で確認する
 * （CLAUDE.md テスト規律「CI が緑でも実バイナリ・実ブラウザを起動して確かめる」の
 * 手前 --- まずユニットテストで DOM に出るところまで固定する）。
 *
 * **タイマーは fake にしない。** コンポーネントは `Date.now()` を直接呼ぶため、
 * `vi.useFakeTimers()` と組み合わせると `findByText` 等の非同期待機
 * （testing-library 内部のポーリング）が固まって全テストがタイムアウトする
 * （実際に踏んだ --- `setSystemTime` を使った最初の実装は 7 件全滅した）。
 * 代わりに実行時の `Date.now()` からの相対オフセットで日時を組む。
 *
 * **「無い」ことを見るテストは、3 つのクエリが実際に解決したことを確認してから
 * アサートする。** レビューで指摘された通り、`fetch` が「呼ばれた」ことだけを
 * 待つ（`toHaveBeenCalled`）のは「解決した」ことの証明にならない
 * （非同期の空虚な成功 --- クエリが unresolved の間も `media` は
 * `undefined` で「無い」ことと見分けが付かない）。`Harness` が `StorageBalance`
 * と同じ 3 つのクエリを（同じ `queryClient` を共有するので同じキャッシュ状態を）
 * 呼び、そのステータス文字列が確定するまで待ってから、初めて否定のアサートに進む。
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

function scratchRoot(overrides: Partial<StorageRoot> = {}): StorageRoot {
  return {
    root: 'scratch',
    path: '/scratch',
    totalBytes: 500_000_000_000,
    usedBytes: 300_000_000_000,
    availableBytes: 200_000_000_000,
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
    serviceName: 'test',
    channelType: 'GR',
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

/**
 * mockApis は StorageBalance が発行する 3 つの GET を差し替える。
 * `reservationsStatus` を 200 以外にすると `GET /api/reservations` の失敗
 * （指摘 1 の再現）をシミュレートできる。
 */
function mockApis(options: {
  storage?: StorageRoot[]
  recordings?: Recording[]
  reservations?: Reservation[]
  reservationsStatus?: number
}) {
  const storage = options.storage ?? [mediaRoot()]
  const recordings = options.recordings ?? []
  const reservations = options.reservations ?? []
  const reservationsStatus = options.reservationsStatus ?? 200

  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')

    if (url.pathname === '/api/storage') return Promise.resolve(jsonResponse(storage))
    if (url.pathname === '/api/recordings') {
      const status = url.searchParams.get('status')
      const limit = Number(url.searchParams.get('limit') ?? '50')
      const filtered = recordings.filter((r) => status === null || r.status === status)
      return Promise.resolve(jsonResponse(filtered.slice(0, limit)))
    }
    if (url.pathname === '/api/reservations') {
      if (reservationsStatus !== 200) {
        return Promise.resolve(jsonResponse({ error: 'boom' }, reservationsStatus))
      }
      return Promise.resolve(jsonResponse(reservations))
    }

    throw new Error(`unexpected fetch: ${url.pathname}`)
  }) as unknown as typeof fetch
}

/**
 * Harness は `StorageBalance` と同じ 3 クエリを併走させ、それぞれの
 * `status`（'pending' | 'error' | 'success'）をテストから見える形で出す。
 * 同じ `QueryClientProvider` の下では同じ `queryKey` を指すクエリはキャッシュを
 * 共有するので、ここで見えるステータスは `StorageBalance` 内部のクエリの
 * ステータスそのもの。
 */
function Harness() {
  const storageQuery = useGetStorage()
  const recordingsQuery = useListRecordings({
    status: 'finished',
    limit: recentRecordingSampleLimit,
  })
  const reservationsQuery = useListReservations()

  return (
    <>
      <div data-testid="query-status">
        {storageQuery.status} {recordingsQuery.status} {reservationsQuery.status}
      </div>
      <StorageBalance />
    </>
  )
}

/** renderSettled は Harness を描き、3 クエリすべてが指定のステータスに確定するまで待つ。 */
async function renderSettled(expectedStatus: string) {
  const view = renderInRouter(<Harness />)
  await waitFor(() => expect(screen.getByTestId('query-status')).toHaveTextContent(expectedStatus))
  return view
}

describe('StorageBalance', () => {
  it('観測なし（media root が無い）ときは何も描かない', async () => {
    mockApis({ storage: [] })

    await renderSettled('success success success')

    // 3 クエリすべて解決した後で「無い」ことを見る（空虚な成功を避ける）。
    expect(screen.queryByText(/空き/)).not.toBeInTheDocument()
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })

  it('展開するとアーカイブとスクラッチの容量を階層ごとに表示する', async () => {
    const user = userEvent.setup()
    mockApis({ storage: [mediaRoot(), scratchRoot()] })

    await renderSettled('success success success')

    const summary = screen.getByText('ストレージ詳細')
    const details = summary.closest('details')
    expect(details).not.toHaveAttribute('open')

    await user.click(summary)

    expect(details).toHaveAttribute('open')
    expect(screen.getByText('アーカイブ')).toBeInTheDocument()
    expect(screen.getByText('スクラッチ')).toBeInTheDocument()
    expect(screen.getByText('186.3 GB')).toBeInTheDocument()
  })

  it('録画実績が 0 件のときは空きだけ出し、見込みは出さない', async () => {
    mockApis({ recordings: [], reservations: [reservation()] })

    await renderSettled('success success success')

    expect(screen.getAllByText(/空き/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })

  // 指摘 1: 予約の取得が失敗すると、以前は upcomingDurationMs が 0 に
  // フォールバックし「今後7日の予約で約 +0 B の見込み」という、欠損データから
  // 捏造した肯定を描いていた（実ブラウザで再現: 「空き 931.3 GB今後7日の予約で
  // 約 +0 B の見込み観測: 8/13 15:57」）。録画実績はあるが予約取得が失敗する
  // ケースを再現し、見込み欄が一切出ないことを確認する。
  it('予約の取得が失敗したときは見込みを出さない（残高のみ。「+0 B」を捏造しない）', async () => {
    mockApis({
      recordings: [recording()],
      reservationsStatus: 500,
    })

    await renderSettled('success success error')

    expect(screen.getAllByText(/空き/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
    // 「+0 B」という文字列そのものが出ていないことも明示的に確認する
    // （0 B は formatBytes(0) の出力そのものなので、これが出ていれば
    // 「見込みが算出された」ことの直接の証拠になる）。
    expect(screen.queryByText(/\+0 B/)).not.toBeInTheDocument()
  })

  // 指摘 2: 予約が正当に 0 件（取得は成功したが窓の中に予約が無い）のときも、
  // 指摘 1 と同じ表示（見込み欄が出ない）になるべきだが、内部的には別の経路
  // （hasEstimate: true, projectedConsumptionBytes: 0）を通る。表示結果は同じでも
  // 取得失敗と正当な 0 件を混ぜて同じ扱いにしていないことを、クエリステータス
  // （'success'）で区別して確認する。
  it('予約が正当に 0 件のときも見込みを出さない（0 のものは出さない）', async () => {
    mockApis({
      recordings: [recording()],
      reservations: [],
    })

    await renderSettled('success success success')

    expect(screen.getAllByText(/空き/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/\+0 B/)).not.toBeInTheDocument()
  })

  it('見込み消費が残量に収まるときは満杯見込みを出さない（下界主義）', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 1_000_000_000_000 })], // 1TB
      recordings: [recording()],
      // 1 件・30分。900MB/30分の実測なので今後 7 日の見込み消費は
      // 900MB 程度 --- 1TB の残量に対して十分に収まる
      reservations: [reservation()],
    })

    await renderSettled('success success success')

    expect(screen.getByText(/の見込み/)).toBeInTheDocument()
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

    await renderSettled('success success success')

    expect(screen.getByText(/満杯見込み/)).toBeInTheDocument()
  })

  it('観測が古い（1 時間超）ときは古い可能性を表示する', async () => {
    mockApis({
      storage: [mediaRoot({ observedAt: iso(-2 * 60 * 60 * 1000) })],
    })

    await renderSettled('success success success')

    expect(screen.getAllByText(/古い可能性/).length).toBeGreaterThan(0)
  })

  it('観測が新しい（1 時間以内）ときは古い可能性を表示しない', async () => {
    mockApis({
      storage: [mediaRoot({ observedAt: iso(-5 * 60 * 1000) })],
    })

    await renderSettled('success success success')

    expect(screen.getAllByText(/空き/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/古い可能性/)).not.toBeInTheDocument()
  })

  // skip=true の予約は消費に数えない（reconciler が mirakc に同期しないため）。
  // 全予約が skip=true なら見込み消費は 0 になり、指摘 2 の修正により
  // 「+0 B」は描かれない（見込み消費が 0 のときは見込み自体を出さない方針）。
  it('skip=true の予約は消費見込みに数えない（全 skip なら見込み消費 0 で見込み欄は出ない）', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 100_000_000 })], // 100MB
      recordings: [recording()],
      reservations: Array.from({ length: 7 }, (_, i) =>
        reservation({
          id: i + 1,
          programId: i + 1,
          startAt: iso(i * 24 * 60 * 60 * 1000 + 1000),
          skip: true,
        }),
      ),
    })

    await renderSettled('success success success')

    expect(screen.getAllByText(/空き/).length).toBeGreaterThan(0)
    expect(screen.queryByText(/の見込み/)).not.toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })

  // skip=true と skip=false が混在する場合は skip=false の分だけ数える
  // （上のテストが「全 skip なら 0」を見ているので、ここでは「一部だけ有効」を
  // 別途固定する --- 全 skip のケースだけでは skip フィルタが機能しているのか
  // 単に予約が無いのかを区別できない）。
  it('skip=true と skip=false が混在するときは skip=false の分だけ見込みに数える', async () => {
    mockApis({
      storage: [mediaRoot({ availableBytes: 1_000_000_000_000 })], // 1TB（収まる）
      recordings: [recording()],
      reservations: [reservation({ skip: false }), reservation({ id: 2, programId: 2, skip: true })],
    })

    await renderSettled('success success success')

    expect(screen.getByText(/の見込み/)).toBeInTheDocument()
    expect(screen.queryByText(/満杯見込み/)).not.toBeInTheDocument()
  })
})
