import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CapacityOverage, CircuitBreaker, EncodeQueueSummary, Recording, Reservation, StorageRoot } from '@/api/generated'
import { HOME_MODE_STORAGE_KEY } from '@/lib/home-mode'
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
 * web/e2e/README.md §判定を足すときの規律）。
 */
beforeEach(() => {
  vi.setSystemTime(nowMs)
})
afterEach(() => {
  vi.useRealTimers()
  localStorage.removeItem(HOME_MODE_STORAGE_KEY)
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
    keepOriginal: 'always',
    cmDetection: { state: 'disabled' },
    createdAt: iso(-HOUR),
    ...overrides,
  }
}

function reservation(id: number, title: string, startOffsetMs: number, overrides: Partial<Reservation> = {}): Reservation {
  return {
    site: 'default',
    programId: id * 10,
    source: 'manual',
    state: 'active',
    title,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: iso(startOffsetMs),
    durationMs: HOUR,
    createdAt: iso(-HOUR),
    updatedAt: iso(-HOUR),
    skip: false,
    series: null,
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
   * 完了録画。ドロップ警告（`status=finished&limit=20`）は全件から拾い、
   * 時間軸（`limit=200` + `from` / `to`）は開始時刻の範囲で絞って返す。
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
  continueWatching?: Recording[]
  storage?: StorageRoot[]
  encodeQueue?: EncodeQueueSummary
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
 * stubApi はホームが叩く 7 本の GET を振り分ける。`/api/recordings` は `status`
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
        const from = url.searchParams.get('from')
        const to = url.searchParams.get('to')
        if (status === null) throw new Error('home must not fetch recordings without status')
        // サーバーと同じく `from` / `to` は開始時刻（`program_start_at`）の範囲で、
        // 付いたものだけで絞る。
        const inRange = (item: Recording) => {
          const start = new Date(item.startAt).getTime()
          return (from === null || start >= new Date(from).getTime()) &&
            (to === null || start < new Date(to).getTime())
        }
        const serverFailed = (fixtures.failed ?? []).filter(
          (item) => (item as Recording & { supersededAt?: string }).supersededAt === undefined,
        )
        if (limit === '200') {
          const source = status === 'recording'
            ? fixtures.recording
            : status === 'finished'
              ? fixtures.finished
              : status === 'failed'
                ? serverFailed
                : []
          return jsonResponse((source ?? []).filter(inRange))
        }
        if (status === 'recording') return jsonResponse(fixtures.recording ?? [])
        if (status === 'finished' && limit === String(DROP_WARNING_SCAN_LIMIT)) {
          return jsonResponse(fixtures.finished ?? [])
        }
        if (status === 'failed' && limit === String(FAILED_RECORDING_SCAN_LIMIT)) {
          return jsonResponse(serverFailed)
        }
        return jsonResponse([])
      }
      if (p === '/api/recordings/continue-watching') return jsonResponse(fixtures.continueWatching ?? [])
      if (p === '/api/reservations') return jsonResponse(fixtures.reservations ?? [])
      if (p === '/api/breakers') return jsonResponse(fixtures.breakers ?? [])
      if (p === '/api/capacity/overages') return jsonResponse(fixtures.overages ?? [])
      if (p === '/api/storage') return jsonResponse(fixtures.storage ?? [])
      if (p === '/api/encode-queue') return jsonResponse(fixtures.encodeQueue ?? { queued: 0, running: 0 })
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

function renderHome(path = '/?mode=ops') {
  return renderInRouter(<HomePage />, { path: '/', initialEntries: [path] })
}

/** 「要対応」の行をタイトルで探す（時間軸の詳細一覧や title 属性と取り違えない）。 */
async function findWarningRow(title: string): Promise<HTMLElement> {
  const section = (await screen.findByRole('heading', { name: '要対応' })).closest('section')!
  const el = await within(section).findByText(title, { selector: '[data-testid="warning-title"]' })
  return el.closest('li')!
}

async function openTimelineDetails() {
  const details = await screen.findByTestId('home-timeline-details')
  fireEvent.click(within(details).getByText('録画・予約の詳細'))
  return details
}

describe('ホーム: 見る / 管理モード（issue #1020）', () => {
  it('既定は「見る」、URL が無いときだけ端末の保存値を使う', async () => {
    stubApi({})
    const defaultHome = renderHome('/')
    const toggle = await screen.findByTestId('home-mode-toggle')
    expect(within(toggle).getByRole('link', { name: '見る' })).toHaveAttribute('aria-current', 'page')
    expect(defaultHome.router.state.location.search).toMatchObject({})
    defaultHome.unmount()

    localStorage.setItem(HOME_MODE_STORAGE_KEY, 'ops')
    stubApi({})
    renderHome('/')
    const opsToggle = await screen.findByTestId('home-mode-toggle')
    expect(within(opsToggle).getByRole('link', { name: '管理' })).toHaveAttribute('aria-current', 'page')
    expect(await screen.findByText('表示できる項目がありません')).toBeInTheDocument()
  })

  it('有効な URL が保存値より優先され、選択したモードを保存する', async () => {
    localStorage.setItem(HOME_MODE_STORAGE_KEY, 'ops')
    stubApi({})
    renderHome('/?mode=watch')
    const toggle = await screen.findByTestId('home-mode-toggle')
    const watch = within(toggle).getByRole('link', { name: '見る' })
    expect(watch).toHaveAttribute('aria-current', 'page')
    fireEvent.click(within(toggle).getByRole('link', { name: '管理' }))
    await waitFor(() => expect(localStorage.getItem(HOME_MODE_STORAGE_KEY)).toBe('ops'))
    expect(await screen.findByText('表示できる項目がありません')).toBeInTheDocument()
  })

  it('未知の URL 値は無視し、保存値が無ければ見るへ戻る', async () => {
    stubApi({})
    renderHome('/?mode=unexpected')
    const toggle = await screen.findByTestId('home-mode-toggle')
    expect(within(toggle).getByRole('link', { name: '見る' })).toHaveAttribute('aria-current', 'page')
  })

  it('続きからと完了録画の両方が解決するまで主役を決めない', async () => {
    const api = stubApi({
      finished: [recording(90, '後から届く完了録画', 'finished', { sizeBytes: 100 })],
      pendingPaths: new Set(['/api/recordings/continue-watching']),
    })
    renderHome('/?mode=watch')
    await waitFor(() =>
      expect(api.unresolvedCount('/api/recordings/continue-watching')).toBe(1),
    )
    expect(screen.queryByText('後から届く完了録画')).not.toBeInTheDocument()
    expect(screen.queryByTestId('home-primary-action')).not.toBeInTheDocument()

    await act(async () => api.resolvePending())
    expect(await screen.findByText('後から届く完了録画')).toBeInTheDocument()
    const primaryAction = screen.getByTestId('home-primary-action')
    const thumbnail = screen.getByTestId('home-next-watch-thumbnail')
    expect(primaryAction).toHaveTextContent('再生')
    expect(primaryAction).toHaveAttribute('href', '/recordings/90')
    expect(thumbnail.tagName).toBe('A')
    expect(thumbnail).toHaveAttribute('href', '/recordings/90')
    expect(thumbnail).toHaveAttribute('tabindex', '-1')
    expect(thumbnail).toHaveAttribute('aria-hidden', 'true')
  })

  it('続きからを主役にし、最初からのリンクは再開位置を復元しない詳細へ向ける', async () => {
    stubApi({
      continueWatching: [
        recording(31, '続きの番組', 'finished', { resumePositionMs: 330_000 }),
        recording(32, '次の続き', 'finished', { resumePositionMs: 420_000 }),
      ],
      finished: [recording(33, 'もっと新しい未視聴', 'finished', { sizeBytes: 100 })],
    })
    renderHome('/?mode=watch')

    expect(await screen.findByRole('heading', { name: '続きの番組' })).toBeInTheDocument()
    const primaryAction = await screen.findByTestId('home-primary-action')
    expect(primaryAction).toHaveTextContent('続きから再生')
    expect(primaryAction).toHaveAttribute('href', '/recordings/31')
    const thumbnail = screen.getByTestId('home-next-watch-thumbnail')
    expect(thumbnail.tagName).toBe('A')
    expect(thumbnail).toHaveAttribute('href', '/recordings/31')
    expect(thumbnail).toHaveAttribute('tabindex', '-1')
    expect(thumbnail).toHaveAttribute('aria-hidden', 'true')
    expect(primaryAction).not.toHaveAttribute('tabindex', '-1')
    const beginning = screen.getByRole('link', { name: '最初から' })
    expect(beginning.getAttribute('href')).toContain('fromBeginning=true')
    expect(screen.queryByText(/再生元/)).not.toBeInTheDocument()
    // ラフ（#1020）: ▶ 付きの主ボタン、メタは 1 行で位置を末尾に、サムネ内に局名と進み線
    expect(screen.getByTestId('home-primary-action')).toHaveTextContent('▶')
    expect(screen.getByText(/\(.\) \d\d:\d\d · NHK総合 · 5:30 \/ 1:00:00$/)).toBeInTheDocument()
    expect(screen.getByTestId('home-hero-station')).toHaveTextContent('NHK総合')
    expect(screen.getByTestId('home-hero-progress-line')).toBeInTheDocument()
    expect(
      within(screen.getByRole('region', { name: '次に見る 1 本' })).queryByRole('progressbar'),
    ).not.toBeInTheDocument()
    // 「ほかの新着」のサムネイルには重ねない
    expect(screen.getAllByTestId('home-hero-station')).toHaveLength(1)
  })

  it('録画中の主役サムネイルは主ボタンと同じ #chase 付きの詳細へ向ける', async () => {
    stubApi({
      continueWatching: [recording(41, '録画中の続き', 'recording', { resumePositionMs: 330_000 })],
    })
    renderHome('/?mode=watch')

    const primaryAction = await screen.findByTestId('home-primary-action')
    const thumbnail = screen.getByTestId('home-next-watch-thumbnail')
    expect(primaryAction).toHaveAttribute('href', '/recordings/41#chase')
    expect(thumbnail).toHaveAttribute('href', '/recordings/41#chase')
    expect(thumbnail).toHaveAttribute('tabindex', '-1')
    expect(thumbnail).toHaveAttribute('aria-hidden', 'true')
  })

  it('続きからの主役は非カット版を優先して保存位置へシークし、棚は固定画像のままにする', async () => {
    const nonCut = recording(41, '続きの主役', 'finished', {
      resumePositionMs: 330_000,
      encodedAssets: [
        {
          profile: 'cut',
          cut: true,
          keepRanges: [
            { startMs: 0, endMs: 100_000 },
            { startMs: 200_000, endMs: 600_000 },
          ],
          sizeBytes: 200,
        },
        { profile: 'h264', sizeBytes: 100 },
      ],
    })
    const nextArrival = recording(42, '次の新着', 'finished', {
      resumePositionMs: 420_000,
      encodedAssets: [{ profile: 'h264', sizeBytes: 100 }],
    })
    stubApi({ continueWatching: [nonCut, nextArrival] })
    renderHome('/?mode=watch')

    const video = await screen.findByTestId('home-hero-resume-video')
    expect(video).toHaveAttribute('src', '/api/media/recordings/41/file?profile=h264')
    expect(video).toHaveProperty('muted', true)
    expect(video).toHaveProperty('playsInline', true)
    expect(video).toHaveAttribute('preload', 'metadata')
    expect(video).toHaveProperty('controls', false)
    expect(screen.getByTestId('home-hero-thumbnail-image')).toBeInTheDocument()

    let seekTarget = 0
    Object.defineProperty(video, 'duration', { configurable: true, value: 3600 })
    Object.defineProperty(video, 'readyState', { configurable: true, value: HTMLMediaElement.HAVE_METADATA })
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => seekTarget,
      set: (value: number) => { seekTarget = value },
    })
    Object.defineProperty(video, 'requestVideoFrameCallback', { configurable: true, value: () => 1 })
    act(() => fireEvent.loadedMetadata(video))
    expect(seekTarget).toBe(330)

    const arrivals = screen.getByTestId('home-new-arrivals-grid')
    expect(arrivals.querySelector('video')).toBeNull()
  })

  it('カット版だけならその asset の keepRanges で原本の保存位置を写す', async () => {
    const continuation = recording(43, 'カット版のみの主役', 'finished', {
      resumePositionMs: 330_000,
      encodedAssets: [{
        profile: 'cut',
        cut: true,
        keepRanges: [
          { startMs: 0, endMs: 100_000 },
          { startMs: 200_000, endMs: 600_000 },
        ],
        sizeBytes: 100,
      }],
    })
    stubApi({ continueWatching: [continuation] })
    renderHome('/?mode=watch')

    const video = await screen.findByTestId('home-hero-resume-video')
    expect(video).toHaveAttribute('src', '/api/media/recordings/43/file?profile=cut')
    let seekTarget = 0
    Object.defineProperty(video, 'duration', { configurable: true, value: 3600 })
    Object.defineProperty(video, 'readyState', { configurable: true, value: HTMLMediaElement.HAVE_METADATA })
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => seekTarget,
      set: (value: number) => { seekTarget = value },
    })
    Object.defineProperty(video, 'requestVideoFrameCallback', { configurable: true, value: () => 1 })
    act(() => fireEvent.loadedMetadata(video))
    expect(seekTarget).toBe(230)
  })

  it.each([
    {
      label: '新着の主役',
      continueWatching: [] as Recording[],
      finished: [recording(44, '新着', 'finished', {
        resumePositionMs: 60_000,
        sizeBytes: 100,
        encodedAssets: [{ profile: 'h264', sizeBytes: 100 }],
      })],
    },
    {
      label: '録画中で原本だけの主役',
      continueWatching: [recording(45, '録画中の原本のみ', 'recording', { resumePositionMs: 60_000, encodedAssets: [] })],
      finished: [] as Recording[],
    },
    {
      label: '保存位置が無い主役',
      continueWatching: [recording(46, '保存位置無し', 'finished', { encodedAssets: [{ profile: 'h264', sizeBytes: 100 }] })],
      finished: [] as Recording[],
    },
  ])('$label は video を置かず固定画像を使う', async ({ continueWatching, finished }) => {
    stubApi({ continueWatching, finished })
    renderHome('/?mode=watch')

    expect(await screen.findByTestId('home-hero-thumbnail-image')).toBeInTheDocument()
    expect(screen.queryByTestId('home-hero-resume-video')).not.toBeInTheDocument()
  })

  it('管理側は時間軸と要対応を表示し、見る側では出さない', async () => {
    stubApi({
      recording: [recording(1, '録画中', 'recording')],
      continueWatching: [recording(2, '続き', 'finished', { resumePositionMs: 1 })],
      reservations: [reservation(3, '予約', 2 * HOUR)],
      breakers: [breaker()],
      finished: [recording(4, '完了', 'finished')],
    })
    const opsHome = renderHome('/?mode=ops')
    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    opsHome.unmount()

    stubApi({ recording: [recording(5, '録画中', 'recording')] })
    const watchHome = renderHome('/?mode=watch')
    expect(await screen.findByRole('region', { name: '録画中' })).toBeInTheDocument()
    for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
      expect(screen.queryByRole('heading', { name: heading })).not.toBeInTheDocument()
    }
    watchHome.unmount()
  })

  it('警告バッジは材料が未解決 / 0 件なら出ず、警告行数と一致する', async () => {
    // breakers だけ未解決。超過・失敗・ドロップで警告材料は既にあるので、
    // pending ガードが無ければ件数 > 0 のバッジが出てしまう。
    const api = stubApi({
      pendingPaths: new Set(['/api/breakers']),
      breakers: [breaker()],
      overages: [overage(-HOUR, HOUR)],
      finished: [recording(6, 'drop', 'finished', { dropSummary: { packets: 10, drops: 1, errors: 0, scrambled: 0 } })],
      failed: [recording(7, 'failure', 'failed')],
    })
    const pendingHome = renderHome('/?mode=ops')
    const toggle = await screen.findByTestId('home-mode-toggle')
    await waitFor(() => expect(api.unresolvedCount('/api/breakers')).toBe(1))
    await screen.findAllByText('drop')
    expect(within(toggle).queryByTestId('home-warning-count')).not.toBeInTheDocument()
    await act(async () => api.resolvePending())
    const badge = await within(toggle).findByTestId('home-warning-count')
    const warningSection = await screen.findByRole('heading', { name: '要対応' })
    const rows = within(warningSection.closest('section')!).getAllByRole('listitem')
    expect(badge).toHaveTextContent(String(rows.length))
    pendingHome.unmount()

    // 材料が 0 件ならバッジは出ない
    stubApi({})
    renderHome('/?mode=watch')
    await screen.findByTestId('home-mode-toggle')
    await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument())
    expect(screen.queryByTestId('home-warning-count')).not.toBeInTheDocument()
  })

  it('警告材料の取得失敗は既存規則どおり警告なしに縮退し、帯もバッジも出さない', async () => {
    stubApi({ errorPaths: new Set(['/api/breakers', '/api/capacity/overages']) })
    renderHome('/?mode=watch')
    const toggle = await screen.findByTestId('home-mode-toggle')
    await waitFor(() => {
      expect(toggle).toBeInTheDocument()
      expect(screen.queryByTestId('home-warning-count')).not.toBeInTheDocument()
    })
    expect(screen.queryByTestId('home-watch-breaker-band')).not.toBeInTheDocument()
  })

  it('見る側の帯はブレーカーだけで出し、容量超過や失敗だけでは出さない', async () => {
    stubApi({ overages: [overage(-HOUR, HOUR)], failed: [recording(8, '失敗', 'failed')] })
    const cleanWatchHome = renderHome('/?mode=watch')
    const toggle = await screen.findByTestId('home-mode-toggle')
    await waitFor(() => expect(within(toggle).getByTestId('home-warning-count')).toBeInTheDocument())
    expect(screen.queryByTestId('home-watch-breaker-band')).not.toBeInTheDocument()
    cleanWatchHome.unmount()

    stubApi({ breakers: [breaker()] })
    renderHome('/?mode=watch')
    expect(await screen.findByTestId('home-watch-breaker-band')).toHaveTextContent('停止中')
    expect(screen.getByRole('link', { name: '管理で見る' })).toHaveAttribute('href', '/?mode=ops')
  })
})

describe('ホーム: CM 検出失敗が要対応に出る（issue #1101）', () => {
  it('局で直せる失敗は段階を問わず局ごとにまとめ、CM ロゴ画面へ局を渡す', async () => {
    const { fetchMock } = stubApi({
      finished: [
        recording(21, '失敗した番組 1', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'logo', error: 'technical detail' },
        }),
        recording(22, '失敗した番組 2', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'area' },
        }),
        recording(23, '採用待ちの番組', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'adopt' },
        }),
        recording(24, 'ロゴが変わった番組', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'match' },
        }),
        recording(25, '別解像度の番組', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'resolution' },
        }),
        recording(26, '処理系の失敗の番組', 'finished', {
          networkId: 32678,
          serviceId: 5168,
          cmDetection: { state: 'failed', stage: 'setup' },
        }),
      ],
    })
    renderHome()

    const section = (await screen.findByRole('heading', { name: '要対応' })).closest('section')!
    const cmRows = within(section).getAllByRole('listitem').filter(
      (row) => row.getAttribute('data-warning-kind') === 'cm-detection',
    )
    // setup は録画で直す段階なので、同じ局でも局の行に混ぜず録画ごとの行に残る。
    expect(cmRows).toHaveLength(2)
    const stationRow = cmRows.find((row) => row.querySelector('a')?.getAttribute('href') === '/cm-logos/32678/5168')!
    expect(within(stationRow).getByTestId('warning-chip')).toHaveTextContent('CM 検出失敗')
    expect(within(stationRow).getByTestId('warning-title')).toHaveTextContent('NHK総合 5 件')
    const link = within(stationRow).getByRole('link')
    expect(link).toHaveAttribute('href', '/cm-logos/32678/5168')
    expect(link).not.toHaveAttribute('href', expect.stringContaining('recording='))
    expect(section).not.toHaveTextContent('technical detail')

    // limit 等の条件に関わらず、完了録画の取得（時間軸の窓 `from` 付きを除く）が 1 本だけであること（CM 検出専用の取得を足さない）。
    const finishedScans = fetchMock.mock.calls
      .map(([input]) => new URL(String(input), 'http://localhost'))
      .filter((url) => url.pathname === '/api/recordings' && url.searchParams.get('status') === 'finished' && !url.searchParams.has('from'))
    expect(finishedScans).toHaveLength(1)
  })

  it('logo / area 以外の失敗は録画ごとに並び、detecting と disabled は除く', async () => {
    stubApi({
      finished: [
        recording(23, '解析失敗の番組', 'finished', {
          cmDetection: { state: 'failed', stage: 'parse' },
        }),
        recording(24, '段階不明の番組', 'finished', {
          cmDetection: { state: 'failed', stage: null },
        }),
        recording(25, '検出中の番組', 'finished', {
          cmDetection: { state: 'detecting', stage: 'setup' },
        }),
        recording(26, '無効な番組', 'finished', { cmDetection: { state: 'disabled' } }),
      ],
    })
    renderHome()

    const section = (await screen.findByRole('heading', { name: '要対応' })).closest('section')!
    const cmRows = within(section).getAllByRole('listitem').filter(
      (row) => row.getAttribute('data-warning-kind') === 'cm-detection',
    )
    expect(cmRows).toHaveLength(2)
    expect(within(cmRows[0]!).getByTestId('warning-title')).toHaveTextContent('解析失敗の番組')
    expect(within(cmRows[0]!).getByRole('link')).toHaveAttribute('href', '/recordings/23')
    // 副行は開始日時と局名だけ。「1 件」は録画ごとの行では冗長。
    expect(cmRows[0]).not.toHaveTextContent('1 件')
    expect(within(cmRows[1]!).getByTestId('warning-title')).toHaveTextContent('段階不明の番組')
    expect(within(cmRows[1]!).getByRole('link')).toHaveAttribute('href', '/recordings/24')
    expect(section).not.toHaveTextContent('検出中の番組')
    expect(section).not.toHaveTextContent('無効な番組')
  })

  it('見るモードの警告バッジにも、局まとめと録画ごとの CM 失敗を数える', async () => {
    stubApi({
      finished: [
        recording(27, '局まとめ 1', 'finished', {
          cmDetection: { state: 'failed', stage: 'logo' },
        }),
        recording(28, '局まとめ 2', 'finished', {
          cmDetection: { state: 'failed', stage: 'adopt' },
        }),
        recording(29, '個別の失敗', 'finished', {
          cmDetection: { state: 'failed', stage: 'parse' },
        }),
      ],
    })
    renderHome('/?mode=watch')

    const toggle = await screen.findByTestId('home-mode-toggle')
    expect(await within(toggle).findByTestId('home-warning-count')).toHaveTextContent('2')
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()
  })
})

describe('ホーム: 全セクションが空のときの単一の空状態', () => {
  it('時間軸と要対応が空なら単一の空状態だけを出す', async () => {
    stubApi({})
    renderHome()

    expect(await screen.findByText('表示できる項目がありません')).toBeInTheDocument()
    for (const heading of ['今日 0 時 → 明日の終わり', '要対応']) {
      expect(screen.queryByRole('heading', { name: heading })).not.toBeInTheDocument()
    }
    // 「異常なし」「予約がありません」のような肯定/報告の文言を書いていない
    expect(screen.queryByText(/異常/)).not.toBeInTheDocument()
  })

  it('予約が時間軸にあれば単一の空状態は出ない', async () => {
    stubApi({ reservations: [reservation(1, '予約の番組', HOUR)] })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()
  })
})

describe('ホーム管理モード: 時間軸から詳細へ移る', () => {
  it('録画ブロックは非対話のまま、詳細一覧に 24px 以上の導線を置く', async () => {
    stubApi({ finished: [recording(9, '完了した番組', 'finished')] })
    renderHome()

    const block = await screen.findByTestId('home-timeline-block')
    expect(block).toHaveAttribute('data-kind', 'finished')
    expect(block.closest('a')).toBeNull()
    const details = await openTimelineDetails()
    const link = within(details).getByRole('link', { name: /完了した番組/ })
    expect(link).toHaveAttribute('href', '/recordings/9')
    expect(link.className).toContain('min-h-6')
  })

  it('時間軸用の完了録画取得に失敗したときはエラーを出す', async () => {
    stubApi({ errorPaths: new Set(['/api/recordings']) })
    renderHome()

    expect(await screen.findByText('時間軸の取得に失敗しました')).toBeInTheDocument()
  })
})

describe('ホーム: 続きから', () => {
  it('再開対象を表示し、録画中の行は追っかけページに向ける', async () => {
    stubApi({
      continueWatching: [
        recording(31, '再開する録画', 'finished', { resumePositionMs: 12_000 }),
        recording(32, '録画中の再開対象', 'recording', { resumePositionMs: 24_000 }),
      ],
    })
    renderHome('/?mode=watch')

    expect(await screen.findByRole('heading', { name: '再開する録画' })).toBeInTheDocument()
    expect(screen.getByTestId('home-primary-action')).toHaveAttribute('href', '/recordings/31')
    expect(screen.getByRole('link', { name: '最初から' }).getAttribute('href')).toContain('fromBeginning=true')
  })

  it('取得失敗を空として隠さない', async () => {
    stubApi({ errorPaths: new Set(['/api/recordings/continue-watching']) })
    renderHome('/?mode=watch')
    expect(await screen.findByText('次に見る録画の取得に失敗しました')).toBeInTheDocument()
  })
})

describe('ホーム: 0 件のセクションは文言も出さず消える', () => {
  it('予約があれば時間軸を出し、録画中が 0 件でも空の状態文言を足さない', async () => {
    stubApi({ reservations: [reservation(1, '今夜の予約', 2 * HOUR)] })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(screen.queryByText('録画中の番組がありません')).not.toBeInTheDocument()
  })
})

describe('ホーム管理モード: 時間軸の窓', () => {
  it('今日 0 時から明後日 0 時までの予約を表示する', async () => {
    stubApi({
      reservations: [
        reservation(1, '0 時より前', -21 * HOUR),
        reservation(2, '窓に入る予約', 3 * HOUR),
        reservation(3, '窓の終端以降', 28 * HOUR),
      ],
    })
    renderHome()

    const details = await openTimelineDetails()
    expect(within(details).getByRole('link', { name: /窓に入る予約/ })).toBeInTheDocument()
    expect(screen.queryByText('0 時より前')).not.toBeInTheDocument()
    expect(screen.queryByText('窓の終端以降')).not.toBeInTheDocument()
  })

  it('時間軸の詳細一覧は表示件数で打ち切らない', async () => {
    const reservations = Array.from({ length: 10 }, (_, i) =>
      reservation(i + 1, `予約 ${i + 1}`, (i + 1) * HOUR),
    )
    stubApi({ reservations })
    renderHome()

    const details = await openTimelineDetails()
    expect(within(details).getByRole('link', { name: /予約 1(?!\d)/ })).toBeInTheDocument()
    expect(within(details).getByRole('link', { name: /予約 10/ })).toBeInTheDocument()
  })
})

describe('ホーム管理モード: site × channelType の行', () => {
  it('サイト別・種別別に行を分ける', async () => {
    stubApi({
      reservations: [
        reservation(1, '同じ番組名', 2 * HOUR, { serviceName: 'NHK総合' }),
        reservation(2, '別サイトの番組', 4 * HOUR, { site: 'sub', channelType: 'BS', serviceName: 'NHK Eテレ' }),
      ],
    })
    renderHome()

    const labels = await screen.findAllByTestId('home-timeline-row-label')
    expect(labels.map((label) => label.textContent)).toEqual(['default · 地デジ', 'sub · BS'])
  })
})

describe('ホーム管理モード: 失敗/ドロップの timeline rendering', () => {
  it('drop block は destructive の下端線を持ち、drop数は要対応にだけ表示する', async () => {
    stubApi({
      finished: [
        recording(9, 'ドロップのある録画', 'finished', {
          dropSummary: { packets: 1000, drops: 12, errors: 0, scrambled: 3 },
        }),
      ],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    // 警告セクションのテキストとしては出るが、行自体には drop バッジ（DropBadges
    // 由来の「ドロップ」ラベル）を重ねない
    expect(within(await findWarningRow('ドロップのある録画')).getByText('ドロップ 12 / スクランブル 3')).toBeInTheDocument()
    const details = await openTimelineDetails()
    const link = within(details).getByRole('link', { name: /ドロップのある録画/ })
    const block = screen.getByTestId('home-timeline-block')
    expect(block.className).toContain('shadow-[inset_0_-3px_0_var(--destructive)]')
    expect(link.textContent).not.toMatch(/ドロップ 12/)
  })
})

describe('ホーム: 警告セクション', () => {
  it('orphaned 予約を「録画されず」で詳細へ案内し、管理の警告件数に加える', async () => {
    stubApi({
      reservations: [reservation(22, '開始されなかった番組', -3 * HOUR, { state: 'orphaned' })],
    })
    renderHome()

    const row = await findWarningRow('開始されなかった番組')
    expect(within(row).getByText('録画されず')).toBeInTheDocument()
    expect(within(row).getByRole('link')).toHaveAttribute('href', '/reservations/default/220')
    expect(screen.getByTestId('home-warning-count')).toHaveTextContent('1')
  })

  it('予約一覧の取得が未解決なら要対応と管理の警告件数を確定しない', async () => {
    const { resolvePending, unresolvedCount } = stubApi({
      reservations: [reservation(23, '遅れて届く録画されず', -3 * HOUR, { state: 'orphaned' })],
      pendingPaths: new Set(['/api/reservations']),
    })
    renderHome()

    await waitFor(() => expect(unresolvedCount('/api/reservations')).toBe(1))
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()
    expect(screen.queryByTestId('home-warning-count')).not.toBeInTheDocument()

    resolvePending()

    expect(await findWarningRow('遅れて届く録画されず')).toBeInTheDocument()
    expect(screen.getByTestId('home-warning-count')).toHaveTextContent('1')
  })

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

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除/)).toBeInTheDocument()
    expect(screen.getByText(/BSが 1 本不足しています/)).toBeInTheDocument()
    expect(within(await findWarningRow('ドロップのある録画')).getByText('ドロップ 12 / スクランブル 3')).toBeInTheDocument()
  })

  it('チューナー不足の項目は番組表のその時間帯への導線を持つ', async () => {
    const shortage = overage(2 * HOUR, 3 * HOUR)
    stubApi({ overages: [shortage] })
    renderHome()

    const link = await screen.findByRole('link', { name: /BSが 1 本不足しています/ })
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
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()
    expect(screen.queryByText(/BSが 1 本不足しています/)).not.toBeInTheDocument()
  })

  it('時境界より前に始まり進行中の超過区間は警告に出す（回収が広すぎない）', async () => {
    vi.setSystemTime(halfPastNowMs)
    // 18:00〜21:00 = 量子化した `start`（20:00）より前に始まっているが進行中。
    // 量子化の回収が「開始時刻」を見る実装になっていると、これを取り落とす。
    stubApi({ overages: [overage(-2 * HOUR, HOUR)] })
    renderHome()

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(screen.getByText(/BSが 1 本不足しています/)).toBeInTheDocument()
  })

  it('直近完了にドロップが無く、ブレーカー・チューナー不足も無ければ警告は出ない（両方向）', async () => {
    stubApi({
      finished: [recording(9, 'きれいな録画', 'finished')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()
  })

  it('警告項目は種別ごとに固定の色クラスを持つ（チューナー不足=warning、失われた録画/失敗録画/ドロップ=destructive）', async () => {
    // jsdom は実描画色を計算しないので、当たっているクラスだけを見る
    // （実画素は e2e/design.mjs が見る。`pages/reservations.test.tsx` の
    // 「警告の信号色」と同じ流儀）。文字列 key の前方一致で種別を推測する
    // 実装は、key の書式を変えただけで色が黙って壊れる（レビュー指摘）ので、
    // `WarningItem.kind` を経由していることをここで固定する。
    //
    // 失敗録画と未開始の録画も同じ契約に入れる（レビュー指摘: `WarningKind` に
    // 'failed' を
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
        recording(11, 'CM 検出失敗の録画', 'finished', {
          cmDetection: { state: 'failed', stage: 'parse' },
        }),
      ],
      failed: [recording(10, '失敗した録画', 'failed')],
      reservations: [reservation(12, '開始されなかった録画', -3 * HOUR, { state: 'orphaned' })],
    })
    renderHome()

    const overageRow = (await screen.findByText(/BSが 1 本不足しています/)).closest('li')
    const breakerRow = await findWarningRow('ルール評価による予約の削除')
    const dropRow = await findWarningRow('ドロップのある録画')
    const failedRow = await findWarningRow('失敗した録画')
    const notRecordedRow = await findWarningRow('開始されなかった録画')
    const cmDetectionRow = await findWarningRow('CM 検出失敗の録画')
    const warningSection = screen.getByRole('heading', { name: '要対応' }).closest('section')!
    expect(
      within(warningSection)
        .getAllByRole('listitem')
        .map((row) => row.getAttribute('data-warning-kind')),
    ).toEqual(['breaker', 'failed', 'not-recorded', 'overage', 'drop', 'cm-detection'])

    // 色は種別チップだけが持つ（`WarningRow` の実装どおり）。
    const chipOf = (row: HTMLElement | null) => within(row!).getByTestId('warning-chip')
    expect(chipOf(overageRow).className).toMatch(/bg-warning\/15/)
    expect(chipOf(overageRow).className).toMatch(/text-warning/)
    for (const row of [breakerRow, dropRow, failedRow, notRecordedRow, cmDetectionRow]) {
      const el = chipOf(row)
      expect(el.className).toMatch(/text-destructive/)
      expect(el.className).not.toMatch(/bg-warning/)
    }
  })
})

describe('ホーム: 失敗録画が警告に出る（issue #301）', () => {
  it('失敗録画があれば警告セクションに出る', async () => {
    stubApi({
      failed: [recording(9, '失敗した番組', 'failed')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(within(await findWarningRow('失敗した番組')).getByText('録画失敗')).toBeInTheDocument()
  })

  it('失敗録画が無ければ警告に出ない（両方向）', async () => {
    stubApi({
      finished: [recording(9, 'きれいな録画', 'finished')],
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()
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
    expect((await findWarningRow('直後に切れた録画')).textContent).toMatch(/実際 0分・予定 5分/)
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

    expect((await findWarningRow('開始が観測されていない録画')).textContent).toMatch(/予定 5分・未開始/)
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

    const row = await findWarningRow('終了未記録の録画')
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

    // 「理由不明」は理由そのものが言えていないので「理由:」のラベルを付けない
    // （二重表現「理由: 理由不明」にしない）。
    expect(await screen.findByText(/理由不明/)).toBeInTheDocument()
    expect(screen.queryByText(/理由: 理由不明/)).not.toBeInTheDocument()
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

    const row = await findWarningRow('空の理由の失敗')
    expect(row.textContent).toMatch(/理由不明/)
    expect(row.textContent).not.toMatch(/理由: 理由不明/)
    expect(row.textContent).not.toMatch(/\{/)
  })

  it('失敗録画は録画単体ページへの導線を持つ', async () => {
    stubApi({ failed: [recording(9, '失敗した番組', 'failed')] })
    renderHome()

    const links = await screen.findAllByRole('link', { name: /失敗した番組/ })
    expect(links.some((link) => link.getAttribute('href') === '/recordings/9')).toBe(true)
  })

  it('recency 窓の外にある古い失敗は警告に出ない（issue の受け入れ基準「直近の」失敗録画）', async () => {
    stubApi({
      failed: [
        recording(9, '古い失敗', 'failed', { startAt: iso(-30 * 24 * HOUR) }),
        recording(10, '直近の失敗', 'failed', { startAt: iso(-HOUR) }),
      ],
    })
    renderHome()

    await findWarningRow('直近の失敗')
    expect(screen.queryByText('古い失敗', { selector: '[data-testid="warning-title"]' })).not.toBeInTheDocument()
  })
})

describe('ホーム: 警告の検出範囲は時間軸の窓から独立している', () => {
  it('今日 0 時より前の finished drop も 20 件の警告 scan から拾う', async () => {
    const finished = Array.from({ length: 7 }, (_, i) =>
      recording(i + 1, `録画 ${i + 1}`, 'finished', {
        startAt: iso(i === 6 ? -21 * HOUR : -(i + 1) * HOUR),
      }),
    )
    finished[6] = {
      ...finished[6]!,
      dropSummary: { packets: 100, drops: 5, errors: 0, scrambled: 0 },
    }

    stubApi({ finished })
    renderHome()

    expect(await screen.findByTestId('home-ops-timeline-frame')).toBeInTheDocument()
    // 時間軸には出ないが、warning scan は timeline の窓と独立している。
    expect([...screen.queryAllByTestId('home-timeline-block')].some((block) => block.getAttribute('title') === '録画 7')).toBe(false)
    expect(within(await findWarningRow('録画 7')).getByText('ドロップ 5')).toBeInTheDocument()
  })

  it('失敗ブロックは status=failed の応答から作り、superseded 行を除く', async () => {
    const realFailed = recording(30, '現在の失敗', 'failed', { startAt: iso(-2 * HOUR) })
    const superseded = Object.assign(
      recording(31, '置き換え済み擬似失敗', 'failed', { startAt: iso(-2 * HOUR) }),
      { supersededAt: iso(-HOUR) },
    ) as Recording
    const { fetchMock } = stubApi({ failed: [realFailed, superseded] })
    renderHome()

    expect(await screen.findByTestId('home-timeline-block')).toHaveAttribute('data-kind', 'failed')
    expect(screen.queryByText('置き換え済み擬似失敗')).not.toBeInTheDocument()
    const calls = fetchMock.mock.calls
      .map(([input]) => new URL(String(input), 'http://localhost'))
      .filter((url) => url.pathname === '/api/recordings')
    expect(calls.every((url) => url.searchParams.has('status'))).toBe(true)
    const timelineCalls = calls.filter((url) => url.searchParams.get('limit') === '200')
    expect(timelineCalls.map((url) => url.searchParams.get('status')).sort()).toEqual([
      'failed', 'finished', 'recording',
    ])
    // 完了・失敗は開始時刻の範囲で絞る。録画中は開始時刻で絞らない（日またぎを落とさない）。
    expect(timelineCalls.map((url) =>
      `${url.searchParams.get('status')}:${url.searchParams.has('from')}:${url.searchParams.has('to')}`,
    ).sort()).toEqual(['failed:true:true', 'finished:true:true', 'recording:false:false'])
  })

  it('overage警告は同site・時間重複予約だけを補足し、敗者や容量保証を示さない', async () => {
    stubApi({
      overages: [{ ...overage(HOUR, 2 * HOUR), jammedTypes: ['GR', 'BS'] }],
      reservations: [
        reservation(1, '重なる予約', HOUR),
        reservation(2, '別siteの予約', HOUR, { site: 'sub' }),
        reservation(3, 'skipされた予約', HOUR, { skip: true }),
      ],
    })
    renderHome()

    const row = await screen.findByText(/地デジ・BSが 1 本不足しています/)
    const item = row.closest('li')!
    expect(within(item).getByText('この時間帯の予約: 重なる予約')).toBeInTheDocument()
    expect(item).not.toHaveTextContent(/別siteの予約|skipされた予約/)
    expect(item).not.toHaveTextContent(/重なる予約.*(?:失敗|録れない|除外)/)
    expect(item).not.toHaveTextContent(/十分|余裕|収まる/)
  })

  it('詰まっていない種別の予約は副行に並べない（GR だけの超過に重なる BS の予約）', async () => {
    stubApi({
      overages: [{ ...overage(HOUR, 2 * HOUR), jammedTypes: ['GR'] }],
      reservations: [
        reservation(1, '地デジの予約', HOUR),
        reservation(2, 'BSの予約', HOUR, { channelType: 'BS' }),
      ],
    })
    renderHome()

    const item = (await screen.findByText(/地デジが 1 本不足しています/)).closest('li')!
    expect(within(item).getByText('この時間帯の予約: 地デジの予約')).toBeInTheDocument()
    expect(item).not.toHaveTextContent('BSの予約')
  })

  it('容量超過の警告は共有の暦日差で「明日」を決め、範囲を「〜」でつなぐ', async () => {
    stubApi({ overages: [overage(5 * HOUR, 6 * HOUR)] })
    renderHome()

    expect(await screen.findByText('明日 01:00〜02:00 BSが 1 本不足しています')).toBeInTheDocument()
  })

  it('timeline窓外でも7日内の failed は警告し、時間軸のブロックにはしない', async () => {
    const outsideWindow = recording(40, '窓外だが直近の失敗', 'failed', {
      startAt: iso(-26 * HOUR),
    })
    stubApi({ failed: [outsideWindow] })
    renderHome()

    expect(within(await findWarningRow('窓外だが直近の失敗')).getByText('録画失敗')).toBeInTheDocument()
    expect(
      screen.queryAllByTestId('home-timeline-block').some(
        (block) => block.getAttribute('title') === '窓外だが直近の失敗',
      ),
    ).toBe(false)
  })
})

describe('ホーム: 取得失敗はセクションを隠さずエラー表示にする', () => {
  it('見る側の録画中取得が失敗しても、取得失敗を空件数として扱わない', async () => {
    stubApi({ errorPaths: new Set(['/api/recordings']) })
    renderHome('/?mode=watch')

    expect(await screen.findByRole('heading', { name: 'ホーム' })).toBeInTheDocument()
    expect(screen.getByText('録画中の取得に失敗しました')).toBeInTheDocument()
  })
})

describe('ホーム管理モード: 時間軸の読み込み', () => {
  it('予約取得が未解決なら timeline rows を描かず、解決後に表示する', async () => {
    const { resolvePending } = stubApi({
      recording: [recording(1, '録画中の番組', 'recording')],
      reservations: [reservation(2, '今夜の予約', 2 * HOUR)],
      pendingPaths: new Set(['/api/reservations']),
    })
    renderHome()

    // 見出しは出るが、timeline data が揃うまでは chart/frame を出さない。
    expect(await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
    expect(screen.queryByTestId('home-ops-timeline-frame')).not.toBeInTheDocument()
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByTestId('home-ops-timeline-frame')).toBeInTheDocument()
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
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除/)).toBeInTheDocument()
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
    expect(unresolvedCount('/api/recordings?status=failed')).toBe(2)
    expect(screen.queryByRole('heading', { name: '要対応' })).not.toBeInTheDocument()

    resolvePending()

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除/)).toBeInTheDocument()
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
    expect(unresolvedCount('/api/recordings?status=failed')).toBe(2)
    expect(screen.queryByText('表示できる項目がありません')).not.toBeInTheDocument()

    resolvePending()

    // 解決したら警告として出る（「たまたま速すぎて見えなかった」の排除）
    expect(within(await findWarningRow('後から届いた失敗')).getByText('録画失敗')).toBeInTheDocument()
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
    expect(screen.queryByTestId('home-ops-timeline-frame')).not.toBeInTheDocument()

    resolvePending()

    await waitFor(() =>
      expect(screen.getByText('表示できる項目がありません')).toBeInTheDocument(),
    )
  })
})

/**
 * must-fix（レビュー）: 容量超過クエリの `start` に生の `Date.now()` を渡すと、
 * レンダーごとにキャッシュキーが変わり無限に再取得し続ける（実測: stub 即答で
 * 4 秒に 37 回、実サーバー相当の遅延では
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
      // 再レンダーの引き金。警告材料の予約一覧は即答させておき、時間軸の録画 query
      // を解決した瞬間に新しい `Date.now()` でレンダーして容量キーを進める。
      pendingRecordingStatuses: new Set(['recording']),
      // 2 回目（= 新しいキー）の容量超過だけ未解決にする。即答させると
      // 「消えた一瞬」が assert より先に終わってしまい、壊れていても緑になる。
      pendingAfterFirstCall: new Set(['/api/capacity/overages']),
    })
    renderHome()

    expect(await screen.findByRole('heading', { name: '要対応' })).toBeInTheDocument()

    // 時境界を越える
    vi.setSystemTime(nowMs + 500)
    resolvePending()

    // 予約 query は警告を出す前に解決済み。時間軸の詳細リンクも取得できることを確認する。
    const details = await openTimelineDetails()
    expect(await within(details).findByRole('link', { name: /今夜の予約/ })).toBeInTheDocument()

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
    expect(screen.getByRole('heading', { name: '要対応' })).toBeInTheDocument()
    expect(screen.getByText(/ルール評価による予約の削除/)).toBeInTheDocument()
  })
})

describe('ホーム管理モード: 窓は常に今日 0 時から（午前に開いても）', () => {
  it('午前 9 時に開いても「いま」の線は 9 時の位置にあり、午前の録画・録画中も窓に入る', async () => {
    const morning = dayStart.getTime() + 9 * HOUR
    vi.setSystemTime(morning)
    const at = (hours: number) => new Date(dayStart.getTime() + hours * HOUR).toISOString()
    stubApi({
      finished: [recording(1, '朝の完了', 'finished', { startAt: at(7), durationMs: HOUR })],
      recording: [recording(2, '朝の録画中', 'recording', { startAt: at(8.75), durationMs: HOUR })],
    })
    renderHome()

    const titles = (await screen.findAllByTestId('home-timeline-block')).map((block) =>
      block.getAttribute('title'),
    )
    expect(titles.sort()).toEqual(['朝の完了', '朝の録画中'])
    // jsdom の window.innerWidth は 1024（desktop の 64px/h）。9 時 = 9 * 64px。
    expect(screen.getByTestId('home-timeline-now')).toHaveStyle({ left: '576px' })
    expect(screen.getByRole('heading', { name: '今日 0 時 → 明日の終わり' })).toBeInTheDocument()
  })
})

describe('ホーム管理モード: 0 時をまたぐ録画は窓の左端で切って描く', () => {
  it('0 時をまたぐ録画は、録画中・録れた・失敗とも 0:20 に開いた時間軸に出て、窓の前に終わったものは出ない', async () => {
    // 翌日 00:20 に開く。窓の始点はその日の 0:00、前日 23:40 開始の 140 分の録画が録画中。
    const tomorrow = new Date(dayStart)
    tomorrow.setDate(tomorrow.getDate() + 1)
    const after = tomorrow.getTime() + 20 * 60_000
    vi.setSystemTime(after)
    const at = (minutes: number) => new Date(tomorrow.getTime() + minutes * 60_000).toISOString()
    stubApi({
      recording: [recording(1, '日またぎの映画', 'recording', { startAt: at(-20), durationMs: 140 * 60_000 })],
      finished: [
        recording(2, '日またぎの完了', 'finished', { startAt: at(-30), durationMs: 40 * 60_000 }),
        recording(3, '前日に終わった完了', 'finished', { startAt: at(-90), durationMs: 60 * 60_000 }),
      ],
      failed: [recording(4, '日またぎの失敗', 'failed', { startAt: at(-10), durationMs: 20 * 60_000 })],
    })
    renderHome()

    const blocks = await screen.findAllByTestId('home-timeline-block')
    expect(blocks.map((block) => `${block.dataset.kind}:${block.getAttribute('title')}`).sort()).toEqual([
      'failed:日またぎの失敗',
      'finished:日またぎの完了',
      'recording:日またぎの映画',
    ])
    // 左端で切る: 開始は窓の始点（0px）、幅は 0:00〜2:00 の 2 時間（jsdom は 64px/h）。
    const movie = blocks.find((block) => block.getAttribute('title') === '日またぎの映画')!
    expect(movie).toHaveStyle({ left: '0px', width: '128px' })
    // 窓の前に終わった録画は詳細一覧にも出さない（幅 0 のブロックは描かれないので一覧で見る）。
    const details = await openTimelineDetails()
    expect(within(details).getAllByTestId('home-timeline-detail-row').map((row) => row.textContent)).toEqual([
      '7/25 23:30地デジ日またぎの完了録れた',
      '7/25 23:40地デジ日またぎの映画録画中',
      '7/25 23:50地デジ日またぎの失敗失敗',
    ])
  })
})

describe('ホーム管理モード: 容量超過は個々の予約ブロックに印を付けない', () => {
  it('超過区間に重なる予約ブロックは、区間の外の予約ブロックと位置・題名以外が同じ', async () => {
    stubApi({
      overages: [{ ...overage(2 * HOUR, 3 * HOUR), jammedTypes: ['GR'] }],
      reservations: [
        reservation(1, '区間の中', 2 * HOUR + 10 * 60_000, { durationMs: 30 * 60_000 }),
        reservation(2, '区間の外', 5 * HOUR, { durationMs: 30 * 60_000 }),
      ],
    })
    renderHome()

    const blocks = await screen.findAllByTestId('home-timeline-block')
    const inside = blocks.find((block) => block.getAttribute('title') === '区間の中')!
    const outside = blocks.find((block) => block.getAttribute('title') === '区間の外')!
    // 印はクラスだけでなく子要素・data-*・style でも付けられるので、ブロックの
    // 属性と子要素の全体を比べる。違ってよいのは位置（left）と題名だけ。
    const signature = (block: HTMLElement) => {
      const title = block.getAttribute('title')!
      const clone = block.cloneNode(true) as HTMLElement
      clone.removeAttribute('title')
      clone.style.removeProperty('left')
      clone.innerHTML = clone.innerHTML.replaceAll(title, '{title}')
      return clone.outerHTML
    }
    expect(signature(inside)).toBe(signature(outside))
    expect(inside.className).not.toMatch(/warning/)
  })
})

describe('ホーム管理モード: 時間軸の見出しと凡例', () => {
  it('時間軸の行が無く要対応だけあるときは、見出しの右側に種別を出さない', async () => {
    stubApi({ breakers: [breaker()] })
    renderHome()

    const heading = await screen.findByRole('heading', { name: '今日 0 時 → 明日の終わり' })
    await screen.findByRole('heading', { name: '要対応' })
    expect(heading.parentElement).toHaveTextContent(/^今日 0 時 → 明日の終わり$/)
  })

  it('窓・行の種別を見出しに、スクロールの案内とチューナー不足の区間を凡例に出す', async () => {
    stubApi({
      reservations: [
        reservation(1, 'GR 番組', 2 * HOUR),
        reservation(2, 'BS 番組', 2 * HOUR, { channelType: 'BS' }),
      ],
    })
    renderHome()

    await screen.findAllByTestId('home-timeline-block')
    const section = screen.getByTestId('home-ops-timeline')
    expect(within(section).getByRole('heading')).toHaveTextContent('今日 0 時 → 明日の終わり')
    expect(section).toHaveTextContent('地デジ / BS ごと')
    expect(section).toHaveTextContent('チューナー不足の区間')
    expect(section).toHaveTextContent('枠の中を横にスクロールできます')
    expect(section).not.toHaveTextContent('容量不足')
  })

  it('目盛りは日の境界が読める（翌日は「翌」を付ける）', async () => {
    stubApi({ reservations: [reservation(1, '番組', 2 * HOUR)] })
    renderHome()

    await screen.findAllByTestId('home-timeline-block')
    const labels = screen.getAllByTestId('home-timeline-tick').map((tick) => tick.textContent)
    expect(labels).toContain('18時')
    expect(labels).toContain('翌0時')
    expect(labels).toContain('翌3時')
    expect(labels.filter((label) => label === '0時')).toHaveLength(1)
  })
})

describe('ホーム管理モード: 詳細一覧は時刻順で、日・種別・状態を出す', () => {
  it('状態ごとの連結ではなく開始時刻順に並べ、日・種別 / site・状態を各行に出す', async () => {
    stubApi({
      reservations: [
        reservation(1, '明日の予約', 13 * HOUR, { channelType: 'BS' }), // 明日 09:00
        reservation(2, '今夜の予約', 2 * HOUR), // 22:00
      ],
      finished: [recording(3, '昼の完了', 'finished', { startAt: iso(-8 * HOUR) })], // 12:00
      failed: [recording(4, '夕方の失敗', 'failed', { startAt: iso(-3 * HOUR) })], // 17:00
      recording: [recording(5, '録画中の番組', 'recording', { startAt: iso(-30 * 60_000) })], // 19:30
    })
    renderHome()

    const details = await openTimelineDetails()
    const rows = within(details).getAllByTestId('home-timeline-detail-row')
    expect(rows.map((row) => row.textContent)).toEqual([
      '今日 12:00地デジ昼の完了録れた',
      '今日 17:00地デジ夕方の失敗失敗',
      '今日 19:30地デジ録画中の番組録画中',
      '今日 22:00地デジ今夜の予約予約',
      '明日 09:00BS明日の予約予約',
    ])
  })

  it('複数 site のときは種別の前に site を出す', async () => {
    stubApi({
      reservations: [
        reservation(1, 'A', 2 * HOUR),
        reservation(2, 'B', 3 * HOUR, { site: 'sub' }),
      ],
    })
    renderHome()

    const details = await openTimelineDetails()
    expect(
      within(details).getAllByTestId('home-timeline-detail-row').map((row) => row.textContent),
    ).toEqual(['今日 22:00default · 地デジA予約', '今日 23:00sub · 地デジB予約'])
  })
})

describe('ホーム管理モード: 見るモードだけの取得はしない', () => {
  it('録画中の無条件一覧と続きからを取らない（見るモードでは取る）', async () => {
    const ops = stubApi({})
    const opsHome = renderHome('/?mode=ops')
    await screen.findByTestId('home-ops-timeline').catch(() => undefined)
    await new Promise((r) => setTimeout(r, 50))
    const paths = (fetchMock: typeof ops.fetchMock) =>
      fetchMock.mock.calls.map(([input]) => new URL(String(input), 'http://localhost'))
    expect(paths(ops.fetchMock).some((url) => url.pathname === '/api/recordings/continue-watching')).toBe(false)
    expect(
      paths(ops.fetchMock).some(
        (url) => url.pathname === '/api/recordings' && url.searchParams.get('status') === 'recording' && !url.searchParams.has('limit'),
      ),
    ).toBe(false)
    opsHome.unmount()

    const watch = stubApi({})
    renderHome('/?mode=watch')
    await waitFor(() =>
      expect(
        paths(watch.fetchMock).some((url) => url.pathname === '/api/recordings/continue-watching'),
      ).toBe(true),
    )
  })
})
