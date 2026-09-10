import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { ProgramListItem } from '@/api/generated'
import { ProgramRow } from '@/components/program-row'
import type { SiteProgram } from '@/lib/all-sites-services'
import { renderInRouter, testSite } from '@/test/router'

/** program は基準の未放送・未予約番組を作る。個々のテストで startAt/endAt を上書きする。 */
function program(overrides: Partial<ProgramListItem> = {}): SiteProgram {
  return {
    site: testSite,
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
function airingProgram(overrides: Partial<ProgramListItem> = {}): SiteProgram {
  const startAt = Date.now() - 10 * 60_000
  return program({
    startAt: new Date(startAt).toISOString(),
    endAt: new Date(startAt + 30 * 60_000).toISOString(),
    ...overrides,
  })
}

/**
 * stubFetch は ProgramRow の展開パネルが叩く番組詳細 + 能力 API を振り分ける。
 *
 * - `GET /api/capabilities`: ライブボタンの出し分け（issue #209 / #755）
 * - `GET .../programs/{programId}`: 展開時に `ProgramDetail` が問い合わせる番組詳細
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

describe('ProgramRow の外向き導線（issue #229 / #755）', () => {
  it('展開前は、予約済みの行でも展開領域の「予約の設定」が出ない', async () => {
    // 「予約の設定」は実体へのリンクなので展開領域に限定する。ライブは
    // 行に対する動作として予約列へ移ったため、展開前にも別のリンクが存在する。
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram({ programId: 7 })}
        reserved={true}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    expect(screen.queryByRole('link', { name: '予約の設定' })).not.toBeInTheDocument()
  })

  it('放送中の行では予約列にライブボタンが出て /live?service=<Service.id>&site=<site> を指す', async () => {
    const fetchMock = stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram({ networkId: 32736, serviceId: 1024 })}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('/api/capabilities'),
        expect.anything(),
      ),
    )

    const reserveWrapper = screen.getByTestId('program-row-reserve')
    const link = await within(reserveWrapper).findByRole('link', { name: 'ライブで見る' })
    expect(link).toHaveAttribute('aria-label', 'ライブで見る')
    expect(link.querySelector('svg')).not.toBeNull()
    const href = link.getAttribute('href') ?? ''
    expect(href.startsWith('/live?')).toBe(true)
    const params = new URLSearchParams(href.slice('/live?'.length))
    // 期待値はリテラルで書く（合成式を書き写すと `composeServiceId` の変更に
    // 追随してしまい何も主張しなくなる）。networkId 32736 / serviceId 1024。
    expect(params.get('service')).toBe('3273601024')
    expect(params.get('site')).toBe(testSite)
  })

  it('放送中でない行には予約列のライブボタンが出ない', async () => {
    const fetchMock = stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('/api/capabilities'),
        expect.anything(),
      ),
    )
    expect(
      within(screen.getByTestId('program-row-reserve')).queryByRole('link', {
        name: 'ライブで見る',
      }),
    ).not.toBeInTheDocument()
  })

  it('live.enabled=false（能力 API が disabled）では放送中でもライブボタンを出さない（ナビと同じ挙動）', async () => {
    const fetchMock = stubFetch({ live: false })
    renderInRouter(
      <ProgramRow
        program={airingProgram()}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('/api/capabilities'),
        expect.anything(),
      ),
    )
    expect(
      within(screen.getByTestId('program-row-reserve')).queryByRole('link', {
        name: 'ライブで見る',
      }),
    ).not.toBeInTheDocument()
  })

  it('予約済みの番組を展開すると「予約の設定」が出て /reservations/$site/$programId へのリンクになる', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program({ programId: 42 })}
        reserved={true}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()

    const link = await screen.findByRole('link', { name: '予約の設定' })
    expect(link).toHaveAttribute('href', `/reservations/${testSite}/42`)
  })

  it('ライブボタンは展開パネルに移らず、放送中で未予約の行には設定リンクも出ない', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={airingProgram()}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await expandRow()
    await waitFor(() => expect(screen.queryByText('詳細を読み込み中…')).not.toBeInTheDocument())

    const detail = document.getElementById('program-row-detail-1')
    expect(detail).not.toBeNull()
    if (!detail) throw new Error('展開パネルが見つからない')
    expect(within(detail).queryByRole('link', { name: 'ライブで見る' })).not.toBeInTheDocument()
    expect(within(detail).queryByRole('link', { name: '予約の設定' })).not.toBeInTheDocument()
    expect(
      await within(screen.getByTestId('program-row-reserve')).findByRole('link', {
        name: 'ライブで見る',
      }),
    ).toBeInTheDocument()
  })
})

describe('ProgramRow の操作列の開閉配線（issue #310 / #755）', () => {
  // 実際の開閉（列幅が :hover / :focus-visible / pointer メディア特性で
  // w-0 ↔ w-20（放送中は w-[7.75rem]）に変わること）は jsdom では測れない
  // （レイアウトを持たない）
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
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    const title = await screen.findByText('対象番組')
    const toggle = title.closest('button')
    expect(toggle).not.toBeNull()
    expect(toggle).toHaveClass('peer')
    expect(toggle).toHaveAttribute('aria-expanded', 'false')

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
        reservationStateUnknown={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    await screen.findByText('対象番組')
    const reserveWrapper = screen.getByTestId('program-row-reserve')
    expect(within(reserveWrapper).getByRole('button', { name: '取消' })).toBeInTheDocument()
  })

  it('defaultExpanded を指定すると、モーダル用に詳細と操作列を初期展開する', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        defaultExpanded
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    const title = await screen.findByText('対象番組')
    const toggle = title.closest('button')
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('エンコードプロファイル')).toBeInTheDocument()
  })

  it('defaultExpanded={false} なら、リスト用に詳細と予約列を折りたたんで出す', async () => {
    stubFetch()
    renderInRouter(
      <ProgramRow
        program={program()}
        reserved={false}
        pending={false}
        reservationStateUnknown={false}
        defaultExpanded={false}
        onReserve={vi.fn()}
        onCancel={vi.fn()}
      />,
    )

    const title = await screen.findByText('対象番組')
    const toggle = title.closest('button')
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('エンコードプロファイル')).not.toBeInTheDocument()
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
        reservationStateUnknown={false}
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
        reservationStateUnknown={false}
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
