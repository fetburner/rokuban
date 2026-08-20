import type { QueryClient } from '@tanstack/react-query'
import { screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { ReservationsPage } from '@/pages/reservations'
import { renderInRouter } from '@/test/router'

/** 時刻はローカルの 0 時基準で組む（表示に時刻が入るのでタイムゾーンに依存させない）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

/** at は 0 時からの分数を ISO 文字列に直す。 */
function at(minutes: number): string {
  return new Date(dayStart.getTime() + minutes * 60_000).toISOString()
}

function reservation(
  id: number,
  title: string,
  startMinutes: number,
  durationMinutes: number,
  site = 'default',
  serviceName = 'テスト局',
): Reservation {
  return {
    id,
    site,
    programId: id * 10,
    source: 'manual',
    state: 'active',
    title,
    serviceName,
    startAt: at(startMinutes),
    durationMs: durationMinutes * 60_000,
    createdAt: at(0),
    updatedAt: at(0),
    skip: false,
  }
}

function overage(
  startMinutes: number,
  endMinutes: number,
  options: Partial<CapacityOverage> = {},
): CapacityOverage {
  return {
    site: 'default',
    startAt: at(startMinutes),
    endAt: at(endMinutes),
    shortfall: 1,
    jammedTypes: ['BS'],
    ...options,
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * stubApi は予約一覧・超過区間・サーキットブレーカー（AppShell が常に訊く）を振り分ける。
 *
 * 超過区間は時間窓で実際に絞る。窓を無視して全件返すスタブにすると、「一覧の予約を
 * 覆う窓で訊く」という実装の主張をテストが検証できない。
 */
function stubApi(reservations: Reservation[], overages: CapacityOverage[]) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/reservations') return Promise.resolve(jsonResponse(reservations))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    if (url.pathname === '/api/capacity/overages') {
      const start = new Date(url.searchParams.get('start') ?? 0).getTime()
      const end = new Date(url.searchParams.get('end') ?? 0).getTime()
      const matched = overages.filter(
        (o) => new Date(o.endAt).getTime() > start && new Date(o.startAt).getTime() < end,
      )
      return Promise.resolve(jsonResponse(matched))
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

/**
 * renderPage は `ReservationsPage` をルーターの中で描く。
 *
 * 行は詳細への `Link` を含むので、ルーターごと描かないと href を組めない
 * （`renderInRouter` を参照）。返す queryClient は「クエリが解決し終わった」
 * ことの待ち合わせに使う（バッジが出ないことを確かめるテストが、解決前に
 * 通るのを防ぐ）。
 */
function renderPage() {
  return renderInRouter(<ReservationsPage />, { path: '/reservations' })
}

function renderWith(reservations: Reservation[], overages: CapacityOverage[]) {
  const fetchMock = stubApi(reservations, overages)
  return { ...renderPage(), fetchMock }
}

/** row はタイトルからその予約の行を引く。 */
function row(title: string): HTMLElement {
  const el = screen.getByText(title).closest('li')
  if (!el) throw new Error(`row ${title} not found`)
  return el
}

/**
 * overagesSettled は超過区間のクエリが解決し、飛んでいる問い合わせが無くなるまで待つ。
 *
 * 「バッジが出ない」ことの確認はこれを通してから行う。`isFetching() === 0` だけを
 * 見ると、クエリがまだ始まっていない瞬間を「解決済み」と読んで通ってしまう
 * （CLAUDE.md「非同期の空虚な成功」）ので、成功した超過クエリの存在も要求する。
 *
 * 窓は予約一覧から作るので、キャッシュには「予約が届く前の窓（= 停止中）」の
 * エントリも残る。全部が success になることは求められない。
 */
async function overagesSettled(queryClient: QueryClient): Promise<void> {
  await waitFor(() => {
    expect(queryClient.isFetching()).toBe(0)
    const statuses = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['/api/capacity/overages'] })
      .map((query) => query.state.status)
    expect(statuses).toContain('success')
  })
}

/** capacityRequests は超過区間への問い合わせの URL を返す。 */
function capacityRequests(fetchMock: ReturnType<typeof stubApi>): URL[] {
  return fetchMock.mock.calls
    .map((call) => new URL(String(call[0]), 'http://localhost'))
    .filter((url) => url.pathname === '/api/capacity/overages')
}

describe('予約一覧のチューナー不足バッジ', () => {
  it('超過区間と交差する予約にだけ出る', async () => {
    const { queryClient } = renderWith(
      [reservation(1, '交差する番組', 19 * 60, 60), reservation(2, '交差しない番組', 22 * 60, 60)],
      [overage(19 * 60, 20 * 60)],
    )

    // 交差する側にバッジが出ることが「クエリが解決した」ことの証拠になるので、
    // 出ない側の確認が空虚な成功にならない
    expect(await screen.findByText('チューナー不足（BS が 1 本）')).toBeInTheDocument()
    expect(within(row('交差する番組')).getByText(/チューナー不足/)).toBeInTheDocument()
    expect(within(row('交差しない番組')).queryByText(/チューナー不足/)).toBeNull()
    await overagesSettled(queryClient)
  })

  it('別サイトの超過区間では出ない（判定はサイトごとに独立）', async () => {
    renderWith(
      [
        reservation(1, '同じサイトの番組', 19 * 60, 60),
        reservation(2, '別サイトの時間帯の番組', 21 * 60, 60),
      ],
      [
        overage(19 * 60, 20 * 60, { site: 'default' }),
        overage(21 * 60, 22 * 60, { site: 'takamatsu', shortfall: 2, jammedTypes: ['GR'] }),
      ],
    )

    // 高松の不足は default の予約に効かない。同時に default の不足が出ているので
    // 「まだ届いていないから出ていない」ではないことが分かる
    await waitFor(() =>
      expect(within(row('同じサイトの番組')).getByText(/チューナー不足/)).toBeInTheDocument(),
    )
    expect(within(row('別サイトの時間帯の番組')).queryByText(/チューナー不足/)).toBeNull()
    // 高松側の内訳（GR が 2 本）がどこにも漏れていない
    expect(screen.queryByText(/GR/)).toBeNull()
  })

  // 上のテストは「site で絞っている」ことしか担保しない。予約自身の site ではなく
  // 単一サイト前提の定数（'default'）を渡す実装でも、フィクスチャが全部 default
  // なら通ってしまう。**default 以外のサイトの予約に、同じサイトの不足を当てる**
  // ケースを置いて、定数を書いた実装で落ちるようにする。
  it('default 以外のサイトの予約にも自サイトの不足が出る', async () => {
    renderWith(
      [reservation(1, '高松の番組', 19 * 60, 60, 'takamatsu')],
      [overage(19 * 60, 20 * 60, { site: 'takamatsu', shortfall: 2, jammedTypes: ['GR'] })],
    )

    expect(await screen.findByText('チューナー不足（GR が 2 本）')).toBeInTheDocument()
  })

  it('区間の端で接するだけなら出ない', async () => {
    const { queryClient } = renderWith(
      [
        reservation(1, '接するだけの番組', 20 * 60, 60),
        reservation(2, '食い込む番組', 19 * 60 + 30, 60),
      ],
      [overage(19 * 60, 20 * 60)],
    )

    // 19:00-20:00 の不足に対し、20:00 開始の予約は不足の外側
    await waitFor(() =>
      expect(within(row('食い込む番組')).getByText(/チューナー不足/)).toBeInTheDocument(),
    )
    expect(within(row('接するだけの番組')).queryByText(/チューナー不足/)).toBeNull()
    await overagesSettled(queryClient)
  })

  it('超過区間が無ければ何も言わない（沈黙を肯定にしない）', async () => {
    const { queryClient } = renderWith([reservation(1, 'ニュース7', 19 * 60, 60)], [])

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    // 予約一覧が出たあと、超過の問い合わせが解決し切るまで待ってから確かめる
    await overagesSettled(queryClient)

    expect(screen.queryByText(/チューナー/)).toBeNull()
    // 「収まります」「競合なし」に相当する肯定的な表示は出さない
    expect(screen.queryByText(/競合/)).toBeNull()
  })

  it('複数区間に跨るときは最も不足の大きい区間の内訳を出す', async () => {
    renderWith(
      [reservation(1, '2 区間に跨る番組', 19 * 60, 120)],
      [
        overage(19 * 60, 20 * 60, { shortfall: 1, jammedTypes: ['GR'] }),
        overage(20 * 60, 21 * 60, { shortfall: 2, jammedTypes: ['BS'] }),
      ],
    )

    // 種別を合併して「GR・BS が 3 本」とは言わない（どの区間でも成り立たない主張）
    expect(await screen.findByText('チューナー不足（BS が 2 本）')).toBeInTheDocument()
  })

  it('一覧の予約を覆う窓で問い合わせる', async () => {
    const { queryClient, fetchMock } = renderWith(
      [reservation(1, '早い番組', 10 * 60, 30), reservation(2, '遅い番組', 19 * 60, 60)],
      [],
    )

    expect(await screen.findByText('早い番組')).toBeInTheDocument()
    await overagesSettled(queryClient)

    // 窓が固定幅だと、その外に出た予約のバッジが黙って消える
    const asked = capacityRequests(fetchMock).at(-1)
    expect(asked?.searchParams.get('start')).toBe(at(10 * 60))
    expect(asked?.searchParams.get('end')).toBe(at(20 * 60))
  })

  it('予約が無ければ超過を問い合わせない', async () => {
    const { fetchMock } = renderWith([], [])

    expect(await screen.findByText('予約がありません')).toBeInTheDocument()
    // 予約一覧の問い合わせは起きている（スタブが効いていることの確認）
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(0))

    expect(capacityRequests(fetchMock)).toHaveLength(0)
  })
})

/**
 * 容量バッジの番組表への導線（issue #233 M6-5）。
 *
 * バッジは行本体の `Link`（詳細への導線）の中に元々あった。バッジ自身も
 * `Link` になった以上、**`<a>` の中に `<a>` を作っていないこと**（コンテンツ
 * モデル上不正で、クリックの宛先が不定になる）を構造で確かめる。href の値
 * そのもの（宛先ルート・`at` パラメータ）はここで確認し、実際のクリックによる
 * 画面遷移・グリッドのスクロールは `web/e2e` で確認する（jsdom はレイアウトを
 * 測れない。CLAUDE.md テスト規律）。
 */
describe('予約一覧の容量バッジのリンク化（issue #233 M6-5）', () => {
  it('<a> の中に <a> を作らない（行本体のリンクの外に置く）', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    await screen.findByText('チューナー不足（BS が 1 本）')

    // 行本体のリンクと容量バッジのリンクが「入れ子」ではなく「兄弟」になっている
    // ことを、実際の DOM 構造で確かめる（querySelectorAll('a a') が入れ子の
    // 唯一の機械的な証拠）。
    expect(document.querySelectorAll('a a')).toHaveLength(0)

    const links = screen.getAllByRole('link')
    expect(links).toHaveLength(2)
  })

  it('番組表ルートへ、不足区間の開始時刻を at として積んだリンクになる', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    const badge = await screen.findByText('チューナー不足（BS が 1 本）')
    const badgeLink = badge.closest('a')
    expect(badgeLink).not.toBeNull()
    const expectedAtMs = new Date(at(19 * 60)).getTime()
    expect(badgeLink).toHaveAttribute('href', `/programs?at=${expectedAtMs}`)
  })

  it('容量バッジが無い行では追加のリンクは増えない', async () => {
    renderWith([reservation(1, 'ニュース7', 19 * 60, 60)], [])

    expect(await screen.findByText('ニュース7')).toBeInTheDocument()
    expect(screen.getAllByRole('link')).toHaveLength(1)
  })
})

/**
 * 行本体のリンクの accessible name（レビュー nit 1）。
 *
 * 行本体のリンクは `absolute inset-0` にして子要素を持たないため（must-fix 2
 * の再構成）、accessible name は `children` からではなく `aria-label` から
 * 計算される。この配線は前回のテスト（リンクの**本数**だけを見るもの）では
 * 検知できない --- `aria-label` を別の属性（例えば `data-row-label`）に
 * 変える壊し方でも本数は変わらないまま全行のリンクが無名になり、577 テスト
 * 全通過・build/lint clean のまま気付けなかった（レビュー実測）。
 * ここでは `getByRole('link', { name: ... })` で**名前による検索そのもの**が
 * 機能することを見る。
 */
describe('予約一覧の行本体リンクの accessible name（issue #233 レビュー nit 1）', () => {
  it('タイトルを含む名前でリンクを引け、宛先は予約詳細になる', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    // バッジ（別のリンク）も同時に存在する状態で、名前による検索が行本体の
    // リンクだけを一意に引けることまで確認する
    await screen.findByText('チューナー不足（BS が 1 本）')

    const rowLink = screen.getByRole('link', { name: /交差する番組/ })
    expect(rowLink).toHaveAttribute('href', '/reservations/default/10')
  })
})

/**
 * 多サイト時に一覧が何を出すか（`docs/frontend/shell.md`「サイトの扱い」）。
 *
 * `GET /api/reservations` は全サイトの予約を返し（api は site に束縛されない ---
 * 不変条件 1）、UI はそれを `<SiteGate>` が配る「現在の site」で絞らない。
 * 上の「default 以外のサイトの予約にも自サイトの不足が出る」は容量バッジの
 * `site` の配線を見るテストで、**一覧そのものが絞られていないこと**は主張の
 * 副産物として通っているに過ぎない（バッジが無い構成に変えると消える）ので、
 * 決定そのものをここで独立に固定する。
 *
 * 期待値は href のリテラルで書く。行の有無だけを見ると、宛先に単一サイト前提の
 * 定数を書いた実装（`params={{ site: 'default', ... }}`）でも通ってしまう。
 */
describe('多サイトの予約一覧（issue #218）', () => {
  it('現在サイト以外の予約も一覧に出し、宛先はその予約自身の site になる', async () => {
    // renderInRouter が SiteContext に流すのは 'default'（test/router.tsx の testSite）
    renderWith(
      [
        reservation(1, '既定サイトの番組', 19 * 60, 60),
        reservation(2, '高松の番組', 20 * 60, 60, 'takamatsu'),
      ],
      [],
    )

    // 現在サイトの行が出たことを読み込み完了の目印にする（クエリ解決前に
    // getAllByRole が空を返して通る「空虚な成功」を防ぐ）
    await screen.findByRole('link', { name: /既定サイトの番組/ })

    expect(screen.getAllByRole('link').map((a) => a.getAttribute('href'))).toEqual([
      '/reservations/default/10',
      '/reservations/takamatsu/20',
    ])
  })
})

/**
 * 警告の信号色。jsdom は色を計算しないので、当たっているクラスだけを見る
 * （実画素での判定は `web/e2e/design.mjs`。docs/frontend/design.md）。
 */
describe('予約一覧の信号色', () => {
  it('チューナー不足は琥珀（--warning）で、Tailwind 標準パレットを直接使わない', async () => {
    renderWith([reservation(1, '交差する番組', 19 * 60, 60)], [overage(19 * 60, 20 * 60)])

    const badge = (await screen.findByText('チューナー不足（BS が 1 本）')).closest('span')
      ?.parentElement
    expect(badge).not.toBeNull()
    expect(badge).toHaveClass('text-warning')
    expect(badge).toHaveClass('bg-warning/10')
    expect(badge!.className).not.toMatch(/amber|yellow|orange/)
  })

  it('EPG から消失は destructive のまま（警告色に落とさない）', async () => {
    renderWith(
      [{ ...reservation(1, '消えた番組', 19 * 60, 60), state: 'orphaned' as const }],
      [],
    )

    const badge = await screen.findByText('EPG から消失')
    expect(badge).toHaveClass('text-destructive')
    expect(badge.className).not.toMatch(/warning|tally/)
  })
})

/**
 * 予約一覧に局名（`program_snapshots.service_name` 由来）が出ること（issue #302）。
 *
 * 同じタイトルの番組が日付・局違いで並ぶと局名なしでは区別できないのが issue の
 * 観測そのものなので、**同タイトル 2 件を局名だけで見分けられる**ことを主張する。
 * タイトルが重複するため `row()` ヘルパー（`getByText` は一意な文字列前提）は
 * 使わず、`findAllByText` で得た 2 つのタイトル要素からそれぞれの行を辿る。
 */
describe('予約一覧の局名表示（issue #302）', () => {
  it('同タイトル・別局の予約を局名で区別できる', async () => {
    renderWith(
      [
        reservation(1, '同じ番組名', 19 * 60, 60, 'default', 'NHK総合'),
        reservation(2, '同じ番組名', 20 * 60, 60, 'default', 'NHK Eテレ'),
      ],
      [],
    )

    const titles = await screen.findAllByText('同じ番組名')
    expect(titles).toHaveLength(2)
    const rows = titles.map((el) => el.closest('li'))
    expect(rows[0]).not.toBeNull()
    expect(rows[1]).not.toBeNull()
    expect(within(rows[0]!).getByText('NHK総合')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('NHK Eテレ')).toBeInTheDocument()
  })

  /**
   * 行本体のリンクは `absolute inset-0` の空の `Link` なので、accessible name は
   * `aria-label`（= `rowLabel`）が唯一の情報源。上のテストは「行の中に局名の
   * テキストが居る」ことしか見ておらず、**リンク走査**（スクリーンリーダーの
   * リンク一覧・キーボード）では局名が読めないままでも通る（レビュー実測:
   * `aria-label` に局名が無い実装で 2 本のリンク名は
   * `["同じ番組名 7/25 19:00 1時間","同じ番組名 7/25 20:00 1時間"]`）。
   * 同時刻・別局（同名ニュースの裏かぶり）にすると 2 本の名前は完全に一致し、
   * 局名以外に差が無くなる。
   */
  it('同時刻・別局でも行本体リンクを局名を含む名前で一意に引ける', async () => {
    renderWith(
      [
        reservation(1, '同じ番組名', 19 * 60, 60, 'default', 'NHK総合'),
        reservation(2, '同じ番組名', 19 * 60, 60, 'default', 'NHK Eテレ'),
      ],
      [],
    )

    expect(await screen.findAllByText('同じ番組名')).toHaveLength(2)

    // 名前による検索で 1 本に絞れる（局名が名前に入っていなければ 2 本に
    // 当たって getByRole が投げる、または当たらずに投げる）
    expect(screen.getByRole('link', { name: /NHK総合/ })).toHaveAttribute(
      'href',
      '/reservations/default/10',
    )
    expect(screen.getByRole('link', { name: /NHK Eテレ/ })).toHaveAttribute(
      'href',
      '/reservations/default/20',
    )
  })
})
