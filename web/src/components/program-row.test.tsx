import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { ProgramListItem } from '@/api/generated'
import { ProgramRow } from '@/components/program-row'
import { renderInRouter, testSite } from '@/test/router'

/** program は基準の未放送・未予約番組を作る。個々のテストで startAt/endAt を上書きする。 */
function program(overrides: Partial<ProgramListItem> = {}): ProgramListItem {
  return {
    programId: 1,
    networkId: 32736,
    serviceId: 1024,
    eventId: 1,
    startAt: new Date(Date.now() + 100 * 3_600_000).toISOString(),
    endAt: new Date(Date.now() + 101 * 3_600_000).toISOString(),
    name: '対象番組',
    description: '',
    durationMs: 3_600_000,
    genres: [0],
    isFree: true,
    ...overrides,
  }
}

/** airingProgram は「いま放送中」（`isAiring` が true）になるよう startAt/endAt を組む。 */
function airingProgram(overrides: Partial<ProgramListItem> = {}): ProgramListItem {
  const startAt = Date.now() - 10 * 60_000
  return program({
    startAt: new Date(startAt).toISOString(),
    endAt: new Date(startAt + 30 * 60_000).toISOString(),
    ...overrides,
  })
}

/**
 * stubFetch は ProgramRow の展開パネルが叩く 2 本 + 能力 API を振り分ける。
 *
 * - `GET /api/capabilities`: 「ライブで見る」の出し分け（issue #209 / #229）
 * - `GET .../overlaps`: 未予約行が常に問い合わせる（`ProgramOverlapWarning`）
 * - `GET .../programs/{id}`: 展開時に `ProgramDetail` が問い合わせる番組詳細
 */
function stubFetch({ live = true }: { live?: boolean } = {}) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(
        new Response(JSON.stringify({ live }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    if (url.pathname.endsWith('/overlaps')) {
      return Promise.resolve(
        new Response(JSON.stringify({ count: 0, reservations: [] }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    if (url.pathname === '/api/encode-profiles') {
      // 未予約行の展開パネル（EncodeSettingsFields）が引く。この issue の関心
      // 事ではないので空配列で足りる。
      return Promise.resolve(
        new Response(JSON.stringify([]), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    // 番組詳細（ProgramDetail）。テストは中身を見ないので最小限。
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

async function expandRow() {
  const user = userEvent.setup()
  // ルーターの初回マッチ解決は非同期（test/router.tsx のコメント）なので、
  // 行本体が描かれるまで待ってからクリックする。
  const title = await screen.findByText('対象番組')
  const button = title.closest('button')
  if (!button) throw new Error('行の展開ボタンが見つからない')
  await user.click(button)
}

describe('ProgramRow の外向き導線（issue #229）', () => {
  it('展開前は、放送中かつ予約済みの行でも外向きリンクが 1 つも出ない（展開領域限定）', async () => {
    // issue #229 の「決定済みの方向」（固有名詞へのリンクは折りたたみ行では
    // なく展開領域側に置く）そのものを守るテスト。他のテストは全部
    // expandRow() を通してから見るので、展開前に何が無いかを見るのはこの
    // 1 本だけ --- リンクを `{expanded && ...}` の外（行ヘッダの兄弟）へ
    // 移す変異は、他のテストを崩さずにこれだけを落とす。
    const fetchMock = stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram({ programId: 7 })}
        reserved={true}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    // 行本体が描かれるまで待つ（ルーターの初回マッチ解決は非同期）。
    await screen.findByText('対象番組')
    // 能力 API の解決を待ってから確認する。未解決のうちは fail-closed で
    // 元から「ライブで見る」が出ないだけなので、それだけで「出ない」を
    // 確認すると空虚な成功になる（CLAUDE.md テスト規律）。
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('/api/capabilities'),
        expect.anything(),
      ),
    )

    expect(screen.queryAllByRole('link')).toHaveLength(0)
  })

  it('放送中の番組を展開すると「ライブで見る」が出て /live?serviceId= （SI の serviceId）へのリンクになる', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram({ serviceId: 1024 })}
        reserved={false}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    const link = await screen.findByRole('link', { name: 'ライブで見る' })
    expect(link).toHaveAttribute('href', '/live?serviceId=1024')
  })

  it('放送中でない番組を展開しても「ライブで見る」は出ない', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    // 番組詳細（ProgramDetail）の読み込みが解決するまで待ってから「無い」ことを
    // 確認する（非同期の空虚な成功を避ける。CLAUDE.md テスト規律）。
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())
    expect(screen.queryByRole('link', { name: 'ライブで見る' })).not.toBeInTheDocument()
  })

  it('live.enabled=false（能力 API が disabled）では放送中でも「ライブで見る」を出さない（ナビと同じ挙動）', async () => {
    stubFetch({ live: false })
    renderInRouter(
      <ProgramRow
        program={airingProgram()}
        reserved={false}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    // 能力 API（pending の間は fail-closed でまだ出ていないだけ）の解決を、
    // 同じ展開パネルが問い合わせる番組詳細の解決を目印に待つ
    // （app-shell.test.tsx の waitForNavSettled と同じ「別の確実な完了を
    // 目印にしてマイクロタスクを回す」やり方）。
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())
    expect(screen.queryByRole('link', { name: 'ライブで見る' })).not.toBeInTheDocument()
  })

  it('予約済みの番組を展開すると「予約の詳細」が出て /reservations/$site/$programId へのリンクになる', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program({ programId: 42 })}
        reserved={true}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    const link = await screen.findByRole('link', { name: '予約の詳細' })
    expect(link).toHaveAttribute('href', `/reservations/${testSite}/42`)
  })

  it('未予約の番組を展開しても「予約の詳細」は出ない', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())

    expect(screen.queryByRole('link', { name: '予約の詳細' })).not.toBeInTheDocument()
  })
})
