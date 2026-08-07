import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import { RecordingFilters } from '@/components/recording-filters'
import { emptyRecordingsSearch, type RecordingsPageSearch } from '@/lib/recording-search'

function service(overrides: Partial<Service> = {}): Service {
  return {
    networkId: 1,
    serviceId: 1024,
    name: 'ＮＨＫ総合',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
    ...overrides,
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * renderFilters は `RecordingFilters` を「呼び出し側が search を持つ」形で描く。
 * 実際の `pages/recordings.tsx` と同じく、状態はここ（テスト側）に置き、
 * onChange の結果を state に書き戻す --- コンポーネント自身が状態を持たない
 * ことを固定する。
 */
function renderFilters(
  initial: RecordingsPageSearch = emptyRecordingsSearch(),
  services: Service[] = [service()],
) {
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites/default/services') return Promise.resolve(jsonResponse(services))
    return Promise.resolve(new Response('not found', { status: 404 }))
  }) as unknown as typeof fetch

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })
  const onChangeCalls: RecordingsPageSearch[] = []
  let current = initial

  function Harness() {
    const [search, setSearch] = useState(initial)
    return (
      <RecordingFilters
        search={search}
        onChange={(updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => {
          setSearch((prev: RecordingsPageSearch) => {
            const next = updater(prev)
            current = next
            onChangeCalls.push(next)
            return next
          })
        }}
      />
    )
  }

  render(
    <QueryClientProvider client={queryClient}>
      <Harness />
    </QueryClientProvider>,
  )

  return { onChangeCalls, getCurrent: () => current }
}

describe('RecordingFilters キーワード', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('300ms の debounce を挟んで onChange を呼ぶ（1 文字ごとには呼ばない）', () => {
    const { onChangeCalls } = renderFilters()
    const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })

    fireEvent.change(input, { target: { value: 'ニ' } })
    vi.advanceTimersByTime(299)
    expect(onChangeCalls).toHaveLength(0)

    vi.advanceTimersByTime(2)
    expect(onChangeCalls).toHaveLength(1)
    expect(onChangeCalls[0].q).toBe('ニ')
  })

  it('debounce 中に続けて入力すると、最後の値だけが 1 回反映される', () => {
    const { onChangeCalls } = renderFilters()
    const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })

    fireEvent.change(input, { target: { value: 'ニ' } })
    vi.advanceTimersByTime(150)
    fireEvent.change(input, { target: { value: 'ニュ' } })
    vi.advanceTimersByTime(150)
    // 最初のタイマーは打ち切られているので、まだ 300ms 経っていない
    expect(onChangeCalls).toHaveLength(0)

    vi.advanceTimersByTime(150)
    expect(onChangeCalls).toHaveLength(1)
    expect(onChangeCalls[0].q).toBe('ニュ')
  })

  it('空文字列にすると q を undefined にする（空文字列を送らない）', () => {
    const { onChangeCalls } = renderFilters({ q: 'ニュース' })
    const input = screen.getByRole('searchbox', { name: '番組名・説明で検索' })

    fireEvent.change(input, { target: { value: '' } })
    vi.advanceTimersByTime(300)

    expect(onChangeCalls).toHaveLength(1)
    expect(onChangeCalls[0].q).toBeUndefined()
  })
})

describe('RecordingFilters チップ', () => {
  it('条件が無ければチップ行が出ない', () => {
    renderFilters()
    expect(screen.queryByText('条件をクリア')).not.toBeInTheDocument()
  })

  it('適用中の条件がチップで出て、押すとその条件だけ外れる', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters({ genre: [0, 1], status: 'failed' })

    expect(screen.getByText('ジャンル: ニュース・報道')).toBeInTheDocument()
    expect(screen.getByText('ジャンル: スポーツ')).toBeInTheDocument()
    expect(screen.getByText('状態: 失敗')).toBeInTheDocument()

    await user.click(screen.getByText('ジャンル: ニュース・報道'))

    expect(onChangeCalls).toHaveLength(1)
    expect(onChangeCalls[0]).toEqual({ genre: [1], status: 'failed' })
    // 外した条件のチップは消える。他の条件は残る
    expect(screen.queryByText('ジャンル: ニュース・報道')).not.toBeInTheDocument()
    expect(screen.getByText('ジャンル: スポーツ')).toBeInTheDocument()
    expect(screen.getByText('状態: 失敗')).toBeInTheDocument()
  })

  it('「条件をクリア」で並び順以外の条件を全部外す', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters({ genre: [0], status: 'failed', order: 'asc' })

    await user.click(screen.getByText('条件をクリア'))

    expect(onChangeCalls).toHaveLength(1)
    expect(onChangeCalls[0]).toEqual({ order: 'asc' })
  })
})

describe('RecordingFilters 絞り込みパネル', () => {
  it('状態チップを選ぶと status が立ち、「問わない」を選ぶと外れる', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const statusGroup = within(panel).getByRole('group', { name: '状態' })

    await user.click(within(statusGroup).getByRole('button', { name: '失敗' }))
    expect(onChangeCalls.at(-1)?.status).toBe('failed')

    await user.click(within(statusGroup).getByRole('button', { name: '問わない' }))
    expect(onChangeCalls.at(-1)?.status).toBeUndefined()
  })

  it('種別チップ（ルール・手動）が source を切り替える', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    await user.click(within(panel).getByRole('button', { name: '手動' }))
    expect(onChangeCalls.at(-1)?.source).toBe('manual')
  })

  it('ジャンルチップは複数選択で、選択中は同じチップを押すと外れる', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    await user.click(within(panel).getByRole('button', { name: 'ドラマ' }))
    expect(getCurrent().genre).toEqual([3])

    await user.click(within(panel).getByRole('button', { name: 'ドラマ' }))
    expect(getCurrent().genre).toBeUndefined()
  })

  it('チャンネルは ChannelPicker を経由して serviceId に反映される', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters(emptyRecordingsSearch(), [service()])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    await user.click(within(panel).getByRole('button', { name: /チャンネル/ }))
    const channelDialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    await user.click(within(channelDialog).getByRole('button', { name: /ＮＨＫ総合/ }))

    await waitFor(() => expect(getCurrent().serviceId).toEqual([1024]))
  })
})
