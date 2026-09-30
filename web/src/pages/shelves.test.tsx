import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { LabelRule, LabelRuleInput, RecordingShelf } from '@/api/generated'
import { ShelvesPage } from '@/pages/shelves'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** stubApi は棚と分類ルールの GET/POST を返す。 */
function stubApi(shelves: RecordingShelf[], rules: LabelRule[] = []) {
  const posted: LabelRuleInput[] = []
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/recording-shelves' && method === 'GET') {
      return Promise.resolve(jsonResponse(shelves))
    }
    if (url.pathname === '/api/label-rules' && method === 'GET') {
      return Promise.resolve(jsonResponse(rules))
    }
    if (url.pathname === '/api/label-rule-value-key' && method === 'GET') {
      // サーバーの series_key の代役: 最初の空白で切る（実 DB では
      // internal/api の TestLabelRule_ValueKeyIsTheTruncatedShelfKey が測る）。
      return Promise.resolve(
        jsonResponse({ valueKey: (url.searchParams.get('value') ?? '').split(' ')[0] }),
      )
    }
    if (url.pathname === '/api/label-rules' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as LabelRuleInput
      posted.push(body)
      return Promise.resolve(
        jsonResponse(
          {
            ...body,
            id: 99,
            key: 'series',
            createdAt: '2026-09-29T00:00:00Z',
            updatedAt: '2026-09-29T00:00:00Z',
          },
          201,
        ),
      )
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch
  return { posted }
}

const shelves: RecordingShelf[] = [
  { value: 'NHK高校講座', title: 'NHK高校講座　日本史　第1回', count: 120, representativeId: 11 },
  { value: '作品X', title: 'アニメ　作品X　第2話', count: 5, representativeId: 12 },
  { value: '単発', title: '単発の特番', count: 1, representativeId: 13 },
  { title: '【特集】', count: 2, representativeId: 14 },
]

afterEach(() => {
  vi.restoreAllMocks()
})

describe('ShelvesPage', () => {
  it('件数によらず棚を並べ、値の無い棚は表示しない', async () => {
    stubApi(shelves)
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    const content = await screen.findByTestId('bounded-page-content')
    // 過剰併合の棚が見出し（代表の生タイトル）で見える。
    // 全角空白を含むので、testing-library の空白正規化に頼らず生の文字列で引く。
    expect(
      await within(content).findByText((_, el) => el?.textContent === 'NHK高校講座　日本史　第1回'),
    ).toBeInTheDocument()
    // 件数 1 の棚にも行がある。
    await waitFor(() => {
      expect(within(content).getByText('単発の特番')).toBeInTheDocument()
    })
    // 値が NULL の棚には番組ハブの起点がないため行を作らない。
    expect(within(content).queryByText('【特集】')).not.toBeInTheDocument()
  })

  it('「この棚を割る・指定する」で棚のキーがフォームに入る', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    const content = await screen.findByTestId('bounded-page-content')
    const row = (
      await within(content).findByText(
        (_, el) => el?.textContent === 'NHK高校講座　日本史　第1回',
      )
    ).closest('li')
    expect(row).not.toBeNull()
    await user.click(
      within(row as HTMLElement).getByRole('button', { name: 'この棚を割る・指定する' }),
    )

    const dialog = await screen.findByRole('dialog')
    // value は見出し（生タイトル）ではなく棚のキー。
    expect(within(dialog).getByLabelText('棚のキー')).toHaveValue('NHK高校講座')
    expect(within(dialog).getByLabelText('キーワード')).toHaveValue('')
  })

  it('棚の行は代表録画の番組ハブへリンクする', async () => {
    stubApi(shelves)
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    const content = await screen.findByTestId('bounded-page-content')
    const row = (
      await within(content).findByText(
        (_, el) => el?.textContent === 'NHK高校講座　日本史　第1回',
      )
    ).closest('li')
    expect(row).not.toBeNull()
    expect(within(row as HTMLElement).getByRole('link', { name: 'NHK高校講座　日本史　第1回のシリーズ' })).toHaveAttribute(
      'href',
      '/recordings/11/series',
    )
  })

  it('キーワードを入力すると棚のキーが追従し、POST に両方が載る', async () => {
    const user = userEvent.setup()
    const { posted } = stubApi(shelves)
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    await screen.findByTestId('bounded-page-content')
    await user.click(await screen.findByRole('button', { name: '分類ルールを作成' }))

    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('キーワード'), '烏は主を選ばない')
    expect(within(dialog).getByLabelText('棚のキー')).toHaveValue('烏は主を選ばない')
    await user.click(within(dialog).getByRole('button', { name: '作成' }))

    await waitFor(() => expect(posted).toHaveLength(1))
    expect(posted[0]).toEqual({ keyword: '烏は主を選ばない', value: '烏は主を選ばない', priority: 0 })
  })

  it('分類ルールの一覧を勝者順に出す', async () => {
    stubApi(shelves, [
      {
        id: 1,
        key: 'series',
        keyword: '日本史',
        value: '日本史',
        priority: 5,
        valueKey: '日本史',
        createdAt: '2026-09-29T00:00:00Z',
        updatedAt: '2026-09-29T00:00:00Z',
      },
    ])
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    expect(await screen.findByText('「日本史」→ 日本史')).toBeInTheDocument()
  })

  it('値と実効の棚キーが食い違う分類ルールを一覧で明示する', async () => {
    stubApi(shelves, [
      {
        id: 2,
        key: 'series',
        keyword: '数学',
        value: 'NHK高校講座 数学I',
        priority: 0,
        valueKey: 'NHK高校講座',
        createdAt: '2026-09-29T00:00:00Z',
        updatedAt: '2026-09-29T00:00:00Z',
      },
    ])
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    expect(await screen.findByText('この値は棚キー NHK高校講座 として扱われます')).toBeInTheDocument()
  })

  it('フォームは入力中の値から得られる棚キーを、食い違うときだけ示す', async () => {
    const user = userEvent.setup()
    stubApi(shelves)
    renderInRouter(<ShelvesPage />, { path: '/shelves' })

    await screen.findByTestId('bounded-page-content')
    await user.click(await screen.findByRole('button', { name: '分類ルールを作成' }))
    const dialog = await screen.findByRole('dialog')

    await user.type(within(dialog).getByLabelText('棚のキー'), '作品X')
    // 一致しているうちは何も言わない（サーバー応答を待ってから否定を確かめる）。
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith(
      expect.stringContaining('/api/label-rule-value-key?value=%E4%BD%9C%E5%93%81X'),
      expect.anything(),
    ))
    expect(within(dialog).queryByRole('status')).not.toBeInTheDocument()

    await user.type(within(dialog).getByLabelText('棚のキー'), ' 第2期')
    expect(await within(dialog).findByRole('status')).toHaveTextContent(
      'この値は棚キー 作品X として扱われます',
    )
  })
})
