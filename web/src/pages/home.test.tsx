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
 * HomePage は `Date.now()` を直接呼ぶ（`pages/programs.tsx` と同じ規律。注入口を
 * 持たない）。フィクスチャの `nowMs` と実際の `Date.now()` を一致させないと、
 * 窓判定（今夜〜明日の予約）がフィクスチャの時刻を「はるか過去」として全除外して
 * しまう。`vi.useFakeTimers()` を呼ばずに `setSystemTime` だけ使うと `Date.*` /
 * `new Date()` だけがモックされ、`waitFor`/`findBy*` が使う実タイマーはそのまま
 * 動く（vitest の挙動。fake timers 全体を有効にすると async の待ち合わせを
 * 自前で進める必要が出て、ここでは要らない複雑さになる）。
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
  finished?: Recording[]
  reservations?: Reservation[]
  breakers?: CircuitBreaker[]
  overages?: CapacityOverage[]
  /** 特定パスの応答を意図的に遅延させ、読み込み中の状態を作るためのフック。 */
  pendingPaths?: Set<string>
  /** 特定パスを 500 で応答させる。 */
  errorPaths?: Set<string>
}

/**
 * stubApi はホームが叩く 5 本の GET を振り分ける。`status` クエリで
 * recording/finished を分ける（サーバーの絞り込みを模す）。
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
        if (status === 'recording') return jsonResponse(fixtures.recording ?? [])
        if (status === 'finished') return jsonResponse(fixtures.finished ?? [])
        return jsonResponse([])
      }
      if (p === '/api/reservations') return jsonResponse(fixtures.reservations ?? [])
      if (p === '/api/breakers') return jsonResponse(fixtures.breakers ?? [])
      if (p === '/api/capacity/overages') return jsonResponse(fixtures.overages ?? [])
      throw new Error(`unexpected fetch: ${p}`)
    }

    if (fixtures.pendingPaths?.has(p)) {
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

describe('ホーム: 読み込み中は空セクション判定をしない', () => {
  it('クエリが解決するまでは、見出しも単一の空状態も出さない', async () => {
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
