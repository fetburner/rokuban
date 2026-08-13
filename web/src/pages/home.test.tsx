import { screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, CircuitBreaker, Recording, Reservation } from '@/api/generated'
import { HomePage } from '@/pages/home'
import { renderInRouter } from '@/test/router'

/** 時刻はローカルの 0 時基準で組む（表示に時刻が入るのでタイムゾーンに依存させない）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)
const nowMs = dayStart.getTime() + 20 * 3_600_000 // 当日 20:00 を「今」とする
const HOUR = 3_600_000

/** ホームが「直近の完了」の表示に使う上限（`pages/home.tsx` の `RECENT_FINISHED_LIMIT`）。 */
const RECENT_FINISHED_LIMIT = 6
/** ホームがドロップ警告の検出に使う、表示件数とは独立の上限（`DROP_WARNING_SCAN_LIMIT`）。 */
const DROP_WARNING_SCAN_LIMIT = 20

/**
 * HomePage は `Date.now()` を直接呼ぶ（`pages/programs.tsx` と同じ規律。注入口を
 * 持たない）。フィクスチャの `nowMs` と実際の `Date.now()` を一致させないと、
 * 窓判定（今夜〜明日の予約）がフィクスチャの時刻を「はるか過去」として全除外して
 * しまう。`vi.useFakeTimers()` を呼ばずに `setSystemTime` だけ使うと `Date.*` /
 * `new Date()` だけがモックされ、`waitFor`/`findBy*` が使う実タイマーはそのまま
 * 動く（vitest の挙動。fake timers 全体を有効にすると async の待ち合わせを
 * 自前で進める必要が出て、ここでは要らない複雑さになる）。
 *
 * **例外は「実時計でのクエリキー安定性」の describe ブロック** --- そこでは
 * `vi.useRealTimers()` でこの固定を明示的に解除する。時計を止めた構成は
 * 「時計が動くことに起因する欠陥」を原理的に検出できないため（レビューで発覚。
 * docs/frontend/home.md §経緯と失敗事例）。
 */
beforeEach(() => {
  vi.setSystemTime(nowMs)
})
afterEach(() => {
  vi.useRealTimers()
})

function iso(offsetMsFromNow: number): string {
  return new Date(nowMs + offsetMsFromNow).toISOString()
}

function recording(id: number, title: string, status: Recording['status'], overrides: Partial<Recording> = {}): Recording {
  return {
    id,
    site: 'default',
    source: 'manual',
    serviceName: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    networkId: 32736,
    serviceId: 1024,
    eventId: id,
    title,
    startAt: iso(-HOUR),
    durationMs: HOUR,
    status,
    createdAt: iso(-HOUR),
    ...overrides,
  }
}

function reservation(id: number, title: string, startOffsetMs: number, overrides: Partial<Reservation> = {}): Reservation {
  return {
    id,
    site: 'default',
    programId: id * 10,
    source: 'manual',
    state: 'active',
    title,
    startAt: iso(startOffsetMs),
    durationMs: HOUR,
    createdAt: iso(-HOUR),
    updatedAt: iso(-HOUR),
    skip: false,
    ...overrides,
  }
}

function breaker(name: CircuitBreaker['name'] = 'ruler_deletes'): CircuitBreaker {
  return {
    site: 'default',
    name,
    trippedAt: iso(-HOUR),
    pending: 42,
    threshold: 20,
    detail: { total: 42 },
  }
}

function overage(startOffsetMs: number, endOffsetMs: number): CapacityOverage {
  return {
    site: 'default',
    startAt: iso(startOffsetMs),
    endAt: iso(endOffsetMs),
    shortfall: 1,
    jammedTypes: ['BS'],
  }
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

type Fixtures = {
  recording?: Recording[]
  /** 「直近の完了」表示用（`limit=6`）。省略時は `dropScan` があればそれを、無ければ空を使う。 */
  finished?: Recording[]
  /**
   * ドロップ警告検出用（`limit=20`）。省略時は `finished` にフォールバックする ---
   * 「表示とは独立」を明示的に検証したいテストだけがこれを指定する。
   */
  dropScan?: Recording[]
  reservations?: Reservation[]
  breakers?: CircuitBreaker[]
  overages?: CapacityOverage[]
  /** 特定パスの応答を意図的に遅延させ、読み込み中の状態を作るためのフック。 */
  pendingPaths?: Set<string>
  /**
   * `/api/recordings` を pending にするとき、`status=recording` /
   * `status=finished&limit=6` / `status=finished&limit=20` のどれを遅らせるか
   * を絞り込む述語。省略時は `pendingPaths` に `/api/recordings` があれば
   * status を問わず全部遅らせる。
   */
  pendingRecordingsMatch?: (url: URL) => boolean
  /** 特定パスを 500 で応答させる。 */
  errorPaths?: Set<string>
}

/**
 * stubApi はホームが叩く 6 本の GET を振り分ける。`status` / `limit` クエリで
 * 「いま録画中」「直近の完了（表示）」「ドロップ警告検出用」を分ける
 * （サーバーの絞り込みを模す）。
 */
function stubApi(fixtures: Fixtures) {
  const pendingResolvers: Array<() => void> = []
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    const p = url.pathname

    const respond = (): Response => {
      if (fixtures.errorPaths?.has(p)) return jsonResponse({ error: 'boom' }, 500)
      if (p === '/api/recordings') {
        const status = url.searchParams.get('status')
        const limit = url.searchParams.get('limit')
        if (status === 'recording') return jsonResponse(fixtures.recording ?? [])
        if (status === 'finished' && limit === String(RECENT_FINISHED_LIMIT)) {
          return jsonResponse(fixtures.finished ?? [])
        }
        if (status === 'finished' && limit === String(DROP_WARNING_SCAN_LIMIT)) {
          return jsonResponse(fixtures.dropScan ?? fixtures.finished ?? [])
        }
        return jsonResponse([])
      }
      if (p === '/api/reservations') return jsonResponse(fixtures.reservations ?? [])
      if (p === '/api/breakers') return jsonResponse(fixtures.breakers ?? [])
      if (p === '/api/capacity/overages') return jsonResponse(fixtures.overages ?? [])
      throw new Error(`unexpected fetch: ${p}`)
    }

    const isPending =
      fixtures.pendingPaths?.has(p) &&
      (p !== '/api/recordings' || !fixtures.pendingRecordingsMatch || fixtures.pendingRecordingsMatch(url))
    if (isPending) {
      return new Promise<Response>((resolve) => {
        pendingResolvers.push(() => resolve(respond()))
      })
    }
    return Promise.resolve(respond())
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return { fetchMock, resolvePending: () => pendingResolvers.forEach((r) => r()) }
}

function renderHome() {
  return renderInRouter(<HomePage />, { path: '/' })
}

describe('ホーム: 全セクションが空のときの単一の空状態', () => {
  it('4 セクションとも 0 件なら見出しを 1 つも出さず、単一の空状態だけを出す', async () => {
    stubApi({})
    renderHome()

    expect(await screen.findByText('表示できる項目がありません')).toBeInTheDocument()
    for (const heading of ['いま録画中', '今夜〜明日の予約', '警告', '直近の完了']) {
      expect(screen.queryByRole('heading', { name: heading })).not.toBeInTheDocument()
    }
    // 「異常なし」「予約がありません」のような肯定/報告の文言を書いていない
    expect(screen.queryByText(/異常/)).not.toBeInTheDocument()
  })

  it('1 セクションでもあれば単一の空状態は出ない（両方向）', async () => {
    stubApi({ recording: [recording(1, '録画中の番組', 'recording')] })
    renderHome()

    expect(await screen.findByRole('heading', { name: 'いま録画中' })).toBeInTheDocument()
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()
  })
})

describe('ホーム: 0 件のセクションは文言も出さず消える', () => {
  it('いま録画中が 0 件ならセクションごと消える（他のセクションは出る）', async () => {
    stubApi({ reservations: [reservation(1, '今夜の予約', 2 * HOUR)] })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今夜〜明日の予約' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'いま録画中' })).not.toBeInTheDocument()
    // 「録画中の番組がありません」のような文言も出さない
    expect(screen.queryByText(/録画中/)).not.toBeInTheDocument()
  })
})

describe('ホーム: 今夜〜明日の予約の窓', () => {
  it('今から明日の暦日の終わりまでに入る予約だけを表示し、外は除外する', async () => {
    stubApi({
      reservations: [
        reservation(1, '過去に始まった予約', -HOUR),
        reservation(2, '窓に入る予約', 3 * HOUR),
        reservation(3, '明後日以降の予約', 30 * HOUR),
      ],
    })
    renderHome()

    expect(await screen.findByText('窓に入る予約')).toBeInTheDocument()
    expect(screen.queryByText('過去に始まった予約')).not.toBeInTheDocument()
    expect(screen.queryByText('明後日以降の予約')).not.toBeInTheDocument()
  })

  it('ちょうど 10 件なら「予約をすべて見る」は出ない', async () => {
    const reservations = Array.from({ length: 10 }, (_, i) =>
      reservation(i + 1, `予約 ${i + 1}`, (i + 1) * HOUR),
    )
    stubApi({ reservations })
    renderHome()

    expect(await screen.findByText('予約 1')).toBeInTheDocument()
    expect(screen.getByText('予約 10')).toBeInTheDocument()
    expect(screen.queryByText('予約をすべて見る')).not.toBeInTheDocument()
  })

  it('10 件を超えたら先頭 10 件のみ表示し、「予約をすべて見る」を出す', async () => {
    const reservations = Array.from({ length: 11 }, (_, i) =>
      reservation(i + 1, `予約 ${i + 1}`, (i + 1) * HOUR),
    )
    stubApi({ reservations })
    renderHome()

    expect(await screen.findByText('予約 1')).toBeInTheDocument()
    expect(screen.getByText('予約 10')).toBeInTheDocument()
    expect(screen.queryByText('予約 11')).not.toBeInTheDocument()
    const link = screen.getByRole('link', { name: '予約をすべて見る' })
    expect(link).toHaveAttribute('href', '/reservations')
  })
})

describe('ホーム: 「いま録画中」「直近の完了」の行は警告と二重に主張しない', () => {
  it('ドロップがあっても行にバッジは出ない（警告セクションだけが一覧化する）', async () => {
    stubApi({
      finished: [
        recording(9, 'ドロップのある録画', 'finished', {
          dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        }),
      ],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '直近の完了' })).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    // 警告セクションのテキストとしては出るが、行自体には drop バッジ（DropBadges
    // 由来の "drop" ラベル）を重ねない
    expect(screen.getByText(/ドロップのある録画: drop 12 \/ scrambled 3/)).toBeInTheDocument()
    const row = screen.getByText('ドロップのある録画', { selector: 'span' }).closest('li')
    expect(row).not.toBeNull()
    expect(row!.textContent).not.toMatch(/drop 12/)
  })
})

describe('ホーム: 警告セクション', () => {
  it('サーキットブレーカー・チューナー不足・直近完了のドロップを集約する', async () => {
    stubApi({
      breakers: [breaker('ruler_deletes')],
      overages: [overage(2 * HOUR, 3 * HOUR)],
      finished: [
        recording(9, 'ドロップのある録画', 'finished', {
          dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        }),
      ],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除が停止中/)).toBeInTheDocument()
    expect(screen.getByText(/チューナーが不足しています/)).toBeInTheDocument()
    expect(screen.getByText(/ドロップのある録画: drop 12 \/ scrambled 3/)).toBeInTheDocument()
  })

  it('チューナー不足の項目は番組表のその時間帯への導線を持つ', async () => {
    const shortage = overage(2 * HOUR, 3 * HOUR)
    stubApi({ overages: [shortage] })
    renderHome()

    const link = await screen.findByRole('link', { name: /チューナーが不足しています/ })
    const expectedAtMs = new Date(shortage.startAt).getTime()
    expect(link).toHaveAttribute('href', `/programs?at=${expectedAtMs}`)
  })

  it('直近完了にドロップが無く、ブレーカー・チューナー不足も無ければ警告は出ない（両方向）', async () => {
    stubApi({
      finished: [recording(9, 'きれいな録画', 'finished')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '直近の完了' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()
  })

  it('警告項目は種別ごとに固定の色クラスを持つ（チューナー不足=warning、ブレーカー/ドロップ=destructive）', async () => {
    // jsdom は実描画色を計算しないので、当たっているクラスだけを見る
    // （実画素は e2e/design.mjs が見る。`pages/reservations.test.tsx` の
    // 「警告の信号色」と同じ流儀）。文字列 key の前方一致で種別を推測する
    // 実装は、key の書式を変えただけで色が黙って壊れる（レビュー指摘）ので、
    // `WarningItem.kind` を経由していることをここで固定する。
    stubApi({
      breakers: [breaker('ruler_deletes')],
      overages: [overage(2 * HOUR, 3 * HOUR)],
      dropScan: [
        recording(9, 'ドロップのある録画', 'finished', {
          dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        }),
      ],
    })
    renderHome()

    const overageRow = (await screen.findByText(/チューナーが不足しています/)).closest('li')
    const breakerRow = screen.getByText(/ルール評価による予約の削除が停止中/).closest('li')
    const dropRow = screen.getByText(/ドロップのある録画: drop 12/).closest('li')

    // 色クラスは、リンクを持つ行（チューナー不足）では中の `<a>` に、
    // リンクを持たない行（ブレーカー・ドロップ）では `<li>` 自身に付く
    // （`WarningRow` の実装どおり）。`<a>` を持たない行のメッセージ `<span>`
    // 自体は色クラスを持たないので、そこを誤って掴まないよう `a` だけを探し、
    // 無ければ `<li>` 自身にフォールバックする。
    const colorElement = (row: HTMLElement | null) => row?.querySelector('a') ?? row
    expect(colorElement(overageRow)?.className).toMatch(/bg-warning\/10/)
    expect(colorElement(overageRow)?.className).toMatch(/text-warning/)
    for (const row of [breakerRow, dropRow]) {
      const el = colorElement(row)
      expect(el?.className).toMatch(/text-destructive/)
      expect(el?.className).not.toMatch(/bg-warning/)
    }
  })
})

describe('ホーム: ドロップ警告の検出範囲は「直近の完了」の表示件数から独立している', () => {
  it('表示上限（6 件）の外にある録画のドロップも警告には出る', async () => {
    // 「直近の完了」表示には出ない（7 番目以降）が、ドロップ検出用の
    // より広い問い合わせ（limit=20）には入っている録画を用意する。
    const dropScan = Array.from({ length: 7 }, (_, i) =>
      recording(i + 1, `録画 ${i + 1}`, 'finished', { startAt: iso(-(i + 1) * HOUR) }),
    )
    // 7 番目（表示の 6 件には入らない）にドロップを持たせる
    dropScan[6] = {
      ...dropScan[6]!,
      dropSummary: { packets: 100, drops: 5, errors: 0, scrambled: 0 },
    }
    const finished = dropScan.slice(0, 6)

    stubApi({ finished, dropScan })
    renderHome()

    expect(await screen.findByRole('heading', { name: '直近の完了' })).toBeInTheDocument()
    // 表示（直近の完了）には出ない
    expect(screen.queryByText('録画 7')).not.toBeInTheDocument()
    // が、警告には出る（検出範囲が表示件数から独立していることの証拠）
    expect(await screen.findByText(/録画 7: drop 5/)).toBeInTheDocument()
  })
})

describe('ホーム: 取得失敗はセクションを隠さずエラー表示にする', () => {
  it('いま録画中の取得が失敗しても、0 件と違いセクション自体は表示してエラーを出す', async () => {
    stubApi({ errorPaths: new Set(['/api/recordings']) })
    renderHome()

    expect(await screen.findByRole('heading', { name: 'いま録画中' })).toBeInTheDocument()
    expect(screen.getByText('録画中の取得に失敗しました')).toBeInTheDocument()
    // 0 件のときの「セクションごと消す」とは違う挙動であることの確認
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()
  })
})

describe('ホーム: セクションごとに独立して読み込み、空セクション判定はしない', () => {
  it('遅いセクション（予約）が未解決でも、解決済みのセクション（いま録画中）は先に表示する', async () => {
    // レビュー指摘: `GET /api/reservations` は絞り込みを持たない全件取得で、
    // 予約が増えるほど遅くなりうる。これに「いま録画中」のような速く・最も
    // 見たいセクションまで引きずられて隠れないことを固定する。
    const { resolvePending } = stubApi({
      recording: [recording(1, '録画中の番組', 'recording')],
      reservations: [reservation(2, '今夜の予約', 2 * HOUR)],
      pendingPaths: new Set(['/api/reservations']),
    })
    renderHome()

    // 「いま録画中」は予約を待たずに出る
    expect(await screen.findByRole('heading', { name: 'いま録画中' })).toBeInTheDocument()
    // 予約はまだ解決していないので、まだ何も言っていない（見出しも出ない。
    // 「0 件だから消えている」のではなく「まだ分からない」）
    expect(screen.queryByRole('heading', { name: '今夜〜明日の予約' })).not.toBeInTheDocument()
    // まだ全部は解決していないので、単一の空状態も出さない
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByRole('heading', { name: '今夜〜明日の予約' })).toBeInTheDocument()
  })

  it('警告は 3 本（ブレーカー・容量超過・ドロップ検出）すべての解決を待つ', async () => {
    // 「静かに空へ縮退させる」側（容量超過）が遅れているだけでも、既に届いた
    // ブレーカー 1 件だけで警告セクションを早出ししない（3 本の合成なので、
    // 未解決の 1 本があるうちは「まだ分からない」のまま）。
    const { resolvePending } = stubApi({
      breakers: [breaker('ruler_deletes')],
      pendingPaths: new Set(['/api/capacity/overages']),
    })
    renderHome()

    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除が停止中/)).toBeInTheDocument()
  })

  it('全セクションが未解決の間は、見出しも単一の空状態も出さない', async () => {
    const { resolvePending } = stubApi({ pendingPaths: new Set(['/api/recordings']) })
    renderHome()

    // 解決前: 何も判定していないことを確認する（非同期の空虚な成功対策）。
    // 少し待っても状態が出ないことを確認してから、実際に解決させて正しい
    // 表示に切り替わることまで見る --- 「たまたま速すぎて見えなかった」を
    // 排除するため、解決後の表示が変わることも合わせて確認する。
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'いま録画中' })).not.toBeInTheDocument()

    resolvePending()

    await waitFor(() =>
      expect(screen.getByText('表示できる項目がありません')).toBeInTheDocument(),
    )
  })
})

/**
 * must-fix（レビュー）: 容量超過クエリの `start` に生の `Date.now()` を渡すと、
 * レンダーごとにキャッシュキーが変わり無限に再取得し続ける（docs/frontend/home.md
 * §経緯と失敗事例。実測: stub 即答で 4 秒に 37 回、実サーバー相当の遅延では
 * 4 秒間ずっと全画面スケルトンのまま収束しなかった）。
 *
 * **この describe ブロックだけ時計を止めない。** 他の全テストは
 * `beforeEach` の `vi.setSystemTime` で時計を固定しているが、時計を止めた
 * 構成では `Date.now()` が常に同じ値を返すため、この種の欠陥（レンダーごとに
 * 変わる生ミリ秒がキャッシュキーに入る）を原理的に検出できない
 * （`e2e/design.mjs` の `page.clock.setFixedTime` も同じ盲点を持つ ---
 * そちらには時計を止めない別の判定を足してある）。
 */
describe('ホーム: 実時計でのクエリキー安定性（無限再取得の回帰検出）', () => {
  it('容量超過クエリへの問い合わせが実質 1 回に収束する（レンダーごとに新しいキーにならない）', async () => {
    vi.useRealTimers()

    const { fetchMock } = stubApi({})
    renderHome()

    // 実時計で 1.5 秒待つ。`start` に生の Date.now() を渡す実装に戻すと、
    // レンダー → 新キー → 未解決 → 即解決（stub は同期的）→ 再レンダー →
    // また新キー…のループがこの間に数十回発生する。
    await new Promise((resolve) => setTimeout(resolve, 1500))

    const overagesCalls = fetchMock.mock.calls.filter(
      (call) =>
        new URL(String(call[0]), 'http://localhost').pathname === '/api/capacity/overages',
    )
    expect(overagesCalls.length).toBeLessThanOrEqual(2)
  })
})
