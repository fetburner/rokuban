import { screen, waitFor, within } from '@testing-library/react'
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

  /**
   * issue #438: `/live` の URL も他画面と同じ `?service=<Service.id>`
   * （`networkId * 100000 + serviceId`）に統一した。`program` は SI の
   * `networkId` / `serviceId` しか持たないため、リンクはそこから合成する。
   * href のクエリ文字列を個別に検証する（キーの順序に依存する文字列一致には
   * しない）。
   */
  it('放送中の番組を展開すると「ライブで見る」が出て /live?service=<Service.id> へのリンクになる', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram({ networkId: 32736, serviceId: 1024 })}
        reserved={false}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    const link = await screen.findByRole('link', { name: 'ライブで見る' })
    const href = link.getAttribute('href') ?? ''
    expect(href.startsWith('/live?')).toBe(true)
    const params = new URLSearchParams(href.slice('/live?'.length))
    // 期待値はリテラルで書く（合成式を書き写すと `composeServiceId` の変更に
    // 追随してしまい何も主張しなくなる）。networkId 32736 / serviceId 1024。
    expect(params.get('service')).toBe('3273601024')
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

  it('予約済みの番組を展開すると「予約の設定」が出て /reservations/$site/$programId へのリンクになる', async () => {
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

    const link = await screen.findByRole('link', { name: '予約の設定' })
    expect(link).toHaveAttribute('href', `/reservations/${testSite}/42`)
  })

  it('未予約の番組を展開しても「予約の設定」は出ない', async () => {
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

    expect(screen.queryByRole('link', { name: '予約の設定' })).not.toBeInTheDocument()
  })
})

describe('ProgramRow の予約列の開閉配線（issue #310）', () => {
  // 実際の開閉（列幅が :hover / :focus-visible / pointer メディア特性で
  // w-0 ↔ w-20 に変わること）は jsdom では測れない（レイアウトを持たない）
  // --- 唯一の判定は e2e/reserve-visibility.mjs（web/e2e/README.md）。ここで
  // 見るのは、その CSS が依存する配線（`group` / `peer` マーカーと
  // `data-testid`）が消えていないことだけ。マーカーが消えると e2e はセレクタが
  // 見つからず即座に落ちるが、原因調査の手間を減らすため、より速い jsdom 側にも
  // 同じ配線を固定しておく。
  it('行トグルが `peer` を持ち、予約ボタンの wrapper が `data-testid="program-row-reserve"` を持つ', async () => {
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

    const title = await screen.findByText('対象番組')
    const toggle = title.closest('button')
    expect(toggle).not.toBeNull()
    expect(toggle).toHaveClass('peer')

    const reserveWrapper = screen.getByTestId('program-row-reserve')
    // ホバー / フォーカス駆動の可視性が乗る `group`（行コンテナ）と
    // `peer-aria-expanded`（タッチ側）の両方の基準点が生きていることを、
    // wrapper が行トグルの後続の兄弟であることで確認する --- `peer-*` は
    // 「先行する兄弟」にしか効かないため、順序が入れ替わると壊れる。
    expect(toggle?.nextElementSibling).toBe(reserveWrapper)
    expect(reserveWrapper.parentElement).toHaveClass('group')

    expect(within(reserveWrapper).getByRole('button', { name: '予約' })).toBeInTheDocument()
  })

  it('予約済みの行でも「取消」ボタンが同じ wrapper（program-row-reserve）に入る', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={true}
        pending={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    const reserveWrapper = screen.getByTestId('program-row-reserve')
    expect(within(reserveWrapper).getByRole('button', { name: '取消' })).toBeInTheDocument()
  })
})

describe('ProgramRow の送信中フィードバック（issue #298）', () => {
  // 送信中（pending）はスピナーを重ねず、楽観更新で確定したラベルを出したまま
  // ボタンを disabled にする。スピナーは楽観更新の確定表示を 1 フレーム覆い隠して
  // 高速応答時に点滅していた（#298 実測）ため削除した。disabled 中の淡い dim
  // （Button の `disabled:opacity-50` + `transition opacity`）が送信中の唯一の
  // 手掛かりで、これはネットワーク速度に自然に追従する（jsdom では実測できない
  // ので、ここで見るのはラベルと disabled と「スピナーが無い」ことだけ）。
  it('予約実行中（pending かつ楽観 reserved）はスピナーを出さず「取消」を disabled で保つ', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={true}
        pending={true}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    const reserveWrapper = screen.getByTestId('program-row-reserve')
    expect(reserveWrapper.querySelector('.animate-spin')).toBeNull()
    expect(within(reserveWrapper).getByRole('button', { name: '取消' })).toBeDisabled()
  })

  it('取消実行中（pending かつ楽観未予約）はスピナーを出さず「予約」を disabled で保つ', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={true}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    const reserveWrapper = screen.getByTestId('program-row-reserve')
    expect(reserveWrapper.querySelector('.animate-spin')).toBeNull()
    expect(within(reserveWrapper).getByRole('button', { name: '予約' })).toBeDisabled()
  })
})
