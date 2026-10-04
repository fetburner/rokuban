import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { LabelRule, LabelRuleInput, RecordingShelf } from '@/api/generated'
import { SeriesPage } from '@/pages/series'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function stubApi(shelves: RecordingShelf[], rules: LabelRule[] = []) {
  let labelRuleState = [...rules]
  const posted: LabelRuleInput[] = []
  const patched: { id: number; body: LabelRuleInput }[] = []
  const deleted: number[] = []
  let labelRulesReadCount = 0
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/recording-shelves' && method === 'GET') {
      return Promise.resolve(jsonResponse(shelves))
    }
    if (url.pathname === '/api/label-rules' && method === 'GET') {
      labelRulesReadCount += 1
      return Promise.resolve(jsonResponse(labelRuleState))
    }
    if (url.pathname === '/api/label-rules' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as LabelRuleInput
      posted.push(body)
      const created = labelRule(
        99,
        body.keyword,
        body.value,
        body.value.split(' ')[0] ?? body.value,
        body.priority,
      )
      labelRuleState = [...labelRuleState, created]
      return Promise.resolve(jsonResponse(created, 201))
    }
    if (url.pathname === '/api/label-rule-value-key' && method === 'GET') {
      return Promise.resolve(
        jsonResponse({ valueKey: (url.searchParams.get('value') ?? '').split(' ')[0] }),
      )
    }
    const match = /^\/api\/label-rules\/(\d+)$/.exec(url.pathname)
    if (match && method === 'PATCH') {
      const id = Number(match[1])
      const body = JSON.parse(String(init?.body)) as LabelRuleInput
      patched.push({ id, body })
      const updated = labelRule(id, body.keyword, body.value, body.value, body.priority)
      labelRuleState = labelRuleState.map((rule) => (rule.id === id ? updated : rule))
      return Promise.resolve(jsonResponse(updated))
    }
    if (match && method === 'DELETE') {
      const id = Number(match[1])
      deleted.push(id)
      labelRuleState = labelRuleState.filter((rule) => rule.id !== id)
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch
  return { posted, patched, deleted, labelRulesReadCount: () => labelRulesReadCount }
}

function labelRule(
  id: number,
  keyword: string,
  value: string,
  valueKey: string,
  priority = 0,
): LabelRule {
  return {
    id,
    key: 'series',
    keyword,
    value,
    priority,
    valueKey,
    createdAt: '2026-09-29T00:00:00Z',
    updatedAt: '2026-09-29T00:00:00Z',
  }
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

    const content = await screen.findByTestId('page-content')
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

    const content = await screen.findByTestId('page-content')
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

    const content = await screen.findByTestId('page-content')
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

    const content = await screen.findByTestId('page-content')
    const manual = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    expect(within(manual).getByText('手動')).toBeInTheDocument()
    expect(within(content).getByRole('link', { name: '作品Xのシリーズ' })).not.toHaveTextContent('手動')
  })

  it('valueKey が一致する棚に札を付け、名前に分類ルールの生の値を出さない', async () => {
    // 値 `NHK高校講座 数学I` は正規化で棚 `NHK高校講座` に落ちる（value !== valueKey）。
    stubApi(shelves, [{ ...rule, value: 'NHK高校講座 数学I', valueKey: 'NHK高校講座' }])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    const manual = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    const shelfList = within(content).getByRole('list', { name: 'シリーズ一覧' })
    expect(within(manual).getByText('手動')).toBeInTheDocument()
    expect(within(shelfList).queryByText(/数学I/)).not.toBeInTheDocument()
    expect(within(content).getByRole('link', { name: '作品Xのシリーズ' })).not.toHaveTextContent('手動')
  })

  it('棚の value と等しいだけで valueKey が違うルールには札を付けない', async () => {
    stubApi(shelves, [{ ...rule, value: 'NHK高校講座', valueKey: '別のキー' }])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
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

  it('分類ルールの管理を棚の後ろに置き、一覧を API の順で表示する', async () => {
    stubApi(shelves, [
      labelRule(1, '日本史', '日本史', '日本史', 5),
      labelRule(2, '数学', '数学', '数学'),
    ])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    const shelfList = await within(content).findByRole('list', { name: 'シリーズ一覧' })
    const manager = await within(content).findByRole('region', { name: 'シリーズ分類' })
    expect(
      shelfList.compareDocumentPosition(manager) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy()

    const first = within(manager).getByText('「日本史」→ 日本史')
    const second = within(manager).getByText('「数学」→ 数学')
    expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('作成後に一覧と同じ label_rules クエリが更新され、棚の手動バッジも付く', async () => {
    const user = userEvent.setup()
    const api = stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    const manager = await within(content).findByRole('region', { name: 'シリーズ分類' })
    await user.click(within(manager).getByRole('button', { name: '分類ルールを作成' }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('キーワード'), '作品X')
    expect(within(dialog).getByLabelText('棚のキー')).toHaveValue('作品X')
    await user.click(within(dialog).getByRole('button', { name: '作成' }))

    await waitFor(() => {
      expect(api.posted).toEqual([{ keyword: '作品X', value: '作品X', priority: 0 }])
      expect(within(manager).getByText('「作品X」→ 作品X')).toBeInTheDocument()
      expect(within(content).getByRole('link', { name: '作品Xのシリーズ' })).toHaveTextContent('手動')
    })
    expect(api.labelRulesReadCount()).toBeGreaterThanOrEqual(2)
  })

  it('分類ルールを作成すると棚のキーが追従し、入力中の実効キーを示す', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const manager = await screen.findByRole('region', { name: 'シリーズ分類' })
    await user.click(within(manager).getByRole('button', { name: '分類ルールを作成' }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('キーワード'), '烏は主を選ばない')
    expect(within(dialog).getByLabelText('棚のキー')).toHaveValue('烏は主を選ばない')

    await user.clear(within(dialog).getByLabelText('棚のキー'))
    await user.type(within(dialog).getByLabelText('棚のキー'), 'NHK高校講座 第2期')
    expect(await within(dialog).findByRole('status')).toHaveTextContent(
      'この値は棚キー NHK高校講座 として扱われます',
    )
  })

  it('分類ルールを編集すると PATCH に変更後の内容が飛ぶ', async () => {
    const user = userEvent.setup()
    const api = stubApi(shelves, [labelRule(7, '日本史', '日本史', '日本史', 5)])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const manager = await screen.findByRole('region', { name: 'シリーズ分類' })
    await within(manager).findByText('「日本史」→ 日本史')
    await user.click(within(manager).getByRole('button', { name: '編集' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: '分類ルールを編集' })).toBeInTheDocument()
    expect(within(dialog).getByLabelText('キーワード')).toHaveValue('日本史')
    await user.clear(within(dialog).getByLabelText('優先度'))
    await user.type(within(dialog).getByLabelText('優先度'), '9')
    await user.click(within(dialog).getByRole('button', { name: '保存' }))

    await waitFor(() => expect(api.patched).toEqual([
      { id: 7, body: { keyword: '日本史', value: '日本史', priority: 9 } },
    ]))
  })

  it('分類ルールは削除確認後に DELETE し、棚の手動バッジも更新する', async () => {
    const user = userEvent.setup()
    const api = stubApi(shelves, [rule])
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    const manager = await within(content).findByRole('region', { name: 'シリーズ分類' })
    const manual = await within(content).findByRole('link', { name: 'NHK高校講座のシリーズ' })
    expect(manual).toHaveTextContent('手動')

    await user.click(within(manager).getByRole('button', { name: '削除' }))
    expect(api.deleted).toEqual([])
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: '削除する' }))

    await waitFor(() => {
      expect(api.deleted).toEqual([1])
      expect(within(manager).getByText('分類ルールがありません')).toBeInTheDocument()
      expect(manual).not.toHaveTextContent('手動')
    })
  })

  it('録画一覧と同じ localStorage キーでカード表示を保存する', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    await within(content).findByRole('link', { name: '単発のシリーズ' })
    await user.click(screen.getByRole('button', { name: 'カード表示' }))
    expect(localStorage.getItem('rokuban:recordings:view')).toBe('card')
  })

  it('シリーズ名と代表タイトルで絞り込む', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<SeriesPage />, { path: '/series' })

    const content = await screen.findByTestId('page-content')
    await within(content).findByRole('link', { name: '単発のシリーズ' })
    await user.type(screen.getByLabelText('シリーズを絞り込む'), '作品X')
    expect(within(content).getByText('作品X')).toBeInTheDocument()
    expect(within(content).queryByText('単発')).not.toBeInTheDocument()
  })
})
