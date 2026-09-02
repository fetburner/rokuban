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
    id: (overrides.networkId ?? 1) * 100_000 + (overrides.serviceId ?? 1024),
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
  otherSites: Record<string, Service[]> = {},
) {
  const servicesBySite = { default: services, ...otherSites }
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') {
      return Promise.resolve(jsonResponse(Object.keys(servicesBySite)))
    }
    const match = /^\/api\/sites\/([^/]+)\/services$/.exec(url.pathname)
    if (match && match[1] in servicesBySite) {
      return Promise.resolve(jsonResponse(servicesBySite[match[1] as keyof typeof servicesBySite]))
    }
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

  // **同じチャンネルを 2 サイトで受けていても選択肢は 1 つ**（identity は
  // `Service.id`）。site は別軸なので、site チップで絞る。
  it('全サイトで同じチャンネルは 1 つの選択肢になり、site は別軸で絞る', async () => {
    const user = userEvent.setup()
    // 同じ id を両サイトが持つ局（畳まれる）と、site2 にしか無い局
    // （**畳まれずに候補へ出る**）を混ぜる。後者が無いと「default の一覧しか
    // 見ていない」実装でも緑になる。
    const { getCurrent } = renderFilters(
      emptyRecordingsSearch(),
      [service()],
      { site2: [service(), service({ serviceId: 2048, name: 'site2 だけの局' })] },
    )

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    await user.click(within(panel).getByRole('button', { name: /チャンネル/ }))
    const channelDialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    // 両サイトにある局は合成 id が同じなので候補は 1 つに畳まれる。
    const shared = await within(channelDialog).findAllByRole('button', { name: /ＮＨＫ総合/ })
    expect(shared).toHaveLength(1)
    // site2 にしか無い局も候補に出る。
    expect(within(channelDialog).getByRole('button', { name: /site2 だけの局/ })).toBeInTheDocument()

    await user.click(shared[0])
    await waitFor(() => expect(getCurrent().service).toEqual([101024]))
  })

  // **両方向を見る。** 押して付くところだけ見ると、解除の分岐を no-op に
  // しても緑のまま通る。並びも固定する --- 選択履歴で順序が揺れると
  // 同じ選択でも URL / queryKey が変わる（`parseRecordingsSearch` が
  // 昇順に正準化しているのと同じ理由）。
  it('2 サイト構成では site チップで選択・解除でき、並びは昇順に揃う', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters(emptyRecordingsSearch(), [service()], {
      site2: [service()],
    })

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const siteGroup = within(panel).getByRole('group', { name: 'サイト' })

    // **押した順ではなく昇順に並ぶ。** default → site2 の順に押すと、
    // 追加を先頭に積む実装（[site, ...prev]）では ['site2', 'default'] に
    // なるので、この順序でだけ両者を区別できる。
    await user.click(within(siteGroup).getByRole('button', { name: 'default' }))
    await waitFor(() => expect(getCurrent().site).toEqual(['default']))
    await user.click(within(siteGroup).getByRole('button', { name: 'site2' }))
    await waitFor(() => expect(getCurrent().site).toEqual(['default', 'site2']))

    // もう一度押すと外れ、最後の 1 つを外すとキーごと消える。
    await user.click(within(siteGroup).getByRole('button', { name: 'default' }))
    await waitFor(() => expect(getCurrent().site).toEqual(['site2']))
    await user.click(within(siteGroup).getByRole('button', { name: 'site2' }))
    await waitFor(() => expect(getCurrent().site).toBeUndefined())
  })

  it('1 サイト構成では site の選択肢を出さない', async () => {
    const user = userEvent.setup()
    renderFilters(emptyRecordingsSearch(), [service()])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    expect(within(panel).queryByRole('group', { name: 'サイト' })).not.toBeInTheDocument()
  })

  // serviceId は network をまたぐと一意でない（BS 101 と CS 101 は実在する衝突）。
  // 組で選ぶので、片方だけを選択中にできる。
  it('同一 site・serviceId の別 network を独立して選択・解除できる', async () => {
    const user = userEvent.setup()
    const bs = service({ networkId: 4, serviceId: 101, name: '同名チャンネル', channelType: 'BS' })
    const cs = service({ networkId: 6, serviceId: 101, name: '同名チャンネル', channelType: 'CS' })
    const { getCurrent } = renderFilters({ service: [400101, 600101] }, [bs, cs])

    await user.click(await screen.findByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    expect(within(panel).getByRole('button', { name: 'チャンネル: 2 局を選択中' })).toBeInTheDocument()

    await user.click(within(panel).getByRole('button', { name: /チャンネル/ }))
    const channelDialog = await screen.findByRole('dialog', { name: 'チャンネル' })
    const options = within(channelDialog).getAllByRole('button', { name: /同名チャンネル/ })
    expect(options).toHaveLength(2)
    expect(options.map((option) => option.getAttribute('aria-pressed'))).toEqual(['true', 'true'])

    // 片方（CS 側）を解除すると、もう片方（BS 側）だけが残る。
    await user.click(options[1])
    await waitFor(() => expect(getCurrent().service).toEqual([400101]))
  })

  // from/to は純関数（isoToLocalDateTimeInput / localDateTimeInputToIso）は
  // 別途テスト済みだが、入力欄を実際に操作して search に乗る経路（genre /
  // serviceId には既にある）が無かったので足す。
  it('期間の入力欄を操作すると from/to が ISO 8601（UTC）で反映される', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    const fromInput = within(panel).getByLabelText('開始日時')
    fireEvent.change(fromInput, { target: { value: '2026-01-15T09:30' } })
    await waitFor(() => expect(getCurrent().from).toBeDefined())
    expect(new Date(getCurrent().from as string).getFullYear()).toBe(2026)

    const toInput = within(panel).getByLabelText('終了日時')
    fireEvent.change(toInput, { target: { value: '2026-01-16T00:00' } })
    await waitFor(() => expect(getCurrent().to).toBeDefined())

    // 空欄に戻すと undefined に戻る（キーを undefined 以外の値のまま残さない）。
    fireEvent.change(fromInput, { target: { value: '' } })
    await waitFor(() => expect(getCurrent().from).toBeUndefined())
  })
})
