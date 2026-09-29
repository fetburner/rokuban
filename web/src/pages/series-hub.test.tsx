import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { ProgramSearchMatch, Recording } from '@/api/generated'
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

/**
 * stubApi は起点の録画・次回・シリーズの一覧を返す。
 *
 * `originStatus` は起点の単体 GET の状態。purge 済みの tombstone を表す 404 も、
 * 5xx も、同じ「単体 GET が返らない」形になるので、ここで切り替える。
 */
function stubApi(
  origin: Recording,
  upcoming: ProgramSearchMatch[],
  series: Recording[],
  originStatus = 200,
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
      // ハブの一覧は `?seriesOf=` が付いている。
      expect(url.searchParams.get('seriesOf')).toBe(String(origin.id))
      return Promise.resolve(jsonResponse(series))
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
  it('見出しに起点の生のタイトルを出し、次回を site ごとに畳んで 1 行にする', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', series: '作品X' })
    stubApi(
      origin,
      [
        // 同じ放送が 2 拠点の EPG にある（同じ programId / 時刻）。
        program({ site: 'default', programId: 11 }),
        program({ site: 'other', programId: 11 }),
        program({ startAt: '2026-10-07T12:00:00Z', programId: 22, name: 'アニメ　作品X　第3話' }),
      ],
      [origin, recording({ id: 6, title: 'アニメ　作品X　第2話', series: '作品X' })],
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    // 見出しは正規化キー（作品X）ではなく、起点の録画の生のタイトル。
    const heading = await screen.findByRole('heading', { level: 2 })
    expect(heading.textContent).toBe('アニメ　作品X　第1話')

    const upcoming = await screen.findByRole('region', { name: '次回' })
    // 同じ放送の 2 行は 1 行にまとまり、site はチップで出る。
    const first = await within(upcoming).findByText(
      (_, el) => el?.textContent === 'アニメ　作品X　第2話',
    )
    const row = first.closest('li')
    expect(row).not.toBeNull()
    expect(within(row as HTMLElement).getByText('default')).toBeInTheDocument()
    expect(within(row as HTMLElement).getByText('other')).toBeInTheDocument()
    expect(within(upcoming).getAllByRole('listitem')).toHaveLength(2)
    // 別の時刻の回は別の行。
    expect(
      within(upcoming).getByText((_, el) => el?.textContent === 'アニメ　作品X　第3話'),
    ).toBeInTheDocument()
  })

  it('シリーズの録画を並べ、行から詳細へ辿れる', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', series: '作品X', sizeBytes: 100 })
    stubApi(
      origin,
      [],
      [
        origin,
        recording({ id: 6, title: 'アニメ　作品X　第2話', series: '作品X', sizeBytes: 200 }),
      ],
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const list = await screen.findByRole('region', { name: 'このシリーズの録画' })
    const link = await within(list).findByRole('link', { name: 'アニメ　作品X　第2話' })
    expect(link).toHaveAttribute('href', '/recordings/6')
    // 次回が空なら節ごと出さない（言うことが無い見出しを置かない）。
    expect(screen.queryByRole('region', { name: '次回' })).not.toBeInTheDocument()
  })

  it('upcoming が 500 なら次回の節にエラーが出る', async () => {
    const origin = recording({ id: 5, series: '作品X' })
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
    const origin = recording({ id: 5, series: '作品X' })
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

  // 起点の単体 GET は purged の tombstone を除く（`queryRecordingByID` の契約）が、
  // 一覧（`?seriesOf=`）と次回は行が残るのでシリーズを返す。この差で purged を
  // 見分ける。**このテストは、404 で画面全体をエラーにしていた実装で落ちる。**
  it('起点が purge 済み（単体 GET が 404）でも、一覧と次回を出す', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', series: '作品X' })
    const { requested } = stubApi(
      origin,
      [program({ programId: 11, name: 'アニメ　作品X　第3話' })],
      [
        recording({ id: 6, title: 'アニメ　作品X　第2話', series: '作品X' }),
        recording({ id: 4, title: 'アニメ　作品X　第1話', series: '作品X' }),
      ],
      404,
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })
    // 404 は再試行しない（既定なら backoff 1+2+4 秒で計 4 回になる）。
    await vi.advanceTimersByTimeAsync(10_000)
    expect(originFetches(requested)).toBe(1)

    // 見出しは起点ではなく、一覧の先頭（最も新しい回）の生のタイトル。
    const heading = await screen.findByRole('heading', { level: 2 })
    expect(heading.textContent).toBe('アニメ　作品X　第2話')

    const list = await screen.findByRole('region', { name: 'このシリーズの録画' })
    expect(await within(list).findByRole('link', { name: 'アニメ　作品X　第1話' })).toHaveAttribute(
      'href',
      '/recordings/4',
    )
    expect(
      await screen.findByRole('region', { name: '次回' }),
    ).toBeInTheDocument()
    // 戻る先は起点の詳細ではない（そちらも 404 になる）。
    expect(screen.getByRole('link', { name: '戻る' })).toHaveAttribute('href', '/recordings')
  })

  // 一覧が空でも次回があれば、ハブは開く（見出しは次回の生のタイトル）。
  it('起点が purge 済みで一覧が空でも、次回があれば見出しを次回で出す', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', series: '作品X' })
    stubApi(origin, [program({ programId: 11, name: 'アニメ　作品X　第2話' })], [], 404)
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    const heading = await screen.findByRole('heading', { level: 2 })
    expect(heading.textContent).toBe('アニメ　作品X　第2話')
    await screen.findByRole('region', { name: '次回' })
  })

  // 一覧も次回も 0 件なら、purged の起点と存在しない id を区別できない。
  // このときだけ「見つかりません」に落とす（受け入れた限界）。
  it('起点が purge 済みで一覧も次回も空ならエラーを出す', async () => {
    const origin = recording({ id: 5, series: '作品X' })
    stubApi(origin, [], [], 404)
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    await waitFor(() => expect(screen.getByText('録画が見つかりません')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: 'このシリーズの録画' })).not.toBeInTheDocument()
  })

  // 一覧が 500 のとき「0 件」は判断材料にならない（行が残っているかもしれない）。
  // `nothingToShow` の `!isError` を外すと「見つかりません」に落ちる。
  it('起点が purge 済みで一覧が 500・次回が空なら、一覧の節にエラーを出す', async () => {
    const origin = recording({ id: 5, series: '作品X' })
    globalThis.fetch = vi.fn((input: string | URL | Request) => {
      const url = new URL(String(input), 'http://localhost')
      if (url.pathname === '/api/recordings/5/upcoming') return Promise.resolve(jsonResponse([]))
      if (url.pathname === '/api/recordings/5') return Promise.resolve(jsonResponse(origin, 404))
      if (url.pathname === '/api/recordings') return Promise.resolve(jsonResponse({}, 500))
      throw new Error(`unexpected fetch: ${url.pathname}`)
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

  // 404 以外のエラーは purged と見なさない。一覧が返っていても開かない。
  it('起点の取得が 500 なら purged と見なさずエラーを出す', async () => {
    const origin = recording({ id: 5, title: 'アニメ　作品X　第1話', series: '作品X' })
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { requested } = stubApi(
      origin,
      [],
      [recording({ id: 6, title: 'アニメ　作品X　第2話', series: '作品X' })],
      500,
    )
    renderInRouter(<SeriesHubPage />, {
      path: '/recordings/$id/series',
      initialEntries: ['/recordings/5/series'],
    })

    // 404 以外は起点クエリの再試行（3 回。テストの QueryClient の retry: false より
    // クエリ側の設定が勝つ）を経てから出る。backoff（1+2+4 秒）は偽の時計で進める。
    await vi.advanceTimersByTimeAsync(10_000)
    await waitFor(() => expect(screen.getByText('録画が見つかりません')).toBeInTheDocument())
    expect(screen.queryByRole('region', { name: 'このシリーズの録画' })).not.toBeInTheDocument()
    // 初回 + 再試行 3 回。retry の行を消すと 1 回になる。
    expect(originFetches(requested)).toBe(4)
  })
})
