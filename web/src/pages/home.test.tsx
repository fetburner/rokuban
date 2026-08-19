import { screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, CircuitBreaker, Recording, Reservation } from '@/api/generated'
import { HomePage } from '@/pages/home'
import { renderInRouter } from '@/test/router'

/** 時刻はローカルの 0 時基準で組む（表示に時刻が入るのでタイムゾーンに依存させない）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)
const nowMs = dayStart.getTime() + 20 * 3_600_000 // 当日 20:00 を「今」とする
const HOUR = 3_600_000

/**
 * ホームが完了録画を取るときに送る `limit`（`pages/home.tsx` の
 * `DROP_WARNING_SCAN_LIMIT`）。stub がこの値のリクエストだけに応えるので、
 * 実装が送る `limit` を変えるとフィクスチャが届かずテストが落ちる（意図的）。
 * 表示件数（6）の方は期待値としてリテラルで書く --- 実装の定数と比較する
 * アサーションは、定数を変えても通ってしまい何も主張しない。
 */
const DROP_WARNING_SCAN_LIMIT = 20

/**
 * ホームが失敗録画を取るときに送る `limit`（`pages/home.tsx` の
 * `FAILED_RECORDING_SCAN_LIMIT`）。上記 `DROP_WARNING_SCAN_LIMIT` と同じ理由で
 * リテラルにしてある。
 */
const FAILED_RECORDING_SCAN_LIMIT = 20

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
  /**
   * 完了録画（`status=finished&limit=20`）。ホームはこれの先頭 6 件を
   * 「直近の完了」に表示し、ドロップ警告は全件から拾う。
   */
  finished?: Recording[]
  /**
   * 失敗録画（`status=failed&limit=20`）。取得した全件のうち
   * `FAILED_RECORDING_WARNING_WINDOW_MS`（recency 窓）の中だけを警告セクションに
   * 出す。既定の `recording()` の `startAt` は「今」の 1 時間前なので窓には
   * 必ず収まる。
   */
  failed?: Recording[]
  reservations?: Reservation[]
  breakers?: CircuitBreaker[]
  overages?: CapacityOverage[]
  /** 特定パスの応答を意図的に遅延させ、読み込み中の状態を作るためのフック。 */
  pendingPaths?: Set<string>
  /**
   * `/api/recordings` の**特定の `status` だけ**を遅延させるフック。ホームは同じ
   * パスへ 3 本（`recording` / `finished` / `failed`）投げるので、`pendingPaths`
   * （パス単位）では 1 本だけを未解決にできない --- 「警告の材料 4 本のうち
   * 失敗録画だけが遅れている」を作るために `status` 単位の口が要る。
   */
  pendingRecordingStatuses?: Set<string>
  /**
   * 特定パスの **2 回目以降**の呼び出しだけを遅延させるフック。クエリキーが
   * 進んだ瞬間（時境界の越え際）に前のデータを見せ続けるかを、確定的に測る
   * ために使う --- 2 回目を即答させると「消えた一瞬」が assert より先に
   * 終わってしまい、壊れていても緑になる（CLAUDE.md「非同期の空虚な成功」）。
   */
  pendingAfterFirstCall?: Set<string>
  /** 特定パスを 500 で応答させる。 */
  errorPaths?: Set<string>
}

/**
 * stubApi はホームが叩く 6 本の GET を振り分ける。`/api/recordings` は `status`
 * クエリで「いま録画中」「完了録画（表示 + ドロップ検出）」「失敗録画（警告）」を
 * 分ける（サーバーの絞り込みを模す）。
 */
function stubApi(fixtures: Fixtures) {
  const pendingResolvers: Array<{ key: string; done: boolean; run: () => void }> = []
  const callCounts = new Map<string, number>()
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    const p = url.pathname
    const callCount = (callCounts.get(p) ?? 0) + 1
    callCounts.set(p, callCount)

    const respond = (): Response => {
      if (fixtures.errorPaths?.has(p)) return jsonResponse({ error: 'boom' }, 500)
      if (p === '/api/recordings') {
        const status = url.searchParams.get('status')
        const limit = url.searchParams.get('limit')
        if (status === 'recording') return jsonResponse(fixtures.recording ?? [])
        if (status === 'finished' && limit === String(DROP_WARNING_SCAN_LIMIT)) {
          return jsonResponse(fixtures.finished ?? [])
        }
        if (status === 'failed' && limit === String(FAILED_RECORDING_SCAN_LIMIT)) {
          return jsonResponse(fixtures.failed ?? [])
        }
        return jsonResponse([])
      }
      if (p === '/api/reservations') return jsonResponse(fixtures.reservations ?? [])
      if (p === '/api/breakers') return jsonResponse(fixtures.breakers ?? [])
      if (p === '/api/capacity/overages') return jsonResponse(fixtures.overages ?? [])
      throw new Error(`unexpected fetch: ${p}`)
    }

    // `/api/recordings` は 3 本（status ごと）が同じパスに来るので、遅延の識別子は
    // status まで含めた鍵にする（`unresolvedCount` もこの鍵で数える）。
    const recordingStatus = p === '/api/recordings' ? url.searchParams.get('status') : null
    const pendingKey = recordingStatus === null ? p : `${p}?status=${recordingStatus}`
    const isPending =
      fixtures.pendingPaths?.has(p) ||
      (recordingStatus !== null &&
        fixtures.pendingRecordingStatuses?.has(recordingStatus) === true) ||
      (fixtures.pendingAfterFirstCall?.has(p) === true && callCount > 1)
    if (isPending) {
      return new Promise<Response>((resolve) => {
        pendingResolvers.push({ key: pendingKey, done: false, run: () => resolve(respond()) })
      })
    }
    return Promise.resolve(respond())
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return {
    fetchMock,
    resolvePending: () => {
      for (const entry of pendingResolvers) {
        if (entry.done) continue
        entry.done = true
        entry.run()
      }
    },
    /**
     * unresolvedCount は `key` 宛で**まだ解決していない**応答の数。`key` はパス
     * （`/api/reservations`）か、`/api/recordings` なら status 付き
     * （`/api/recordings?status=failed`）。
     *
     * 「未解決のまま」を前提に書いたテストが、遅延の仕掛けが静かに効かなく
     * なった（= 即答するようになった）ときに空虚な成功へ転ぶのを防ぐための
     * ガード。前提そのものを assert できるようにしておく。
     */
    unresolvedCount: (key: string) =>
      pendingResolvers.filter((e) => e.key === key && !e.done).length,
  }
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

  /**
   * 容量超過クエリの `start` は時境界へ量子化してある（`pages/home.tsx`）ので、
   * サーバーは「最大 59 分前に始まって既に終わった超過区間」まで返しうる
   * （`openapi.yaml` の `start` は「この時刻より後に終わる区間が対象」）。それを
   * `activeOverages` の `endAt > now` が落として、量子化前と同じ主張の強さに
   * 戻している --- **その回収を実際に測る両方向の判定**（レビュー指摘: 以前は
   * この 1 行を `filter(() => true)` に変えても 17 件全部が緑だった。消すと
   * 「もう終わったチューナー不足」が最大 59 分ぶん警告に出続ける）。
   *
   * 「今」を時境界の 30 分後（20:30）に置くので、量子化された `start` は 20:00。
   * サーバー役の stub は `start` を見ずに返すので、ここで測るのは「時頭より後に
   * 終わった区間（= 量子化で新たに入ってくる分）をクライアントが落とすか」。
   */
  const halfPastNowMs = nowMs + 30 * 60_000

  it('既に終わった超過区間は警告に出さない（量子化で広げた窓の回収）', async () => {
    vi.setSystemTime(halfPastNowMs)
    // 20:00〜20:15 = 時頭より後に終わっており、量子化した `start`（20:00）では
    // サーバーの対象に入るが、実際の「今」（20:30）にはもう終わっている。
    stubApi({ overages: [overage(0, 15 * 60_000)] })
    renderHome()

    // 全クエリが解決したことを、単一の空状態が出ることで確かめてから不在を見る
    // （非同期の空虚な成功を避ける）。
    expect(await screen.findByText('表示できる項目がありません')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()
    expect(screen.queryByText(/チューナーが不足しています/)).not.toBeInTheDocument()
  })

  it('時境界より前に始まり進行中の超過区間は警告に出す（回収が広すぎない）', async () => {
    vi.setSystemTime(halfPastNowMs)
    // 18:00〜21:00 = 量子化した `start`（20:00）より前に始まっているが進行中。
    // 量子化の回収が「開始時刻」を見る実装になっていると、これを取り落とす。
    stubApi({ overages: [overage(-2 * HOUR, HOUR)] })
    renderHome()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/チューナーが不足しています/)).toBeInTheDocument()
  })

  it('直近完了にドロップが無く、ブレーカー・チューナー不足も無ければ警告は出ない（両方向）', async () => {
    stubApi({
      finished: [recording(9, 'きれいな録画', 'finished')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '直近の完了' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()
  })

  it('警告項目は種別ごとに固定の色クラスを持つ（チューナー不足=warning、ブレーカー/ドロップ/失敗録画=destructive）', async () => {
    // jsdom は実描画色を計算しないので、当たっているクラスだけを見る
    // （実画素は e2e/design.mjs が見る。`pages/reservations.test.tsx` の
    // 「警告の信号色」と同じ流儀）。文字列 key の前方一致で種別を推測する
    // 実装は、key の書式を変えただけで色が黙って壊れる（レビュー指摘）ので、
    // `WarningItem.kind` を経由していることをここで固定する。
    //
    // 失敗録画も同じ契約に入れる（レビュー指摘: `WarningKind` に 'failed' を
    // 足したのに色 × 種別の主張が無く、`amber` の条件に 'failed' を混ぜても
    // 全テストが緑だった）。録画が失われたことは取り返しがつかないので
    // destructive 側（`docs/frontend/design.md`「色は信号のみ」の表が
    // destructive を「失敗・ドロップ・…」と定めている）。
    stubApi({
      breakers: [breaker('ruler_deletes')],
      overages: [overage(2 * HOUR, 3 * HOUR)],
      finished: [
        recording(9, 'ドロップのある録画', 'finished', {
          dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        }),
      ],
      failed: [recording(10, '失敗した録画', 'failed')],
    })
    renderHome()

    const overageRow = (await screen.findByText(/チューナーが不足しています/)).closest('li')
    const breakerRow = screen.getByText(/ルール評価による予約の削除が停止中/).closest('li')
    const dropRow = screen.getByText(/ドロップのある録画: drop 12/).closest('li')
    const failedRow = screen.getByText(/失敗した録画: 録画失敗/).closest('li')

    // 色クラスは、リンクを持つ行（チューナー不足）では中の `<a>` に、
    // リンクを持たない行（ブレーカー・ドロップ）では `<li>` 自身に付く
    // （`WarningRow` の実装どおり）。`<a>` を持たない行のメッセージ `<span>`
    // 自体は色クラスを持たないので、そこを誤って掴まないよう `a` だけを探し、
    // 無ければ `<li>` 自身にフォールバックする。
    const colorElement = (row: HTMLElement | null) => row?.querySelector('a') ?? row
    expect(colorElement(overageRow)?.className).toMatch(/bg-warning\/10/)
    expect(colorElement(overageRow)?.className).toMatch(/text-warning/)
    for (const row of [breakerRow, dropRow, failedRow]) {
      const el = colorElement(row)
      expect(el?.className).toMatch(/text-destructive/)
      expect(el?.className).not.toMatch(/bg-warning/)
    }
  })
})

describe('ホーム: 失敗録画が警告に出る（issue #301）', () => {
  it('失敗録画があれば警告セクションに出る', async () => {
    stubApi({
      failed: [recording(9, '失敗した番組', 'failed')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/失敗した番組: 録画失敗/)).toBeInTheDocument()
  })

  it('失敗録画が無ければ警告に出ない（両方向）', async () => {
    stubApi({
      finished: [recording(9, 'きれいな録画', 'finished')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '直近の完了' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()
    expect(screen.queryByText(/録画失敗/)).not.toBeInTheDocument()
  })

  it('開始・終了の両方が記録されている失敗は、予定尺と実際に録れた尺を区別して出す', async () => {
    // 開始と終了が同じ秒（実際は 0 分）なのに、予定尺（5 分。issue #301 の実機
    // 観測と同じ値）だけを見ると「ほぼ予定通り録れた」ように誤読できてしまう。
    const startedAndEnded = iso(-30 * 60_000)
    stubApi({
      failed: [
        recording(9, '直後に切れた録画', 'failed', {
          durationMs: 5 * 60_000,
          startedAt: startedAndEnded,
          endedAt: startedAndEnded,
        }),
      ],
    })
    renderHome()

    // 実際尺（0 分）と予定尺（5 分）の両方が別々に出る --- 予定尺だけの表示に
    // 潰すと「実際 0分」が消え、この変異でテストが落ちる。
    expect(
      await screen.findByText(/直後に切れた録画: 録画失敗（実際 0分・予定 5分/),
    ).toBeInTheDocument()
  })

  it('開始が観測されていない失敗は「未開始」と出し、実際尺は主張しない', async () => {
    stubApi({
      failed: [
        recording(9, '開始が観測されていない録画', 'failed', {
          durationMs: 5 * 60_000,
          startedAt: undefined,
          endedAt: undefined,
        }),
      ],
    })
    renderHome()

    expect(
      await screen.findByText(/開始が観測されていない録画: 録画失敗（予定 5分・未開始/),
    ).toBeInTheDocument()
    // 「実際」という言葉は、開始の観測が無い以上出さない（実際尺が定義できない）
    expect(screen.queryByText(/実際/)).not.toBeInTheDocument()
  })

  it('startedAt だけが記録されている失敗は「未開始」と潰さず、実際尺も主張しない', async () => {
    // レビュー指摘: `UpdateRecordingStatus`（internal/db/queries/recordings.sql）は
    // `started_at` を無条件に、`ended_at` は非 NULL のときだけ書くので、mirakc の
    // failed record に `endTime` が無ければ `startedAt` だけが立つ行がある。
    // これを「未開始」と言うのは、開始した事実がある録画に「開始していない」と
    // 言う新しい嘘になる（issue #301 が問題にしているのと同じ種類の食い違い）。
    stubApi({
      failed: [
        recording(9, '終了未記録の録画', 'failed', {
          durationMs: 5 * 60_000,
          startedAt: iso(-30 * 60_000),
          endedAt: undefined,
        }),
      ],
    })
    renderHome()

    const row = await screen.findByText(/終了未記録の録画: 録画失敗/)
    expect(row.textContent).not.toMatch(/未開始/)
    expect(row.textContent).not.toMatch(/実際/)
  })

  it('failed 理由（recording.failed。オブジェクトの type フィールド）を出す', async () => {
    // internal/watcher/watcher.go の handleRecordingFailed は
    // json.Marshal(data.Reason) で書き、data.Reason は
    // mirakc.FailedReason（{type, message?, osError?, exitCode?}）なので
    // 素の文字列にはならない。
    stubApi({
      failed: [
        recording(9, '理由ありの失敗', 'failed', {
          qualityEvents: [
            { at: iso(-HOUR), event: 'recording.failed', reason: { type: 'tuner-unavailable' } },
          ],
        }),
      ],
    })
    renderHome()

    expect(await screen.findByText(/理由: tuner-unavailable/)).toBeInTheDocument()
  })

  it('failed 理由（recording.record-broken。オブジェクトの reason フィールド）を出す', async () => {
    // internal/watcher/watcher.go の handleRecordBroken は
    // map[string]string{"reason": data.Reason} で書く。
    stubApi({
      failed: [
        recording(9, '録画中に壊れた失敗', 'failed', {
          qualityEvents: [
            { at: iso(-HOUR), event: 'recording.record-broken', reason: { reason: 'io-error' } },
          ],
        }),
      ],
    })
    renderHome()

    expect(await screen.findByText(/理由: io-error/)).toBeInTheDocument()
  })

  it('失敗系イベントが期待した形を持たない未知のケースは JSON へフォールバックする', async () => {
    stubApi({
      failed: [
        recording(9, '未知の形の失敗', 'failed', {
          qualityEvents: [{ at: iso(-HOUR), event: 'recording.failed', reason: 'unexpected' }],
        }),
      ],
    })
    renderHome()

    expect(await screen.findByText(/理由: "unexpected"/)).toBeInTheDocument()
  })

  it('quality_events の最後の要素が bcas_anomaly でも、その前の失敗理由を読む', async () => {
    // quality_events は recording.failed / record-broken / bcas_anomaly が
    // 混ざる追記専用の履歴なので、「最後の要素」だけを見ると失敗理由が
    // bcas_anomaly（reason 無し）に上書きされる。
    stubApi({
      failed: [
        recording(9, '複数イベントの失敗', 'failed', {
          qualityEvents: [
            { at: iso(-2 * HOUR), event: 'recording.failed', reason: { type: 'io-error' } },
            { at: iso(-HOUR), event: 'bcas_anomaly' },
          ],
        }),
      ],
    })
    renderHome()

    expect(await screen.findByText(/理由: io-error/)).toBeInTheDocument()
  })

  it('failed 理由が無ければ「理由不明」と沈黙を区別する', async () => {
    stubApi({
      failed: [recording(9, '理由なしの失敗', 'failed', { qualityEvents: [] })],
    })
    renderHome()

    expect(await screen.findByText(/理由: 理由不明/)).toBeInTheDocument()
  })

  it('失敗理由のフィールドが空文字なら JSON へ落とさず「理由不明」に寄せる', async () => {
    // レビュー指摘: mirakc.FailedReason.Type に omitempty は無いので
    // `{"type":""}` はあり得る形。JSON フォールバックに落とすと
    // 「理由: {"type":""}」になり、材料が無い（沈黙）ことと区別できる
    // 文言にならない。
    stubApi({
      failed: [
        recording(9, '空の理由の失敗', 'failed', {
          qualityEvents: [{ at: iso(-HOUR), event: 'recording.failed', reason: { type: '' } }],
        }),
      ],
    })
    renderHome()

    const row = await screen.findByText(/空の理由の失敗: 録画失敗/)
    expect(row.textContent).toMatch(/理由: 理由不明/)
    expect(row.textContent).not.toMatch(/\{/)
  })

  it('失敗録画は録画単体ページへの導線を持つ', async () => {
    stubApi({ failed: [recording(9, '失敗した番組', 'failed')] })
    renderHome()

    const link = await screen.findByRole('link', { name: /失敗した番組/ })
    expect(link).toHaveAttribute('href', '/recordings/9')
  })

  it('recency 窓の外にある古い失敗は警告に出ない（issue の受け入れ基準「直近の」失敗録画）', async () => {
    stubApi({
      failed: [
        recording(9, '古い失敗', 'failed', { startAt: iso(-30 * 24 * HOUR) }),
        recording(10, '直近の失敗', 'failed', { startAt: iso(-HOUR) }),
      ],
    })
    renderHome()

    expect(await screen.findByText(/直近の失敗: 録画失敗/)).toBeInTheDocument()
    expect(screen.queryByText(/古い失敗: 録画失敗/)).not.toBeInTheDocument()
  })
})

describe('ホーム: ドロップ警告の検出範囲は「直近の完了」の表示件数から独立している', () => {
  it('表示上限（6 件）の外にある録画のドロップも警告には出る', async () => {
    // サーバーは `limit=20` の 7 件を返し、ホームは表示だけを先頭 6 件に切る。
    // 7 番目（表示には出ない）にドロップを持たせ、それでも警告に出ることを見る
    // --- 表示のスライスを警告の材料にも掛けてしまうと落ちる。
    const finished = Array.from({ length: 7 }, (_, i) =>
      recording(i + 1, `録画 ${i + 1}`, 'finished', { startAt: iso(-(i + 1) * HOUR) }),
    )
    finished[6] = {
      ...finished[6]!,
      dropSummary: { packets: 100, drops: 5, errors: 0, scrambled: 0 },
    }

    stubApi({ finished })
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

  it('警告は 4 本（ブレーカー・容量超過・ドロップ検出・失敗録画）すべての解決を待つ: 容量超過が遅い場合', async () => {
    // 「静かに空へ縮退させる」側（容量超過）が遅れているだけでも、既に届いた
    // ブレーカー 1 件だけで警告セクションを早出ししない（4 本の合成なので、
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

  /**
   * 4 本目（失敗録画）も同じ網に掛ける（レビュー指摘: `failedQuery.isPending` を
   * `warningsPending` から外しても、他の 3 本を固定したこのテストは緑のままだった）。
   *
   * 守っている挙動は 2 つ: 失敗録画だけが遅れているとき (a) 既に届いたブレーカー
   * 1 件で警告セクションを早出ししない、(b) 「表示できる項目がありません」を先に
   * 出してから警告が後出しで現れることがない。どちらも `warningsPending` に
   * `failedQuery.isPending` が入っていないと壊れる。
   */
  it('警告は 4 本（ブレーカー・容量超過・ドロップ検出・失敗録画）すべての解決を待つ: 失敗録画が遅い場合', async () => {
    const { resolvePending, unresolvedCount } = stubApi({
      breakers: [breaker('ruler_deletes')],
      pendingRecordingStatuses: new Set(['failed']),
    })
    renderHome()

    await new Promise((r) => setTimeout(r, 50))
    // 遅延の仕掛けが実際に効いていることを前提として assert する（即答に
    // 戻ったら以下の不在は空虚な成功になる）。
    expect(unresolvedCount('/api/recordings?status=failed')).toBe(1)
    expect(screen.queryByRole('heading', { name: '警告' })).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除が停止中/)).toBeInTheDocument()
  })

  it('失敗録画だけが未解決のうちは、単一の空状態（表示できる項目がありません）も出さない', async () => {
    // 他の 5 本が全部 0 件で解決していても、失敗録画が未解決なら「空である」と
    // まだ言い切れない --- 言ってしまうと、空状態を出したあとに警告が後出しで
    // 現れる（`allSettled` は `warningsPending` を経由して 4 本目に依存する）。
    const { resolvePending, unresolvedCount } = stubApi({
      failed: [recording(9, '後から届いた失敗', 'failed')],
      pendingRecordingStatuses: new Set(['failed']),
    })
    renderHome()

    await new Promise((r) => setTimeout(r, 50))
    expect(unresolvedCount('/api/recordings?status=failed')).toBe(1)
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()

    resolvePending()

    // 解決したら警告として出る（「たまたま速すぎて見えなかった」の排除）
    expect(await screen.findByText(/後から届いた失敗: 録画失敗/)).toBeInTheDocument()
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()
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
    // **下限も見る。** 上限だけの判定は「クエリを消した」「`enabled: false` に
    // した」「ページが起動しない」のいずれでも 0 回で緑になり、何も判定して
    // いない（レビュー指摘）。
    expect(overagesCalls.length).toBeGreaterThanOrEqual(1)
    expect(overagesCalls.length).toBeLessThanOrEqual(2)
  })
})

/**
 * should-fix（レビュー）: `start` の量子化により、キーは毎時 0 分に 1 回変わる。
 * 新しいキーにはまだデータが無いので、素のままだと `isPending` → 警告セクションが
 * 1 RTT だけ消える（警告だけが可視だった場合はページ全体がスケルトンに戻る）。
 * この画面の主題は「セクションが理由なく消えないこと」なので
 * `placeholderData: keepPreviousData` を置いた。**それが効いていることの判定。**
 */
describe('ホーム: 時境界を越えてキーが変わっても警告は消えない', () => {
  it('容量超過クエリのキーが進み、新キーが未解決のままでも警告セクションは残る', async () => {
    // 時境界（20:00）の直前に「今」を置く。量子化された `start` は 19:00。
    vi.setSystemTime(nowMs - 500)
    const { fetchMock, resolvePending, unresolvedCount } = stubApi({
      breakers: [breaker('ruler_deletes')],
      reservations: [reservation(1, '今夜の予約', 2 * HOUR)],
      // 再レンダーの引き金。これを解決させた瞬間に新しい `Date.now()` で
      // レンダーが走り、量子化後の `start` が 20:00 に進む。
      pendingPaths: new Set(['/api/reservations']),
      // 2 回目（= 新しいキー）の容量超過だけ未解決にする。即答させると
      // 「消えた一瞬」が assert より先に終わってしまい、壊れていても緑になる。
      pendingAfterFirstCall: new Set(['/api/capacity/overages']),
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '警告' })).toBeInTheDocument()

    // 時境界を越える
    vi.setSystemTime(nowMs + 500)
    resolvePending()

    // 予約が解決 = 新しい「今」でレンダーされたことの目印
    expect(await screen.findByRole('heading', { name: '今夜〜明日の予約' })).toBeInTheDocument()

    // キーが実際に進んだこと（`start` の違う 2 回目の要求が出たこと）を確かめる。
    // これが無いと「キーが変わらなかったので消えなかった」でも通ってしまう。
    const starts = fetchMock.mock.calls
      .map((call) => new URL(String(call[0]), 'http://localhost'))
      .filter((url) => url.pathname === '/api/capacity/overages')
      .map((url) => url.searchParams.get('start'))
    expect(new Set(starts).size).toBe(2)

    // **2 回目がこの時点でまだ未解決であること自体を assert する。**
    // `pendingAfterFirstCall` の仕掛けが静かに効かなくなって 2 回目も即答する
    // ようになると、`placeholderData` が無くても警告は戻ってきてしまい、
    // 下の 2 行が通る（このテストが測っているつもりのものを測らなくなる）。
    expect(unresolvedCount('/api/capacity/overages')).toBe(1)

    // 新しいキーは未解決のままだが、警告は消えていない
    expect(screen.getByRole('heading', { name: '警告' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除が停止中/)).toBeInTheDocument()
  })
})
