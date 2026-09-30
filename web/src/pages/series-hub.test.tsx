import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { LabelRule, ProgramSearchMatch, Recording, Rule } from '@/api/generated'
import { SeriesHubPage } from '@/pages/series-hub'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** recording は一覧の要素に必要な列だけを持つ行を作る。 */
function recording(over: Partial<Recording> & { id: number }): Recording {
  return {
    title: `番組${over.id}`,
    series: '作品X',
    seriesKey: '作品X',
    startAt: '2026-09-01T12:00:00Z',
    durationMs: 1_800_000,
    site: 'default',
    serviceName: 'テスト',
    channelType: 'GR',
    channel: '1',
    networkId: 32678,
    serviceId: 5168,
    eventId: 1,
    status: 'finished',
    source: 'manual',
    keepOriginal: 'always',
    createdAt: '2026-09-01T12:00:00Z',
    cmDetection: { state: 'disabled' },
    ingest: { state: 'committed' },
    ...over,
  } as Recording
}

function program(over: Partial<ProgramSearchMatch>): ProgramSearchMatch {
  return {
    site: 'default',
    programId: 1,
    networkId: 32678,
    serviceId: 5168,
    startAt: '2026-09-30T12:00:00Z',
    durationMs: 1_800_000,
    name: 'アニメ　作品X　第2話',
    isFree: true,
    ...over,
  }
}

function labelRule(over: Partial<LabelRule> & { id: number }): LabelRule {
  return {
    key: 'series',
    keyword: '作品',
    value: '作品X',
    valueKey: '作品X',
    priority: 0,
    createdAt: '2026-09-01T00:00:00Z',
    updatedAt: '2026-09-01T00:00:00Z',
    ...over,
  } as LabelRule
}

function rule(over: Partial<Rule> & { id: number }): Rule {
  return {
    name: '毎週録画',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: '2026-09-01T00:00:00Z',
    updatedAt: '2026-09-01T00:00:00Z',
    ...over,
  } as Rule
}

/**
 * stubApi は起点の録画・次回・シリーズの一覧に加え、シリーズ identity と操作に
 * 必要な分類ルール・録画ルールも返す。
 */
function stubApi(
  origin: Recording,
  upcoming: ProgramSearchMatch[],
  series: Recording[],
  originStatus = 200,
  labelRules: LabelRule[] = [],
  rules: Rule[] = [],
) {
  const requested: string[] = []
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    requested.push(`${url.pathname}?${url.searchParams.toString()}`)
    if (url.pathname === `/api/recordings/${origin.id}/upcoming`) {
      return Promise.resolve(jsonResponse(upcoming))
    }
    if (url.pathname === `/api/recordings/${origin.id}`) {
      return Promise.resolve(jsonResponse(origin, originStatus))
    }
    if (url.pathname === '/api/recordings') {
      expect(url.searchParams.get('seriesOf')).toBe(String(origin.id))
      return Promise.resolve(jsonResponse(series))
    }
    if (url.pathname === '/api/label-rules') return Promise.resolve(jsonResponse(labelRules))
    if (url.pathname === '/api/rules') return Promise.resolve(jsonResponse(rules))
    if (url.pathname === '/api/label-rule-value-key') {
      return Promise.resolve(jsonResponse({ valueKey: url.searchParams.get('value') ?? '' }))
    }
    throw new Error(`unexpected fetch: ${url.pathname}`)
  }) as unknown as typeof fetch
  return { requested }
}

/** originFetches は起点の単体 GET（`/api/recordings/5`）の呼び出し回数。 */
const originFetches = (requested: string[]) =>
  requested.filter((r) => r.startsWith('/api/recordings/5?')).length

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

describe('SeriesHubPage', () => {
  it('実効シリーズを見出しに出し、次回を site ごとに畳んで 1 行にする', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話' })
    stubApi(
      origin,
      [
        // 同じ放送が 2 拠点の EPG にある（同じ programId / 時刻）。
        program({ site: 'default', programId: 11 }),
        program({ site: 'other', programId: 11 }),
        program({ startAt: '2026-10-07T12:00:00Z', programId: 22, name: 'アニメ　作品X　第3話' }),
      ],
      [origin, recording({ id: 6, title: 'アニメ　作品X　第2話' })],
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    expect(await screen.findByRole('heading', { level: 2, name: '作品X' })).toBeInTheDocument()

    const upcoming = await screen.findByRole('region', { name: '次回' })
    const first = await within(upcoming).findByText(
      (_, el) => el?.textContent === 'アニメ　作品X　第2話',
    )
    const row = first.closest('li')
    expect(row).not.toBeNull()
    expect(within(row as HTMLElement).getByText('default')).toBeInTheDocument()
    expect(within(row as HTMLElement).getByText('other')).toBeInTheDocument()
    expect(within(upcoming).getAllByRole('listitem')).toHaveLength(2)
    expect(
      within(upcoming).getByText((_, el) => el?.textContent === 'アニメ　作品X　第3話'),
    ).toBeInTheDocument()
  })

  it('シリーズの録画を共有 RecordingRow で並べ、行から詳細へ辿れる', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', sizeBytes: 100 })
    stubApi(
      origin,
      [],
      [origin, recording({ id: 6, title: 'アニメ　作品X　第2話', sizeBytes: 200 })],
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const list = await screen.findByRole('region', { name: 'このシリーズの録画' })
    const link = await within(list).findByRole('link', { name: 'アニメ　作品X　第2話' })
    expect(link).toHaveAttribute('href', '/recordings/6')
    expect(screen.queryByRole('region', { name: '次回' })).not.toBeInTheDocument()
  })

  it('upcoming が 500 なら次回の節にエラーが出る', async () => {
    const origin = recording({ id: 5 })
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/recordings/5/upcoming') {
        return Promise.resolve(jsonResponse({ error: 'boom' }, 500))
      }
      if (url.pathname === '/api/recordings/5') return Promise.resolve(jsonResponse(origin))
      return Promise.resolve(jsonResponse([]))
    }) as unknown as typeof fetch

    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const section = await screen.findByRole('region', { name: '次回' })
    expect(await within(section).findByText('次回の取得に失敗しました')).toBeInTheDocument()
  })

  it('起点の録画が無ければエラーを出す', async () => {
    const origin = recording({ id: 5 })
    stubApi(origin, [], [])
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/recordings/5') {
        return Promise.resolve(jsonResponse({ error: 'recording not found' }, 404))
      }
      return Promise.resolve(jsonResponse([]))
    }) as unknown as typeof fetch

    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await waitFor(() => expect(screen.getByText('録画が見つかりません')).toBeInTheDocument())
  })

  it('起点が purge 済みでも一覧と次回を出し、戻る先を一覧にする', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話' })
    const { requested } = stubApi(
      origin,
      [program({ programId: 11, name: 'アニメ　作品X　第3話' })],
      [
        recording({ id: 6, title: 'アニメ　作品X　第2話' }),
        recording({ id: 4, title: 'アニメ　作品X　第1話' }),
      ],
      404,
    )
    const { router } = renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(originFetches(requested)).toBe(1)
    expect(await screen.findByRole('heading', { level: 2, name: '作品X' })).toBeInTheDocument()
    expect(await screen.findByRole('region', { name: '次回' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '戻る' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/recordings'))
  })

  it('起点が purge 済みで一覧が空でも、次回は表示する', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話' })
    stubApi(origin, [program({ programId: 11, name: 'アニメ　作品X　第2話' })], [], 404)
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    expect(await screen.findByRole('region', { name: '次回' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { level: 2, name: 'アニメ　作品X　第2話' })).not.toBeInTheDocument()
  })

  it('起点が purge 済みで一覧も次回も空ならエラーを出す', async () => {
    const origin = recording({ id: 5 })
    stubApi(origin, [], [], 404)
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await waitFor(() => expect(screen.getByText('録画が見つかりません')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: 'このシリーズの録画' })).not.toBeInTheDocument()
  })

  it('起点が purge 済みで一覧が 500 なら一覧の節にエラーを出す', async () => {
    const origin = recording({ id: 5 })
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/recordings/5/upcoming') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/recordings/5') return Promise.resolve(jsonResponse(origin, 404))
      if (url.pathname === '/api/recordings') return Promise.resolve(jsonResponse({}, 500))
      return Promise.resolve(jsonResponse([]))
    }) as unknown as typeof fetch
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const list = await screen.findByRole('region', { name: 'このシリーズの録画' })
    expect(await within(list).findByText('録画の取得に失敗しました')).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /再試行/ })).toBeInTheDocument()
    expect(screen.queryByText('録画が見つかりません')).not.toBeInTheDocument()
  })

  it('起点の取得が 500 なら purged と見なさずエラーを出す', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話' })
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { requested } = stubApi(
      origin,
      [],
      [recording({ id: 6, title: 'アニメ　作品X　第2話' })],
      500,
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await vi.advanceTimersByTimeAsync(10_000)
    await waitFor(() => expect(screen.getByText('録画が見つかりません')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: 'このシリーズの録画' })).not.toBeInTheDocument()
    expect(originFetches(requested)).toBe(4)
  })

  it('手動棚では実効シリーズと手動バッジ、自動キーの別名を表示する', async () => {
    const origin = recording({ id: 5, series: '日本史', seriesKey: 'NHK高校講座' })
    stubApi(
      origin,
      [],
      [
        origin,
        recording({ id: 6, series: '日本史', seriesKey: 'NHK高校講座 数学I', startAt: '2026-09-02T12:00:00Z' }),
      ],
      200,
      [labelRule({ id: 1, keyword: '日本史', value: '日本史', valueKey: '日本史' })],
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    expect(await screen.findByRole('heading', { level: 2, name: '日本史' })).toBeInTheDocument()
    expect(within(screen.getByRole('region', { name: 'シリーズ情報' })).getByText('手動')).toBeInTheDocument()
    expect(screen.getByText('自動: NHK高校講座 数学I（ほか 1）')).toBeInTheDocument()
  })

  it('最新の録画が recording/failed でも、再生できる最新話へリンクする', async () => {
    const oldPlayable = recording({
      id: 4,
      title: '作品X 第1話',
      startAt: '2026-09-01T12:00:00Z',
      sizeBytes: 100,
    })
    const latestRecording = recording({
      id: 6,
      title: '作品X 第3話',
      startAt: '2026-09-03T12:00:00Z',
      status: 'recording',
    })
    const failed = recording({
      id: 5,
      title: '作品X 第2話',
      startAt: '2026-09-02T12:00:00Z',
      status: 'failed',
    })
    stubApi(latestRecording, [], [latestRecording, failed, oldPlayable])
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/6/series'],
    })

    const play = await screen.findByRole('link', { name: '最新話を再生' })
    expect(play).toHaveAttribute('href', '/recordings/4')
    expect(screen.queryByText('続きから')).not.toBeInTheDocument()
  })

  it('現行の録画ルールがあればルールへの導線を表示する', async () => {
    const origin = recording({ id: 5, ruleId: 7, sizeBytes: 100 })
    stubApi(origin, [], [origin], 200, [], [rule({ id: 7, name: '夜のニュース' })])
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const link = await screen.findByRole('link', { name: 'ルール「夜のニュース」で毎回録画中' })
    expect(link).toHaveAttribute('href', '/search?ruleId=7')
    expect(screen.queryByText('毎回録画する')).not.toBeInTheDocument()
  })

  it('現行ルールが無ければ番組名と起点サービスを検索条件へ渡す', async () => {
    const origin = recording({ id: 5, series: '作品X', seriesKey: '作品X', sizeBytes: 100 })
    stubApi(origin, [], [origin])
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const link = await screen.findByRole('link', { name: '毎回録画する' })
    const url = new URL(link.getAttribute('href') ?? '', 'http://localhost')
    const condition = JSON.parse(url.searchParams.get('cond') ?? '{}') as {
      textMatches?: unknown[]
      services?: unknown[]
    }
    expect(condition.textMatches).toEqual([
      { target: 'name', mode: 'keyword', value: '作品X' },
    ])
    expect(condition.services).toEqual([{ networkId: 32678, serviceId: 5168 }])
  })

  it('新しい順と古い順の切り替えを API の order に反映する', async () => {
    const origin = recording({ id: 5 })
    const { requested } = stubApi(origin, [], [origin])
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await screen.findByRole('region', { name: 'このシリーズの録画' })
    expect(requested.some((request) => request.includes('order=desc'))).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '古い順' }))
    await waitFor(() => expect(requested.some((request) => request.includes('order=asc'))).toBe(true))
  })

  it('overflow からシリーズ軸の分類フォームを開く', async () => {
    const origin = recording({ id: 5, series: '作品X' })
    stubApi(origin, [], [origin])
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    fireEvent.click(await screen.findByRole('button', { name: 'シリーズのその他の操作' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: '分類を直す（割る・指定する）' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('textbox', { name: '棚のキー' })).toHaveValue('作品X')
  })

  it('履歴があれば戻り、直接開いた場合は起点の詳細へ戻す', async () => {
    const origin = recording({ id: 5 })
    stubApi(origin, [], [origin])
    const { router } = renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings', '/recordings/5/series'],
    })

    await screen.findByRole('heading', { level: 2, name: '作品X' })
    fireEvent.click(screen.getByRole('button', { name: '戻る' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/recordings'))
  })

  it('直接開いた purged でないハブは起点の詳細へ戻す', async () => {
    const origin = recording({ id: 5 })
    stubApi(origin, [], [origin])
    const { router } = renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await screen.findByRole('heading', { level: 2, name: '作品X' })
    fireEvent.click(screen.getByRole('button', { name: '戻る' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/recordings/5'))
  })
})
