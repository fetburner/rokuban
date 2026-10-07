import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { getListReservationsQueryKey, getListRulesQueryKey } from '@/api/generated'
import type {
  CapacityOverage,
  EncodeProfileSummary,
  Recording,
  Reservation,
  Rule,
  RuleInput,
  Service,
} from '@/api/generated'
import { summarizeRuleConditions } from '@/components/rule-condition-summary'
import { RulesPage } from '@/pages/rules'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const sampleRule: Rule = {
  id: 1,
  name: 'ニュース',
  enabled: true,
  priority: 10,
  keepOriginal: 'always',
  encodeProfiles: [],
  createdAt: '2026-07-01T00:00:00Z',
  updatedAt: '2026-07-01T00:00:00Z',
}

function sampleReservation(
  id: number,
  ruleId: number,
  state: Reservation['state'] = 'active',
): Reservation {
  return {
    site: 'default',
    programId: 1000 + id,
    source: 'rule',
    ruleId,
    state,
    title: `番組 ${id}`,
    serviceName: 'NHK総合',
    channelType: 'GR',
    startAt: '2026-09-01T00:00:00Z',
    durationMs: 1_800_000,
    createdAt: '2026-08-01T00:00:00Z',
    updatedAt: '2026-08-01T00:00:00Z',
    skip: false,
    series: null,
  }
}

function sampleRecording(
  id: number,
  ruleId: number,
  status: Recording['status'] = 'recording',
  startAt = '2026-09-01T00:00:00Z',
): Recording {
  return {
    id,
    site: 'default',
    ruleId,
    source: 'rule',
    serviceName: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    networkId: 32736,
    serviceId: 1024,
    eventId: id,
    title: `録画 ${id}`,
    startAt,
    durationMs: 1_800_000,
    status,
    keepOriginal: 'always',
    cmDetection: { state: 'disabled' },
    createdAt: '2026-09-01T00:00:00Z',
  }
}

/**
 * ruleWithConditions は条件・UI を持たない項目の両方を埋めたルール。
 * 「復元される」「落ちない」を確認するテストの共通フィクスチャ。
 */
const ruleWithConditions: Rule = {
  id: 2,
  name: '平日ニュース',
  enabled: true,
  priority: 5,
  keepOriginal: 'always',
  encodeProfiles: [],
  textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース', negate: false }],
  genres: [1],
  channelTypes: ['GR'],
  times: [{ weekdays: 31, startSec: 75600, endSec: 82800 }],
  durationMinMs: 1_800_000,
  // UI を持たない項目（preserve が引き継ぐべきもの）
  dedupeEnabled: true,
  dedupeThreshold: 0.8,
  dedupeWindowSeconds: 3600,
  filenameTemplate: '{title}',
  metadata: { source: 'legacy' },
  createdAt: '2026-07-01T00:00:00Z',
  updatedAt: '2026-07-01T00:00:00Z',
}

const profiles: EncodeProfileSummary[] = [{ name: 'h264' }, { name: 'hevc' }]
const originalMatchMedia = Object.getOwnPropertyDescriptor(window, 'matchMedia')

const services: Service[] = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
  },
]

function stubApi(
  initialRules: Rule[] = [sampleRule],
  // 削除 API が返す内訳。既定は 0 件（大半のテストは内訳に関心が無い）。
  deleteImpact: { deletedReservations: number; detachedReservations: number } = {
    deletedReservations: 0,
    detachedReservations: 0,
  },
  // 作成・更新・削除を意図的に失敗させる（issue #297: 無音化した成功トーストの
  // 反対側 --- 失敗トーストは従来どおり出ることを確認するため）。既定では
  // 失敗させない。
  failures: {
    create?: number
    update?: number
    delete?: number
    reservations?: number
    capacityOverages?: number
    recordings?: number
  } = {},
  // 行のスイッチ（無効化）が確認に出す件数の母集団。RulesPage は予約一覧と
  // 同じクエリキーで GET /api/reservations を読む。既定は空。
  reservations: Reservation[] = [],
  // `GET /api/sites` の応答（既定 `['default']`）。`<ConditionFields>` の
  // サイトチップはレジストリと下書きの和集合が 2 つ以上のときだけ出るので、
  // それを確かめるテストだけが 2 つ目以降を足す（issue #531）。
  siteNames: string[] = ['default'],
  capacityOverages: CapacityOverage[] = [],
  recordings: Recording[] = [],
) {
  const putBodies: { id: number; body: RuleInput }[] = []
  const postBodies: RuleInput[] = []
  const deletedIds: number[] = []
  const recordingRequests: Record<string, string>[] = []
  // 作成・更新を状態に反映する --- invalidate 後の再取得で「新しい行が
  // 一覧に現れる」「更新後の内容が一覧に反映される」ことを確認するテストのため
  // （GET のたびに現在の状態を返す）。
  let state = [...initialRules]
  const replaceRule = (rule: Rule) => {
    state = state.map((item) => (item.id === rule.id ? rule : item))
  }

  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'

    if (url.pathname === '/api/rules' && method === 'GET') {
      return Promise.resolve(jsonResponse(state.filter((r) => !deletedIds.includes(r.id))))
    }
    const getMatch = /^\/api\/rules\/(\d+)$/.exec(url.pathname)
    if (getMatch && method === 'GET') {
      const rule = state.find((item) => item.id === Number(getMatch[1]))
      return Promise.resolve(
        rule === undefined
          ? jsonResponse({ error: 'ルールが見つかりません' }, 404)
          : jsonResponse(rule),
      )
    }
    if (url.pathname === '/api/reservations' && method === 'GET') {
      if (failures.reservations !== undefined) {
        return Promise.resolve(
          jsonResponse({ error: '予約数を取得できませんでした' }, failures.reservations),
        )
      }
      return Promise.resolve(jsonResponse(reservations))
    }
    if (url.pathname === '/api/recordings' && method === 'GET') {
      recordingRequests.push(Object.fromEntries(url.searchParams))
      if (failures.recordings !== undefined) {
        return Promise.resolve(
          jsonResponse({ error: '録画状況を取得できませんでした' }, failures.recordings),
        )
      }
      const status = url.searchParams.get('status')
      const ruleId = url.searchParams.get('ruleId')
      const recordingSource = url.searchParams.get('source')
      const before = url.searchParams.get('before')
      const beforeId = url.searchParams.get('beforeId')
      const limit = Number(url.searchParams.get('limit') ?? '50')
      const page = recordings
        .filter(
          (recording) =>
            (status === null || recording.status === status) &&
            (ruleId === null || recording.ruleId === Number(ruleId)) &&
            (recordingSource === null || recording.source === recordingSource) &&
            (before === null ||
              recording.startAt < before ||
              (recording.startAt === before && recording.id < Number(beforeId))),
        )
        .sort((left, right) =>
          right.startAt.localeCompare(left.startAt) || right.id - left.id,
        )
        .slice(0, limit)
      return Promise.resolve(jsonResponse(page))
    }
    if (url.pathname === '/api/capacity/overages' && method === 'GET') {
      if (failures.capacityOverages !== undefined) {
        return Promise.resolve(
          jsonResponse({ error: '容量超過を取得できませんでした' }, failures.capacityOverages),
        )
      }
      return Promise.resolve(jsonResponse(capacityOverages))
    }
    if (url.pathname === '/api/rules' && method === 'POST') {
      if (failures.create !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが作成を拒否しました' }, failures.create))
      }
      const body = JSON.parse(String(init?.body)) as RuleInput
      postBodies.push(body)
      const created: Rule = {
        ...body,
        id: 99,
        createdAt: '2026-08-01T00:00:00Z',
        updatedAt: '2026-08-01T00:00:00Z',
        // Rule は priority/keepOriginal/enabled を必須にするが RuleInput は
        // 任意（サーバー側の既定値埋めを前提にした契約）なので、フェイクでも
        // 同じ既定値をここで埋める。
        priority: body.priority ?? 0,
        keepOriginal: body.keepOriginal ?? 'always',
        enabled: body.enabled ?? true,
      }
      state = [...state, created]
      return Promise.resolve(jsonResponse(created))
    }
    const putMatch = /^\/api\/rules\/(\d+)$/.exec(url.pathname)
    if (putMatch && method === 'PATCH') {
      if (failures.update !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが更新を拒否しました' }, failures.update))
      }
      const id = Number(putMatch[1])
      const body = JSON.parse(String(init?.body)) as RuleInput
      putBodies.push({ id, body })
      const existing = state.find((r) => r.id === id)
      const updated: Rule = {
        ...body,
        id,
        createdAt: existing?.createdAt ?? '2026-07-01T00:00:00Z',
        updatedAt: '2026-08-01T00:00:00Z',
        priority: body.priority ?? 0,
        keepOriginal: body.keepOriginal ?? 'always',
        enabled: body.enabled ?? true,
      }
      state = state.map((r) => (r.id === id ? updated : r))
      return Promise.resolve(jsonResponse(updated))
    }
    if (putMatch && method === 'DELETE') {
      if (failures.delete !== undefined) {
        return Promise.resolve(jsonResponse({ error: 'サーバーが削除を拒否しました' }, failures.delete))
      }
      const id = Number(putMatch[1])
      deletedIds.push(id)
      return Promise.resolve(jsonResponse({ id, ...deleteImpact }))
    }
    if (url.pathname === '/api/encode-profiles') return Promise.resolve(jsonResponse(profiles))
    if (url.pathname === '/api/breakers') return Promise.resolve(jsonResponse([]))
    // 条件フォームのサービス選択肢は全 site から作る（issue #290）ので、
    // 単一サイト構成でも `GET /api/sites` を経由する。
    if (url.pathname === '/api/sites') return Promise.resolve(jsonResponse(siteNames))
    const servicesMatch = /^\/api\/sites\/([^/]+)\/services$/.exec(url.pathname)
    if (servicesMatch && siteNames.includes(servicesMatch[1])) {
      return Promise.resolve(jsonResponse(services))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch

  return { postBodies, putBodies, deletedIds, recordingRequests, replaceRule }
}

function renderPage() {
  return renderInRouter(<RulesPage />)
}

function enableFinePointer() {
  window.matchMedia = vi.fn((query: string) => ({
    matches: query === '(pointer: fine)',
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia
}

afterEach(() => {
  vi.restoreAllMocks()
  if (originalMatchMedia) {
    Object.defineProperty(window, 'matchMedia', originalMatchMedia)
  } else {
    Reflect.deleteProperty(window, 'matchMedia')
  }
})

describe('summarizeRuleConditions', () => {
  it('条件が無ければ空配列を返す', () => {
    expect(summarizeRuleConditions(sampleRule)).toEqual([])
  })

  it('テキスト・ジャンル・時間帯を要約する', () => {
    const summary = summarizeRuleConditions(ruleWithConditions)
    expect(summary).toContain('番組名に「ニュース」を含む')
    expect(summary).toContain('スポーツ')
    expect(summary).toContain('月〜金 21:00〜23:00')
    expect(summary).toContain('30分以上')
  })

  // 検索・ルール条件の「サービス」は利用者向けには「チャンネル」で統一する
  // （API の `services` フィールド名自体は変えない）。
  it('サービス条件はチャンネル件数の要約になる', () => {
    const rule: Rule = {
      ...sampleRule,
      services: [
        { networkId: 1, serviceId: 1 },
        { networkId: 1, serviceId: 2 },
      ],
    }
    expect(summarizeRuleConditions(rule)).toContain('チャンネル 2 件')
  })
})

describe('RulesPage 新規作成の入口', () => {
  it('PC とモバイルの作成リンクは検索画面を開き、録画ルールフォームは持たない', async () => {
    stubApi([])
    const user = userEvent.setup()
    const { router } = renderPage()

    await screen.findByText('ルールがありません')
    const links = screen.getAllByRole('link', { name: 'ルールを作成' })
    expect(links).toHaveLength(2)
    expect(links[0]).toHaveClass('hidden', 'lg:inline-flex')
    expect(links[1]).toHaveClass('w-full', 'lg:hidden')
    for (const link of links) expect(link).toHaveAttribute('href', '/search')
    expect(screen.queryByRole('form', { name: 'ルールを作成' })).not.toBeInTheDocument()

    await user.click(links[0]!)
    await waitFor(() => expect(router.state.location.pathname).toBe('/search'))
    expect(router.state.location.search).toEqual({})
  })
})

describe('RulesPage 一覧', () => {
  it('一覧に条件の要約が出て、空のルールは「すべての番組」と分かる', async () => {
    stubApi([sampleRule, ruleWithConditions])
    renderPage()

    await screen.findByText('ニュース')
    expect(screen.getByText('条件なし（すべての番組にマッチ）')).toBeInTheDocument()
    expect(screen.getByText('番組名に「ニュース」を含む')).toBeInTheDocument()
  })

  it('無効なルールに「無効」バッジが出る', async () => {
    stubApi([{ ...sampleRule, enabled: false }])
    renderPage()

    await screen.findByText('ニュース')
    const badge = screen.getByText('無効')
    expect(badge).toBeInTheDocument()
    // text-muted-foreground だと bg-muted との合成後コントラストがライトで
    // 4.5 を割る（issue #308）。jsdom は色を測れないので、退行防止としては
    // クラス名のリテラル比較まで（実測は e2e:design の担当）。
    expect(badge.className).toContain('text-foreground')
    expect(badge.className).not.toContain('text-muted-foreground')
  })

  it('ルール名が検索画面への編集リンクになり、同じ導線を重複させない', async () => {
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    const link = screen.getByRole('link', { name: 'ルール「平日ニュース」を編集' })
    expect(link).toHaveAttribute('href', '/search?ruleId=2')
    expect(link).toHaveClass('min-h-8', 'text-primary')
    // ラベルではなく href そのものを数える --- 別ラベルの 2 本目を足しても
    // 「同じ導線を重複させない」という主張はラベルの不在テストでは検査できない。
    const searchLinks = screen
      .getAllByRole('link')
      .filter((el) => el.getAttribute('href') === '/search?ruleId=2')
    expect(searchLinks).toHaveLength(1)
  })

  it('同名のルールは一覧の名前と操作ラベルに id を添えて押し分ける', async () => {
    stubApi([
      sampleRule,
      { ...sampleRule, id: 2, name: 'ニュース' },
    ])
    renderPage()

    expect(await screen.findByText('ニュース (#1)')).toBeInTheDocument()
    expect(await screen.findByText('ニュース (#2)')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ルール「ニュース (#1)」を編集' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'ルール「ニュース (#2)」を編集' })).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'ルール「ニュース (#1)」を有効にする' })).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'ルール「ニュース (#2)」を有効にする' })).toBeInTheDocument()
  })

  // issue #137: ルールから、そのルール由来の録画だけに絞った一覧への導線。
  // 条件モデルを検索と共有しないため、遷移先は /search ではなく /recordings。
  it('「このルールの録画」リンクが /recordings?ruleId=<id> を指す', async () => {
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    const link = screen.getByRole('link', { name: 'このルールの録画' })
    expect(link).toHaveAttribute('href', '/recordings?ruleId=2')
  })
})

describe('RulesPage の分類ルール導線', () => {
  it('/rules に分類ルールの管理セクションを表示しない', async () => {
    stubApi([])
    renderPage()

    await screen.findByText('ルールがありません')
    expect(screen.queryByText('シリーズ分類')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '分類ルールを作成' })).not.toBeInTheDocument()
  })

  it('録画ルールの「このキーワードで分類ルールを作る」導線は同じフォームを開く', async () => {
    const user = userEvent.setup()
    stubApi([ruleWithConditions])
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: 'このキーワードで分類ルールを作る' }))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: '分類ルールを作成' })).toBeInTheDocument()
    expect(within(dialog).getByLabelText('キーワード')).toHaveValue('ニュース')
  })
})

describe('RulesPage 予約の稼働状況', () => {
  it('予約の結論を同じ ruleId の全 source で集計し、不足件数と予約絞り込みへのリンクを出す', async () => {
    const reservations: Reservation[] = [
      {
        ...sampleReservation(1, 1),
        startAt: '2026-09-01T20:00:00Z',
      },
      {
        ...sampleReservation(2, 1),
        source: 'manual',
        startAt: '2026-09-01T22:00:00Z',
      },
      { ...sampleReservation(4, 1), skip: true, startAt: '2026-09-01T20:00:00Z' },
      {
        ...sampleReservation(5, 1),
        state: 'orphaned',
        startAt: '2026-09-01T20:00:00Z',
      },
      sampleReservation(6, 2),
    ]
    const capacityOverages: CapacityOverage[] = [
      {
        site: 'default',
        startAt: '2026-09-01T20:00:00Z',
        endAt: '2026-09-01T20:30:00Z',
        shortfall: 1,
        jammedTypes: ['GR'],
      },
    ]
    stubApi([sampleRule], undefined, {}, reservations, ['default'], capacityOverages)
    const user = userEvent.setup()
    const { router } = renderPage()

    const link = await screen.findByRole('link', { name: '録画予定 2 件' })
    expect(link).toHaveAttribute('href', '/reservations?ruleId=1')
    expect(await screen.findByText(/うち不足時間帯 1/)).toHaveClass('text-warning')

    await user.click(link)
    await waitFor(() => expect(router.state.location.pathname).toBe('/reservations'))
    expect(router.state.location.search).toEqual({ ruleId: 1 })
  })

  it('有効な予約が無いルールは muted 表示にし、無効なルールには出さない', async () => {
    const disabledRule = { ...sampleRule, id: 2, name: '季節もの', enabled: false }
    stubApi([sampleRule, disabledRule], undefined, {}, [
      { ...sampleReservation(1, 1), skip: true },
      { ...sampleReservation(2, 1), state: 'orphaned' },
    ])
    renderPage()

    const none = await screen.findByText('録画予定なし')
    expect(none).toHaveClass('text-muted-foreground')
    expect(none.closest('li')?.querySelector('a[href="/reservations?ruleId=1"]')).toBeNull()
    const disabledRow = screen
      .getByRole('link', { name: 'ルール「季節もの」を編集' })
      .closest('li')
    expect(disabledRow).not.toBeNull()
    expect(within(disabledRow as HTMLElement).getByText('無効')).toBeInTheDocument()
    expect(within(disabledRow as HTMLElement).queryByText('録画予定なし')).not.toBeInTheDocument()
  })

  it('予約一覧の取得に失敗したら稼働状況を出さない', async () => {
    stubApi([sampleRule], undefined, { reservations: 500 })
    renderPage()

    await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    await waitFor(() => {
      expect(
        (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.some(
          (call: unknown[]) =>
            new URL(String(call[0]), 'http://localhost').pathname === '/api/reservations',
        ),
      ).toBe(true)
    })
    expect(screen.queryByText(/録画予定/)).not.toBeInTheDocument()
    expect(screen.queryByText('録画予定なし')).not.toBeInTheDocument()
  })

  it('容量超過の取得中は不足件数を出さず、録画予定は残す', async () => {
    const reservations = [
      { ...sampleReservation(1, 1), startAt: '2026-09-01T20:00:00Z' },
    ]
    stubApi([sampleRule], undefined, {}, reservations)
    const baseFetch = globalThis.fetch
    let resolveCapacity: ((response: Response) => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/capacity/overages') {
        return new Promise<Response>((resolve) => {
          resolveCapacity = resolve
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch
    const { queryClient } = renderPage()

    expect(await screen.findByRole('link', { name: '録画予定 1 件' })).toBeInTheDocument()
    expect(screen.queryByText(/うち不足時間帯/)).not.toBeInTheDocument()
    await waitFor(() => expect(resolveCapacity).toBeDefined())
    resolveCapacity?.(jsonResponse([]))
    await waitFor(() => {
      const capacityQuerySucceeded = queryClient
        .getQueryCache()
        .getAll()
        .some(
          (query) =>
            query.queryKey[0] === '/api/capacity/overages' &&
            query.state.status === 'success',
        )
      expect(capacityQuerySucceeded).toBe(true)
    })
    expect(screen.queryByText(/うち不足時間帯/)).not.toBeInTheDocument()
  })

  it('容量超過の取得に失敗したら不足件数だけ省き、録画予定は残す', async () => {
    stubApi([sampleRule], undefined, { capacityOverages: 500 }, [sampleReservation(1, 1)])
    renderPage()

    expect(await screen.findByRole('link', { name: '録画予定 1 件' })).toBeInTheDocument()
    expect(screen.queryByText(/うち不足時間帯/)).not.toBeInTheDocument()
  })
})

describe('RulesPage ルールの有効スイッチ', () => {
  it('状態を aria-checked に出し、録画中は予約数と録画中数付きの確認を挟む', async () => {
    const reservations: Reservation[] = [
      sampleReservation(1, 1),
      sampleReservation(2, 1),
      sampleReservation(3, 1, 'detached'),
      sampleReservation(4, 2),
      { ...sampleReservation(5, 1), source: 'manual' },
    ]
    const { putBodies, recordingRequests } = stubApi(
      [sampleRule],
      undefined,
      {},
      reservations,
      ['default'],
      [],
      [
        sampleRecording(10, 1),
        sampleRecording(11, 1),
        sampleRecording(12, 2),
        sampleRecording(13, 1, 'finished'),
        { ...sampleRecording(14, 1), source: 'manual' },
      ],
    )
    const user = userEvent.setup()
    const { queryClient } = renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    await user.click(toggle)

    expect(
      await screen.findByText(
        '「ニュース」を無効にすると、このルールによる予約 2 件が取り消されます。手動で予約したものは残ります。録画中の 2 件は録画が止まります。',
      ),
    ).toBeInTheDocument()
    expect(recordingRequests[0]).toMatchObject({
      status: 'recording',
      ruleId: '1',
      source: 'rule',
    })
    expect(putBodies).toHaveLength(0)

    const reservationCallsBeforeDisable = (
      globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    ).mock.calls.filter(
      (call: unknown[]) =>
        new URL(String(call[0]), 'http://localhost').pathname === '/api/reservations',
    ).length
    await user.click(screen.getByRole('button', { name: '無効にする' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    // PATCH は RuleInput.name が必須で全置換する契約なので、検索画面の上書き
    // フォームと同じ入力を送りつつ enabled だけを変更する。
    expect(putBodies[0]).toMatchObject({ id: 1, body: { name: 'ニュース', enabled: false } })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    // 無効化は予約一覧を invalidate するが、ruler の次回評価までは同じ予約が
    // キャッシュに残る。即座に行が消えることは期待しない。
    await waitFor(() => {
      const reservationCalls = (
        globalThis.fetch as unknown as ReturnType<typeof vi.fn>
      ).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/reservations',
      ).length
      expect(reservationCalls).toBeGreaterThan(reservationCallsBeforeDisable)
    })
    expect(
      queryClient.getQueryData<{ data: Reservation[] }>(getListReservationsQueryKey())?.data,
    ).toEqual(reservations)
  })

  it('録画中が無ければ確認なしで無効にし、予約件数付きのトーストから戻せる', async () => {
    const reservations: Reservation[] = [
      sampleReservation(1, 1),
      sampleReservation(2, 1, 'detached'),
      { ...sampleReservation(3, 1), source: 'manual' },
    ]
    const { putBodies, recordingRequests } = stubApi(
      [sampleRule],
      undefined,
      {},
      reservations,
      ['default'],
      [],
      [sampleRecording(10, 2), sampleRecording(11, 1, 'finished')],
    )
    const user = userEvent.setup()
    const { queryClient } = renderPage()
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    await user.click(toggle)

    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(screen.queryByText(/を無効にしますか/)).not.toBeInTheDocument()
    expect(recordingRequests[0]).toMatchObject({ status: 'recording', ruleId: '1' })
    expect(
      await screen.findByText('ルール「ニュース」を無効にしました。予約 1 件が取り消されます'),
    ).toBeInTheDocument()
    expect(putBodies[0]).toMatchObject({ id: 1, body: { enabled: false } })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    await user.click(screen.getByRole('button', { name: '元に戻す' }))
    await waitFor(() => expect(putBodies).toHaveLength(2))

    expect(putBodies[1]).toMatchObject({ id: 1, body: { enabled: true } })
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'true'))
    expect(
      queryClient.getQueryData<{ data: Reservation[] }>(getListReservationsQueryKey())?.data,
    ).toEqual(reservations)
  })

  it('Undo はトースト表示中に編集されたルールの最新内容を保つ', async () => {
    const { putBodies, replaceRule } = stubApi([sampleRule])
    const user = userEvent.setup()
    renderPage()
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    await user.click(toggle)
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(await screen.findByRole('button', { name: '元に戻す' })).toBeInTheDocument()

    replaceRule({ ...sampleRule, enabled: false, name: '更新後のニュース', priority: 42 })
    await user.click(screen.getByRole('button', { name: '元に戻す' }))
    await waitFor(() => expect(putBodies).toHaveLength(2))

    expect(putBodies[1]).toMatchObject({
      id: 1,
      body: { enabled: true, name: '更新後のニュース', priority: 42 },
    })
  })

  it('Undo の更新が失敗したら無効状態へ戻す', async () => {
    const failures: { update?: number } = {}
    const { putBodies } = stubApi([sampleRule], undefined, failures)
    const user = userEvent.setup()
    const { queryClient } = renderPage()
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    await user.click(toggle)
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(await screen.findByRole('button', { name: '元に戻す' })).toBeInTheDocument()

    failures.update = 500
    await user.click(screen.getByRole('button', { name: '元に戻す' }))

    expect(await screen.findByText('サーバーが更新を拒否しました')).toBeInTheDocument()
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'false'))
    expect(
      queryClient.getQueryData<{ data: Rule[] }>(getListRulesQueryKey())?.data[0]?.enabled,
    ).toBe(false)
  })

  it('画面遷移後の Undo 失敗も通知してキャッシュを無効状態へ戻す', async () => {
    const failures: { update?: number } = {}
    const { putBodies } = stubApi([sampleRule], undefined, failures)
    const user = userEvent.setup()
    function PageHarness() {
      const [showRules, setShowRules] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setShowRules(false)}>別ページへ</button>
          {showRules && <RulesPage />}
        </>
      )
    }
    const { queryClient } = renderInRouter(<PageHarness />)
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    await user.click(toggle)
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(await screen.findByRole('button', { name: '元に戻す' })).toBeInTheDocument()

    failures.update = 500
    await user.click(screen.getByRole('button', { name: '別ページへ' }))
    expect(screen.queryByRole('switch', { name: 'ルール「ニュース」を有効にする' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '元に戻す' }))

    expect(await screen.findByText('サーバーが更新を拒否しました')).toBeInTheDocument()
    expect(
      queryClient.getQueryData<{ data: Rule[] }>(getListRulesQueryKey())?.data[0]?.enabled,
    ).toBe(false)
  })

  it('録画中が手動予約由来だけなら確認なしで無効にする', async () => {
    const { putBodies } = stubApi(
      [sampleRule],
      undefined,
      {},
      [],
      ['default'],
      [],
      [{ ...sampleRecording(10, 1), source: 'manual' }],
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(
      await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' }),
    )

    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(screen.queryByText(/を無効にしますか/)).not.toBeInTheDocument()
    expect(
      await screen.findByText('ルール「ニュース」を無効にしました。予約 0 件が取り消されます'),
    ).toBeInTheDocument()
  })

  it('録画中が 200 件を超えてもすべて数えて確認する', async () => {
    const recordings = Array.from({ length: 201 }, (_, index) =>
      sampleRecording(
        index + 1,
        1,
        'recording',
        new Date(Date.UTC(2026, 0, 1, 0, index)).toISOString(),
      ),
    )
    const { recordingRequests } = stubApi(
      [sampleRule],
      undefined,
      {},
      [],
      ['default'],
      [],
      recordings,
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(
      await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' }),
    )

    expect(await screen.findByText(/録画中の 201 件は録画が止まります/)).toBeInTheDocument()
    expect(recordingRequests).toHaveLength(2)
  })

  it('録画中の録画一覧を取得できない場合は無効化を進めない', async () => {
    const { putBodies } = stubApi([sampleRule], undefined, { recordings: 500 })
    const user = userEvent.setup()
    renderPage()
    await user.click(
      await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' }),
    )

    expect(await screen.findByText('録画状況を取得できませんでした')).toBeInTheDocument()
    expect(screen.queryByText(/を無効にしますか/)).not.toBeInTheDocument()
    expect(putBodies).toHaveLength(0)
  })

  it('予約一覧の取得完了前は件数と無効化確認を出さない', async () => {
    stubApi([sampleRule], undefined, {}, [], ['default'], [], [sampleRecording(1, 1)])
    const baseFetch = globalThis.fetch
    let resolveReservations: (() => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/reservations') {
        return new Promise<Response>((resolve) => {
          resolveReservations = () => resolve(jsonResponse([]))
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })

    expect(screen.queryByText(/録画予定/)).not.toBeInTheDocument()
    await user.click(toggle)
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()

    resolveReservations?.()
    expect(await screen.findByText('録画予定なし')).toBeInTheDocument()
    expect(await screen.findByText(/を無効にすると/)).toBeInTheDocument()
  })

  it('予約一覧の取得に失敗したら件数確認を開かず、API 本文をトーストに出す', async () => {
    stubApi([sampleRule], undefined, { reservations: 500 })
    const user = userEvent.setup()
    renderPage()

    await user.click(
      await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' }),
    )

    expect(await screen.findByText('予約数を取得できませんでした')).toBeInTheDocument()
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()
  })

  it('有効化は確認を挟まず、PATCH の応答前に状態を楽観更新する', async () => {
    stubApi([{ ...sampleRule, enabled: false }])
    const baseFetch = globalThis.fetch
    let updated = false
    let resolveUpdate: (() => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/rules' && (init?.method ?? 'GET') === 'GET' && updated) {
        return Promise.resolve(jsonResponse([{ ...sampleRule, enabled: true }]))
      }
      if (url.pathname === '/api/rules/1' && init?.method === 'PATCH') {
        return new Promise<Response>((resolve) => {
          resolveUpdate = () => {
            updated = true
            resolve(jsonResponse({ ...sampleRule, enabled: true }))
          }
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    await user.click(toggle)

    await waitFor(() => expect(resolveUpdate).toBeDefined())
    expect(screen.queryByText(/を無効にすると/)).not.toBeInTheDocument()
    expect(toggle).toHaveAttribute('aria-checked', 'true')

    const updateCall = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'PATCH',
    )
    if (updateCall === undefined) throw new Error('PATCH was not recorded')
    expect(JSON.parse(String((updateCall[1] as RequestInit).body))).toMatchObject({
      name: 'ニュース',
      enabled: true,
    })
    if (resolveUpdate === undefined) throw new Error('PATCH was not started')
    resolveUpdate()
    await waitFor(() => {
      const ruleGets = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/rules' &&
          ((call[1] as RequestInit | undefined)?.method ?? 'GET') === 'GET',
      )
      expect(ruleGets.length).toBeGreaterThanOrEqual(2)
    })
  })

  it('別ルールの更新成功後に先の更新が失敗しても、成功した行を巻き戻さない', async () => {
    const otherRule: Rule = { ...sampleRule, id: 2, name: 'スポーツ', enabled: false }
    stubApi([{ ...sampleRule, enabled: false }, otherRule])
    const baseFetch = globalThis.fetch
    let failFirstUpdate: ((response: Response) => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/rules/1' && init?.method === 'PATCH') {
        return new Promise<Response>((resolve) => {
          failFirstUpdate = resolve
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const first = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    const second = screen.getByRole('switch', { name: 'ルール「スポーツ」を有効にする' })

    await user.click(first)
    await waitFor(() => expect(failFirstUpdate).toBeDefined())
    await user.click(second)
    await waitFor(() => {
      const ruleGets = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call: unknown[]) =>
          new URL(String(call[0]), 'http://localhost').pathname === '/api/rules' &&
          ((call[1] as RequestInit | undefined)?.method ?? 'GET') === 'GET',
      )
      expect(ruleGets.length).toBeGreaterThanOrEqual(2)
    })

    if (failFirstUpdate === undefined) throw new Error('first PATCH was not started')
    failFirstUpdate(jsonResponse({ error: '先の更新に失敗しました' }, 500))

    expect(await screen.findByText('先の更新に失敗しました')).toBeInTheDocument()
    await waitFor(() => expect(first).toHaveAttribute('aria-checked', 'false'))
    expect(second).toHaveAttribute('aria-checked', 'true')
  })

  it('予約数の取得中は全ルールのスイッチを無効にして、確認を重ねて開かせない', async () => {
    const otherRule: Rule = { ...sampleRule, id: 2, name: 'スポーツ' }
    stubApi(
      [sampleRule, otherRule],
      undefined,
      {},
      [],
      ['default'],
      [],
      [sampleRecording(1, sampleRule.id)],
    )
    const baseFetch = globalThis.fetch
    let resolveReservations: ((response: Response) => void) | undefined
    globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/reservations') {
        return new Promise<Response>((resolve) => {
          resolveReservations = resolve
        })
      }
      return baseFetch(input, init)
    }) as unknown as typeof fetch

    const user = userEvent.setup()
    renderPage()
    const switches = await screen.findAllByRole('switch')
    await user.click(switches[0])
    await waitFor(() => expect(resolveReservations).toBeDefined())

    expect(switches[0]).toBeDisabled()
    expect(switches[1]).toBeDisabled()

    if (resolveReservations === undefined) throw new Error('reservation fetch was not started')
    resolveReservations(jsonResponse([]))
    expect(await screen.findByText(/を無効にすると/)).toBeInTheDocument()
  })

  it('更新失敗時は楽観更新を戻し、API 本文をトーストに出す', async () => {
    stubApi([{ ...sampleRule, enabled: false }], undefined, { update: 500 })
    const user = userEvent.setup()
    renderPage()

    const toggle = await screen.findByRole('switch', { name: 'ルール「ニュース」を有効にする' })
    await user.click(toggle)

    expect(await screen.findByText('サーバーが更新を拒否しました')).toBeInTheDocument()
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'false'))
  })

  it('PATCH 本文は UI を持たない項目（dedupe* / filenameTemplate / metadata）を保持する', async () => {
    const disabledRule = { ...ruleWithConditions, enabled: false }
    const { putBodies } = stubApi([disabledRule])
    const user = userEvent.setup()
    renderPage()

    const toggle = await screen.findByRole('switch', {
      name: 'ルール「平日ニュース」を有効にする',
    })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    await user.click(toggle)

    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0]).toMatchObject({
      id: 2,
      body: {
        name: '平日ニュース',
        enabled: true,
        dedupeEnabled: true,
        dedupeThreshold: 0.8,
        dedupeWindowSeconds: 3600,
        filenameTemplate: '{title}',
        metadata: { source: 'legacy' },
      },
    })
  })
})

// issue #227（M5-4）: 削除（稀・破壊的）を行の overflow メニューへ寄せ、
// 作成フォームの保存・キャンセルと同格には並べない。
describe('RulesPage 削除は overflow メニュー', () => {
  it('一覧の行に「削除」ボタンが直接は出ない（overflow の中）', async () => {
    stubApi()
    renderPage()

    // 行が描画されたことを先に待ってから「無い」ことを確認する
    // （非同期の空虚な成功を避ける）。
    await screen.findByText('ニュース')
    expect(screen.queryByRole('button', { name: '削除' })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '削除' })).not.toBeInTheDocument()
  })

  it('overflow を開くと「削除」が menuitem として出て、選ぶと確認ダイアログの上で削除される', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))

    const deleteItem = await screen.findByRole('menuitem', { name: '削除' })
    await user.click(deleteItem)

    expect(await screen.findByText('ルール「ニュース」を削除しますか？')).toBeInTheDocument()
    // ダイアログを開いただけでは DELETE は飛ばない
    const deleteCallsBeforeConfirm = (
      globalThis.fetch as unknown as ReturnType<typeof vi.fn>
    ).mock.calls.filter((call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE')
    expect(deleteCallsBeforeConfirm.length).toBe(0)

    // 取り返しがつかない操作の確定は destructive（issue #467、
    // alert-dialog.tsx の規約。variant を外すと落ちる）。
    const confirmButton = screen.getByRole('button', { name: '削除する' })
    expect(confirmButton).toHaveClass('text-destructive')

    await user.click(confirmButton)

    // 削除された行が一覧から消えることそのものが常に画面に見える
    // （RulesPage はフィルタもページングも持たない）ので、内訳が 0/0 の
    // ときは成功トーストを無音化する（issue #297）。行の消失で削除が
    // 効いたことを確認する。
    await waitFor(() => expect(screen.queryByText('ニュース')).not.toBeInTheDocument())
    expect(screen.queryByText('ルールを削除しました')).not.toBeInTheDocument()
  })

  // 削除 API の内訳（削除 N 件 / 編集済みのため残った M 件）をトーストに出す
  // （reservation-model.md §4.3「ルール削除の UX は可視化で解決する」）。
  // 残った予約は「ユーザーが自分で触ったもの」だけなので、黙って残すと
  // 一覧に見慣れないマーカー付きの行が増えた理由が分からなくなる。
  it('削除のトーストに予約の内訳（削除 / 残った件数）が出る', async () => {
    stubApi([sampleRule], { deletedReservations: 3, detachedReservations: 2 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(
      await screen.findByText('ルールを削除しました（予約 3 件を削除、2 件は編集済みのため残しました）'),
    ).toBeInTheDocument()
  })

  // detached が 0 でも削除した予約があれば内訳を出す（0 件で黙るのは
  // 「何も起きていない削除」のときだけ、という境界の反対側）。
  it('残った予約が 0 件でも、削除した予約があれば件数を出す', async () => {
    stubApi([sampleRule], { deletedReservations: 4, detachedReservations: 0 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(await screen.findByText('ルールを削除しました（予約 4 件を削除）')).toBeInTheDocument()
  })

  // issue #215: 重複排除の比較対象は「同じ rule_id の recordings」なので、
  // ルールを削除すると履歴がスコープから外れ、同じ条件で作り直しても
  // 引き継がれない（docs/recording/ruler.md §3.1）。押した後では取り返せない
  // 副作用なので、確認の時点で伝える。
  it('重複排除が有効なルールの削除確認に、履歴が外れることと検索編集への案内が出る', async () => {
    stubApi([ruleWithConditions])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('平日ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「平日ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))

    expect(await screen.findByText('ルール「平日ニュース」を削除しますか？')).toBeInTheDocument()
    const description = screen.getByText(/重複排除の履歴も一緒に外れます/)
    expect(description.textContent).toContain('重複排除の履歴も一緒に外れます')
    expect(description.textContent).toContain('作り直しても引き継がれない')
    expect(description.textContent).toContain('ルール名から編集')
    // 被害の大きさを docs より強く書かない（過剰録画は一過性で、新ルールの
    // 下で 1 本録れれば以降は再び弾かれる ——
    // TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete 段階 3 の測定）。
    expect(description.textContent).toContain('1 本録れれば以降はまた弾かれます')
    expect(description.textContent).not.toContain('窓の中の再放送を録り直します')

    // 確認せずにキャンセルする（副作用の大きい操作なので、この時点では
    // まだ削除されていないことを確認する）。
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    const deleteCalls = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCalls.length).toBe(0)
  })

  // 反対方向: 重複排除を使っていないルールでは警告を出さない
  // （常に出す実装だと、無関係なルールにも意味の無い長文が付く）。
  it('重複排除が無効なルールの削除確認には履歴の警告を出さない', async () => {
    stubApi([sampleRule])
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))

    await screen.findByText('ルール「ニュース」を削除しますか？')
    // 本文そのものを固定する（存在チェックだけだと、この文言が空や別の
    // 表現に変わっても気付けない --- 他の破壊的確認と同じ「取り消せません」
    // の語彙を使っていることも含めて固定する）。
    expect(
      screen.getByText('ルールの設定を削除します。取り消せません。'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/重複排除の履歴も一緒に外れます/)).not.toBeInTheDocument()
  })

  it('確認をキャンセルすると削除されない', async () => {
    stubApi()
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: 'キャンセル' }))

    // DELETE が飛んでいないことを確認する（行が残っていることでも分かるが、
    // ネットワーク呼び出しの有無を直接見て確定させる）
    await waitFor(() =>
      expect(screen.queryByText('ルール「ニュース」を削除しますか？')).not.toBeInTheDocument(),
    )
    const deleteCalls = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      (call: unknown[]) => (call[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCalls.length).toBe(0)
    expect(screen.getByText('ニュース')).toBeInTheDocument()
  })

})

describe('ルール行のコンテキストメニュー', () => {
  it('既存の有効切替確認と削除確認を使う', async () => {
    enableFinePointer()
    const { putBodies, deletedIds } = stubApi(
      [sampleRule],
      { deletedReservations: 0, detachedReservations: 0 },
      undefined,
      [sampleReservation(1, sampleRule.id)],
      ['default'],
      [],
      [sampleRecording(1, sampleRule.id)],
    )
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    const ruleLink = screen.getByRole('link', { name: 'ルール「ニュース」を編集' })
    const ruleRow = ruleLink.closest('div.rounded-lg.border')
    expect(ruleRow).not.toBeNull()
    fireEvent.contextMenu(ruleRow!, { clientX: 80, clientY: 40 })
    const disableMenu = await screen.findByRole('menu')
    expect(within(disableMenu).getByRole('menuitem', { name: '無効にする' })).toBeInTheDocument()
    expect(within(disableMenu).getByRole('menuitem', { name: '削除' })).toBeInTheDocument()
    await user.click(within(disableMenu).getByRole('menuitem', { name: '無効にする' }))

    expect(await screen.findByText('ルール「ニュース」を無効にしますか？')).toBeInTheDocument()
    expect(putBodies).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: '無効にする' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0]).toMatchObject({ id: sampleRule.id, body: { enabled: false } })

    const updatedRow = screen
      .getByRole('link', { name: 'ルール「ニュース」を編集' })
      .closest('div.rounded-lg.border')
    expect(updatedRow).not.toBeNull()
    fireEvent.contextMenu(updatedRow!, { clientX: 80, clientY: 40 })
    const enableMenu = await screen.findByRole('menu')
    await user.click(within(enableMenu).getByRole('menuitem', { name: '有効にする' }))
    await waitFor(() => expect(putBodies).toHaveLength(2))
    expect(putBodies[1]).toMatchObject({ id: sampleRule.id, body: { enabled: true } })

    const enabledRow = screen
      .getByRole('link', { name: 'ルール「ニュース」を編集' })
      .closest('div.rounded-lg.border')
    expect(enabledRow).not.toBeNull()
    fireEvent.contextMenu(enabledRow!, { clientX: 80, clientY: 40 })
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    expect(await screen.findByText('ルール「ニュース」を削除しますか？')).toBeInTheDocument()
    expect(deletedIds).toEqual([])
    await user.click(screen.getByRole('button', { name: 'キャンセル' }))
    expect(deletedIds).toEqual([])
  })
})

describe('RulesPage 削除エラー', () => {
  it('削除に失敗すれば失敗トーストは出て、行は一覧に残る', async () => {
    stubApi([sampleRule], undefined, { delete: 500 })
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ニュース')
    await user.click(screen.getByRole('button', { name: 'ルール「ニュース」のその他の操作' }))
    await user.click(await screen.findByRole('menuitem', { name: '削除' }))
    await user.click(await screen.findByRole('button', { name: '削除する' }))

    expect(await screen.findByText('サーバーが削除を拒否しました')).toBeInTheDocument()
    expect(screen.getByText('ニュース')).toBeInTheDocument()
  })
})
