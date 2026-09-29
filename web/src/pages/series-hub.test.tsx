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

/** stubApi は起点の録画・次回・シリーズの一覧を返す。 */
function stubApi(origin: Recording, upcoming: ProgramSearchMatch[], series: Recording[]) {
  const requested: string[] = []
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    requested.push(`${url.pathname}?${url.searchParams.toString()}`)
    if (url.pathname === `/api/recordings/${origin.id}/upcoming`) {
      return Promise.resolve(jsonResponse(upcoming))
    }
    if (url.pathname === `/api/recordings/${origin.id}`) {
      return Promise.resolve(jsonResponse(origin))
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

afterEach(() => {
  vi.restoreAllMocks()
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
})
