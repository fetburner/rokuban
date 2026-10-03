import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { LabelRule, RecordingShelf } from '@/api/generated'
import { SeriesPage } from '@/pages/series'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubApi(shelves: RecordingShelf[], rules: LabelRule[] = []) {
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/recording-shelves' && method === 'GET') {
      return Promise.resolve(jsonResponse(shelves))
    }
    if (url.pathname === '/api/label-rules' && method === 'GET') {
      return Promise.resolve(jsonResponse(rules))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch
}

function shelf(
  value: string | null,
  title: string,
  count: number,
  playableCount: number,
  latestStartAt: string,
  representativeId = 1,
): RecordingShelf {
  return {
    ...(value === null ? {} : { value }),
    title,
    count,
    playableCount,
    unwatchedCount: 0,
    latestStartAt,
    representativeId,
  }
}

const shelves: RecordingShelf[] = [
  shelf('NHK高校講座', 'NHK高校講座　日本史　第1回', 120, 100, '2026-01-03T00:00:00Z', 11),
  shelf('作品X', 'アニメ　作品X　第2話', 5, 0, '2026-01-02T00:00:00Z', 12),
  shelf('単発', '単発の特番', 1, 1, '2026-01-04T00:00:00Z', 13),
  shelf(null, '【特集】', 2, 0, '2026-01-05T00:00:00Z', 14),
]

const rule: LabelRule = {
  id: 1,
  key: 'series',
  keyword: '日本史',
  value: 'NHK高校講座',
  priority: 5,
  valueKey: 'NHK高校講座',
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
}

afterEach(() => {
  vi.restoreAllMocks()
  localStorage.clear()
})

describe('SeriesPage', () => {
  it('件数 1 と見られる件数 0 の棚も表示し、NULL の棚だけを除外する', async () => {
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    expect(await within(content).findByText('単発')).toBeInTheDocument()
    expect(await within(content).findByText('作品X')).toBeInTheDocument()
    expect(within(content).getAllByTestId('series-shelf-meta').map((meta) => meta.textContent)).toContain(
      '見られる 0 件 · 1/2(金)',
    )
    expect(within(content).queryByText('【特集】')).not.toBeInTheDocument()
  })

  it('タイル全体を代表録画の番組ハブへリンクする', async () => {
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    expect(within(content).getByRole('link', { name: 'NHK高校講座のシリーズ' })).toHaveAttribute(
      'href',
      '/recordings/11/series',
    )
  })

  it('既定は新着順で、件数順・名前順へ切り替えられる（先頭が 3 通りとも異なる）', async () => {
    const user = userEvent.setup()
    // 名前順の先頭 = アニメA、件数順の先頭 = ドラマB、新着順の先頭 = バラエティC。
    stubApi([
      shelf('ドラマB', 'ドラマ　B　第1話', 50, 50, '2026-01-02T00:00:00Z', 21),
      shelf('バラエティC', 'バラエティ　C', 10, 10, '2026-01-09T00:00:00Z', 22),
      shelf('アニメA', 'アニメ　A　第1話', 3, 3, '2026-01-01T00:00:00Z', 23),
    ])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    await within(content).findByRole('link', { name: 'アニメAのシリーズ' })
    const order = () =>
      within(content)
        .getAllByRole('link', { name: /のシリーズ$/ })
        .map((link) => link.getAttribute('href'))
    expect(order()).toEqual(['/recordings/22/series', '/recordings/21/series', '/recordings/23/series'])

    await user.selectOptions(screen.getByLabelText('シリーズの並び順'), 'count')
    expect(order()).toEqual(['/recordings/21/series', '/recordings/22/series', '/recordings/23/series'])

    await user.selectOptions(screen.getByLabelText('シリーズの並び順'), 'name')
    expect(order()).toEqual(['/recordings/23/series', '/recordings/21/series', '/recordings/22/series'])
  })

  it('分類ルールの valueKey と一致する棚にだけ手動の札を付ける', async () => {
    stubApi(shelves, [rule])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    const manual = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    expect(within(manual).getByText('手動')).toBeInTheDocument()
    expect(within(content).getByRole('link', { name: '作品Xのシリーズ' })).not.toHaveTextContent('手動')
  })

  it('valueKey が一致する棚に札を付け、名前に分類ルールの生の値を出さない', async () => {
    // 値 `NHK高校講座 数学I` は正規化で棚 `NHK高校講座` に落ちる（value !== valueKey）。
    stubApi(shelves, [{ ...rule, value: 'NHK高校講座 数学I', valueKey: 'NHK高校講座' }])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    const manual = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    expect(within(manual).getByText('手動')).toBeInTheDocument()
    expect(within(content).queryByText(/数学I/)).not.toBeInTheDocument()
    expect(within(content).getByRole('link', { name: '作品Xのシリーズ' })).not.toHaveTextContent('手動')
  })

  it('棚の value と等しいだけで valueKey が違うルールには札を付けない', async () => {
    stubApi(shelves, [{ ...rule, value: 'NHK高校講座', valueKey: '別のキー' }])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    const link = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    // ルールの取得が終わってから否定を確かめる。
    await waitFor(() =>
      expect(globalThis.fetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/label-rules'),
        expect.anything(),
      ),
    )
    expect(link).not.toHaveTextContent('手動')
  })

  it('録画一覧と同じ localStorage キーでカード表示を保存する', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    await within(content).findByRole('link', { name: '単発のシリーズ' })
    await user.click(screen.getByRole('button', { name: 'カード表示' }))
    expect(localStorage.getItem('rokuban:recordings:view')).toBe('card')
  })

  it('シリーズ名と代表タイトルで絞り込む', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('bounded-page-content')
    await within(content).findByRole('link', { name: '単発のシリーズ' })
    await user.type(screen.getByLabelText('シリーズを絞り込む'), '作品X')
    expect(within(content).getByText('作品X')).toBeInTheDocument()
    expect(within(content).queryByText('単発')).not.toBeInTheDocument()
  })
})
