import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import {
  useListRecordingDropStats,
  type DropStat,
  type EncodeProfileSummary,
  type Recording,
} from '@/api/generated'
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

  // M3-24（#136）で GET /api/recordings の limit 既定が 50 になった。
  // この画面はまだページング UI を持たず（M3-25 で置き換え予定）、返った配列を
  // 全部描画するので、limit を明示しないと 50 件を超えるライブラリ・ごみ箱が
  // 黙って頭打ちになる（PR #187 レビュー M4）。明示的に渡していることを固定する。
  it('limit を明示的に渡す（既定 50 での黙った頭打ちを避ける）', async () => {
    const fetchMock = vi.fn((_input: string | URL | Request) =>
      Promise.resolve(
        new Response(JSON.stringify([sampleRecording()]), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()

    const calls = fetchMock.mock.calls.filter((call) => {
      const url = new URL(String(call[0]), 'http://localhost')
      return url.pathname === '/api/recordings'
    })
    expect(calls.length).toBeGreaterThan(0)
    for (const call of calls) {
      const url = new URL(String(call[0]), 'http://localhost')
      expect(url.searchParams.get('limit')).toBe('200')
    }
  })

  // issue #135: 完全削除（purge）が完了した録画は purged_at IS NULL の条件で
  // ListTrashRecordings から外れるので、API は空配列を返す。この「ごみ箱の
  // 中身が全部 purge 済みで 0 件」という経路は、この修正が入るまで実際に
  // 踏まれたことが無かった（それまでは purge してもごみ箱に残り続けるバグが
  // あったため）。目視ではなくここで固定する。
  it('ごみ箱の GET が空配列を返すと「ごみ箱は空です」を出す', async () => {
    const user = userEvent.setup()
    const fetchMock = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      const trash = url.searchParams.get('trash') === 'true'
      const body = trash ? [] : [sampleRecording()]
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    })

    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    expect(await screen.findByText('ライブラリの録画')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))

    expect(await screen.findByText('ごみ箱は空です')).toBeInTheDocument()
    expect(screen.queryByText('ライブラリの録画')).not.toBeInTheDocument()
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

    // 確定後はダイアログが閉じる（開いたまま残らない）
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: '完全削除を予約する' })).not.toBeInTheDocument(),
    )

    // ダイアログが自動で閉じても mutate のコールバックは走る（黙って成功したように
    // 見せない）。AlertDialogAction を Close ラップにした #131 で、閉じるのが先に
    // 走ってトーストが出なくなる形の回帰を防ぐ。
    expect(await screen.findByText(/完全削除を予約しました/)).toBeInTheDocument()
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
    const fetchMock = vi.fn(() => {
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

    // jsdom は <img src> / <video src> を実際の fetch には流さないので、
    // 検証できるのは DOM 上にこれらの要素が存在しないことまで
    // （fetch モックへのリクエスト有無では確認できない）。
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
  function makeFetchMock({
    recording,
    profiles,
    onAddEncodeProfiles,
  }: {
    recording: Recording
    profiles: EncodeProfileSummary[]
    onAddEncodeProfiles?: (id: number, body: unknown) => void
  }) {
    return vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'

      if (url.pathname === '/api/encode-profiles' && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify(profiles), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      const addMatch = /^\/api\/recordings\/(\d+)\/encode-profiles$/.exec(url.pathname)
      if (addMatch && method === 'POST') {
        const body: unknown = init?.body ? JSON.parse(String(init.body)) : undefined
        onAddEncodeProfiles?.(Number(addMatch[1]), body)
        return Promise.resolve(new Response(null, { status: 204 }))
      }
      if (url.pathname === '/api/recordings' && method === 'GET') {
        const trash = url.searchParams.get('trash') === 'true'
        return Promise.resolve(
          new Response(JSON.stringify(trash ? [] : [recording]), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      return Promise.resolve(new Response('not found', { status: 404 }))
    })
  }

  it('encodeProfiles（desired）にあるものは選択肢から外し「追加済み」に出す', async () => {
    const user = userEvent.setup()
    const recording = sampleRecording({
      id: 11,
      title: '一部追加済み',
      sizeBytes: 1_000_000,
      encodeProfiles: ['h264'],
    })
    const fetchMock = makeFetchMock({
      recording,
      profiles: [{ name: 'h264' }, { name: 'h265' }],
    })
    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(await screen.findByText('一部追加済み'))

    expect(await screen.findByText('事後エンコードの追加')).toBeInTheDocument()
    expect(screen.getByText('追加済み: h264')).toBeInTheDocument()
    // 既に追加済みの h264 はチェックボックスとして選ばせない（二重依頼に見せない）。
    expect(screen.queryByRole('checkbox', { name: 'h264' })).not.toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'h265' })).toBeInTheDocument()
  })

  it('全プロファイルが追加済みなら、選択肢もボタンも出さず案内だけ出す', async () => {
    const user = userEvent.setup()
    const recording = sampleRecording({
      id: 15,
      title: '全部追加済み',
      sizeBytes: 1_000_000,
      encodeProfiles: ['h264'],
    })
    const fetchMock = makeFetchMock({ recording, profiles: [{ name: 'h264' }] })
    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(await screen.findByText('全部追加済み'))

    expect(await screen.findByText('すべてのエンコードプロファイルが追加済みです。')).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: '追加エンコードを依頼' }),
    ).not.toBeInTheDocument()
  })

  it('選択したプロファイルを POST し、成功したらトーストを出して選択を空に戻す', async () => {
    const user = userEvent.setup()
    const recording = sampleRecording({
      id: 12,
      title: '追加できる録画',
      sizeBytes: 500,
      encodeProfiles: [],
    })
    const addCalls: unknown[] = []
    const fetchMock = makeFetchMock({
      recording,
      profiles: [{ name: 'h264' }],
      onAddEncodeProfiles: (id, body) => addCalls.push({ id, body }),
    })
    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(await screen.findByText('追加できる録画'))
    await user.click(await screen.findByRole('checkbox', { name: 'h264' }))
    await user.click(screen.getByRole('button', { name: '追加エンコードを依頼' }))

    await waitFor(() => expect(addCalls).toHaveLength(1))
    expect(addCalls[0]).toEqual({ id: 12, body: { profiles: ['h264'] } })
    expect(await screen.findByText('エンコードを依頼しました')).toBeInTheDocument()
  })

  it('原本削除済み（sizeBytes 省略）では追加できない旨を出し、チェックボックスを出さない', async () => {
    const user = userEvent.setup()
    // sizeBytes を指定しない = 原本削除済み（recordingFromListFields の射影と同じ）。
    const recording = sampleRecording({ id: 13, title: '原本削除済み', encodeProfiles: [] })
    const fetchMock = makeFetchMock({ recording, profiles: [{ name: 'h264' }] })
    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(await screen.findByText('原本削除済み'))

    expect(
      await screen.findByText('原本が削除済みのため、追加のエンコードは依頼できません。'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  it('ごみ箱では追加エンコードのコントロールを一切出さない', async () => {
    const user = userEvent.setup()
    const recording = sampleRecording({
      id: 14,
      title: '捨てた録画・エンコード確認',
      deletedAt: '2026-01-05T00:00:00Z',
      sizeBytes: 500,
      encodeProfiles: [],
    })
    const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const method = init?.method ?? 'GET'
      if (url.pathname === '/api/encode-profiles') {
        return Promise.resolve(
          new Response(JSON.stringify([{ name: 'h264' }]), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      if (url.pathname === '/api/recordings' && method === 'GET') {
        const trash = url.searchParams.get('trash') === 'true'
        return Promise.resolve(
          new Response(JSON.stringify(trash ? [recording] : []), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      }
      return Promise.resolve(new Response('not found', { status: 404 }))
    })
    renderRecordingsPage(fetchMock as unknown as typeof fetch)

    await user.click(screen.getByRole('button', { name: 'ごみ箱' }))
    await user.click(await screen.findByText('捨てた録画・エンコード確認'))

    // 展開後の内容（削除日時）が出るまで待ってから「無い」ことを確認する
    // （クエリ未解決のうちに queryBy で通ってしまう空虚な成功を避ける）
    await screen.findByText('削除日時')
    expect(screen.queryByText('事後エンコードの追加')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })
})
