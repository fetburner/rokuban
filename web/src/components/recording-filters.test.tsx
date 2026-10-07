import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { Rule, Service } from '@/api/generated'
import { RecordingFilters } from '@/components/recording-filters'
import { emptyRecordingsSearch, parseRecordingsSearch, type RecordingsPageSearch } from '@/lib/recording-search'

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

function rule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 1,
    name: 'ニュース録画ルール',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
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
  rules: Rule[] = [],
  rulesResponse?: () => Promise<Response>,
) {
  const servicesBySite = { default: services, ...otherSites }
  globalThis.fetch = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') {
      return Promise.resolve(jsonResponse(Object.keys(servicesBySite)))
    }
    if (url.pathname === '/api/rules') {
      return rulesResponse?.() ?? Promise.resolve(jsonResponse(rules))
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
  it('レジストリに無い site もチップで見えて、押すと絞り込みを外せる', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters({ site: ['retired'] })

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const siteGroup = await within(panel).findByRole('group', { name: 'サイト' })

    // 和集合の両方が見えている（レジストリの `default` が消えていない）。
    const siteChips = within(siteGroup).getAllByRole('button')
    expect(siteChips).toHaveLength(2)
    expect(within(siteGroup).getByRole('button', { name: 'default' })).toBeInTheDocument()
    expect(within(siteGroup).getByRole('button', { name: 'retired' })).toHaveAttribute('aria-pressed', 'true')

    await user.click(within(siteGroup).getByRole('button', { name: 'retired' }))
    expect(onChangeCalls.at(-1)?.site).toBeUndefined()
  })

  it('単一 site で絞り込みが無ければサイトの節を出さない', async () => {
    const user = userEvent.setup()
    renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    // site 取得の完了を待ってから「無いこと」を測る（読み込み中に測ると
    // レジストリが空でも通ってしまう。`ChannelPicker` のトリガーは
    // `servicesPending` が false のときだけ描かれ、`useAllSitesServices()`
    // の `isPending` は `sitesQuery.isPending` を OR しているので、この
    // ボタンが出ていれば /api/sites は解決済み）。
    await within(panel).findByRole('button', { name: /チャンネル/ })
    expect(within(panel).queryByRole('group', { name: 'サイト' })).not.toBeInTheDocument()
  })

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

  it('種別チップ（ルール・手動・帰属なし）が source を切り替える', async () => {
    const user = userEvent.setup()
    const { onChangeCalls } = renderFilters()

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })

    await user.click(within(panel).getByRole('button', { name: '手動' }))
    expect(onChangeCalls.at(-1)?.source).toBe('manual')

    await user.click(within(panel).getByRole('button', { name: '帰属なし' }))
    expect(onChangeCalls.at(-1)?.source).toBe('unattributed')
  })

  it('ルールを選ぶと ruleId を反映し、source を外して手動・帰属なしを無効にする', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters(
      { source: 'manual' },
      [service()],
      {},
      [rule({ id: 8, name: '先に返ったルール' }), rule({ id: 3, name: '後に返ったルール' })],
    )

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSelect = await within(panel).findByRole('combobox', { name: 'ルール' })
    expect(Array.from(ruleSelect.querySelectorAll('option')).map((option) => option.value)).toEqual([
      '',
      '8',
      '3',
    ])

    await user.selectOptions(ruleSelect, '3')
    await waitFor(() => expect(getCurrent()).toMatchObject({ ruleId: 3, source: undefined }))

    const sourceGroup = within(panel).getByRole('group', { name: '種別' })
    expect(within(sourceGroup).getByRole('button', { name: '手動' })).toBeDisabled()
    expect(within(sourceGroup).getByRole('button', { name: '帰属なし' })).toBeDisabled()
    expect(within(sourceGroup).getByRole('button', { name: 'ルール' })).not.toBeDisabled()

    await user.selectOptions(ruleSelect, '')
    await waitFor(() => expect(getCurrent().ruleId).toBeUndefined())
  })

  it('ルールの取得中はルール節に読み込み中を出す', async () => {
    const user = userEvent.setup()
    let resolveRules: (response: Response) => void = () => undefined
    const pendingRules = new Promise<Response>((resolve) => {
      resolveRules = resolve
    })
    renderFilters(emptyRecordingsSearch(), [service()], {}, [], () => pendingRules)

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSection = within(panel).getByRole('heading', { name: 'ルール' }).parentElement
    expect(ruleSection).not.toBeNull()
    expect(within(ruleSection as HTMLElement).getByRole('status')).toHaveTextContent('読み込み中…')

    resolveRules(jsonResponse([rule()]))
    await waitFor(() => expect(within(ruleSection as HTMLElement).getByRole('combobox', { name: 'ルール' })).toBeInTheDocument())
  })

  // E: 機能しないコントロールは置かない --- ルールが 0 件の新規インストールでは
  // 「問わない」だけの select を常に出さない。取得完了を明示的に待ってから
  // 節ごと消えることを見る（`resolveRules` を手で叩くことで、フェッチが速すぎて
  // pending の一瞬を観測し損ねる空虚な成功を避ける）。
  it('ルールが 0 件ならルール節ごと出さない', async () => {
    const user = userEvent.setup()
    let resolveRules: (response: Response) => void = () => undefined
    const pendingRules = new Promise<Response>((resolve) => {
      resolveRules = resolve
    })
    renderFilters(emptyRecordingsSearch(), [service()], {}, [], () => pendingRules)

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    // 取得中は節が出て理由（読み込み中）が読める（チャンネル節と同じ流儀）。
    const ruleSection = within(panel).getByRole('heading', { name: 'ルール' }).parentElement
    expect(ruleSection).not.toBeNull()
    expect(within(ruleSection as HTMLElement).getByRole('status')).toHaveTextContent('読み込み中…')

    resolveRules(jsonResponse([]))
    await waitFor(() => expect(within(panel).queryByRole('heading', { name: 'ルール' })).not.toBeInTheDocument())
  })

  it('ルールが 1 件以上あればルール節を出す', async () => {
    const user = userEvent.setup()
    renderFilters(emptyRecordingsSearch(), [service()], {}, [rule()])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    expect(await within(panel).findByRole('combobox', { name: 'ルール' })).toBeInTheDocument()
  })

  it('ルールが 0 件でも search.ruleId があればルール節を出す（削除済みルールで絞っている状態を読める）', async () => {
    const user = userEvent.setup()
    renderFilters({ ruleId: 5 }, [service()], {}, [])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    expect(await within(panel).findByRole('heading', { name: 'ルール' })).toBeInTheDocument()
  })

  // A: 一覧に無い ruleId（削除済みルール・古い共有リンク）を select が
  // 「問わない」に落として見せてしまうと、適用中チップ（`ルール #N`）と
  // パネルの表示が食い違う。フォールバック option を足して選択状態を保つ。
  it('一覧に無い ruleId は select にフォールバック option を足して選択状態にする', async () => {
    const user = userEvent.setup()
    renderFilters({ ruleId: 99 }, [service()], {}, [rule({ id: 8, name: '先に返ったルール' })])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSelect = (await within(panel).findByRole('combobox', { name: 'ルール' })) as HTMLSelectElement

    expect(ruleSelect.value).toBe('99')
    expect(ruleSelect.selectedIndex).not.toBe(0)
    expect(within(panel).getByRole('option', { name: 'ルール #99' })).toBeInTheDocument()
  })

  it('一覧にある ruleId は名前の option を選択状態にする（フォールバックを足さない）', async () => {
    const user = userEvent.setup()
    renderFilters({ ruleId: 8 }, [service()], {}, [rule({ id: 8, name: '先に返ったルール' })])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSelect = (await within(panel).findByRole('combobox', { name: 'ルール' })) as HTMLSelectElement

    expect(ruleSelect.value).toBe('8')
    expect(within(panel).getByRole('option', { name: '先に返ったルール' })).toBeInTheDocument()
    expect(within(panel).queryByRole('option', { name: /^ルール #/ })).not.toBeInTheDocument()
  })

  it('同名のルールは option と適用中チップで id を添えて押し分ける', async () => {
    const user = userEvent.setup()
    renderFilters(
      { ruleId: 8 },
      [service()],
      {},
      [rule({ id: 8, name: '同名ルール' }), rule({ id: 3, name: '同名ルール' })],
    )

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'ルール: 同名ルール (#8)' })).toBeInTheDocument(),
    )

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    await within(panel).findByRole('combobox', { name: 'ルール' })
    expect(within(panel).getByRole('option', { name: '同名ルール (#8)' })).toBeInTheDocument()
    expect(within(panel).getByRole('option', { name: '同名ルール (#3)' })).toBeInTheDocument()
  })

  // B: `parseRecordingsSearch` が URL 段階で落とすので、パネルへ渡る search
  // には active かつ disabled なチップが原理的に現れない（表現不可能にする。
  // 不変条件 10）。壊すと（正規化を外すと）このテストは落ちる。
  it('ruleId と source=manual を同時に指定する URL を開いても、active かつ disabled な種別チップは無い', async () => {
    const user = userEvent.setup()
    const initial = parseRecordingsSearch({ ruleId: 8, source: 'manual' })
    renderFilters(initial, [service()], {}, [rule({ id: 8 })])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const sourceGroup = within(panel).getByRole('group', { name: '種別' })
    const activeDisabled = within(sourceGroup)
      .getAllByRole('button')
      .filter((btn) => btn.getAttribute('aria-pressed') === 'true' && (btn as HTMLButtonElement).disabled)

    expect(activeDisabled).toHaveLength(0)
  })

  // C: 「種別=ルール」を選んだあとに特定のルールを選び、select を
  // 「問わない」へ戻しても、`updateRuleFilter` が解除してよいのは
  // manual / unattributed だけ --- source='rule' は ruleId と矛盾しないので
  // 残る。壊すと（無条件に source を解除すると）このテストは落ちる。
  it('種別=ルールのまま特定のルールを選び、select を「問わない」に戻しても source=rule は残る', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters({ source: 'rule' }, [service()], {}, [rule({ id: 8 })])

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSelect = await within(panel).findByRole('combobox', { name: 'ルール' })

    await user.selectOptions(ruleSelect, '8')
    await waitFor(() => expect(getCurrent()).toMatchObject({ ruleId: 8, source: 'rule' }))

    await user.selectOptions(ruleSelect, '')
    await waitFor(() => expect(getCurrent()).toMatchObject({ ruleId: undefined, source: 'rule' }))
  })

  it('ルールの取得に失敗したときはルール節に理由を出す', async () => {
    const user = userEvent.setup()
    renderFilters(emptyRecordingsSearch(), [service()], {}, [], () =>
      Promise.resolve(new Response('failed', { status: 500 })),
    )

    await user.click(screen.getByRole('button', { name: /絞り込み/ }))
    const panel = await screen.findByRole('dialog', { name: '絞り込み' })
    const ruleSection = within(panel).getByRole('heading', { name: 'ルール' }).parentElement
    expect(ruleSection).not.toBeNull()
    expect(await within(ruleSection as HTMLElement).findByText('ルールの取得に失敗しました')).toBeInTheDocument()
    expect(within(ruleSection as HTMLElement).queryByRole('combobox', { name: 'ルール' })).not.toBeInTheDocument()
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
    const shared = await within(channelDialog).findAllByRole('checkbox', { name: /ＮＨＫ総合/ })
    expect(shared).toHaveLength(1)
    // site2 にしか無い局も候補に出る。
    expect(within(channelDialog).getByRole('checkbox', { name: /site2 だけの局/ })).toBeInTheDocument()

    // 何も絞っていない状態は全局チェック済みなので、まず全局を外してから選ぶ。
    await user.click(within(channelDialog).getByRole('checkbox', { name: 'すべて' }))
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
    const options = within(channelDialog).getAllByRole('checkbox', { name: /同名チャンネル/ })
    expect(options).toHaveLength(2)
    expect(options.map((option) => option.getAttribute('aria-checked'))).toEqual(['true', 'true'])

    // 片方（CS 側）を解除すると、もう片方（BS 側）だけが残る。
    await user.click(options[1])
    await waitFor(() => expect(getCurrent().service).toEqual([400101]))
  })
})

describe('RecordingFilters 期間', () => {
  // 「今」を 2026-10-07（水）12:00 JST に固定する。Date だけを偽にし、userEvent の
  // タイマーは本物のまま動かす。
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-10-07T03:00:00Z'))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('クールを選ぶと from/to が日本時間の区切りになり、他の条件は保たれ、パネルが閉じる', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters({ genre: [7], q: 'アニメ' })

    await user.click(screen.getByRole('button', { name: '期間' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    await user.click(within(within(panel).getByRole('group', { name: '2026 年のクール' })).getByRole('button', { name: '夏' }))

    expect(getCurrent()).toEqual({
      genre: [7],
      q: 'アニメ',
      from: '2026-06-30T15:00:00.000Z',
      to: '2026-09-30T15:00:00.000Z',
    })
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '期間' })).not.toBeInTheDocument())
    // ボタンとチップが同じ表示関数で同じ文字列を出す
    expect(screen.getByRole('button', { name: '2026 夏' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '期間: 2026 夏' })).toBeInTheDocument()
  })

  it('まだ始まっていない期と来年へは進めない', async () => {
    const user = userEvent.setup()
    renderFilters()

    await user.click(screen.getByRole('button', { name: '期間' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    const group = within(panel).getByRole('group', { name: '2026 年のクール' })
    expect(within(group).getByRole('button', { name: '秋' })).toBeEnabled()
    expect(within(panel).getByRole('button', { name: '次の年' })).toBeDisabled()

    await user.click(within(panel).getByRole('button', { name: '前の年' }))
    const lastYear = within(panel).getByRole('group', { name: '2025 年のクール' })
    expect(within(lastYear).getByRole('button', { name: '秋' })).toBeEnabled()
    expect(within(panel).getByRole('button', { name: '次の年' })).toBeEnabled()
  })

  it('年の途中では、今年のまだ始まっていない期だけを押せない', async () => {
    vi.setSystemTime(new Date('2026-05-10T03:00:00Z')) // 2026 春
    const user = userEvent.setup()
    renderFilters()

    await user.click(screen.getByRole('button', { name: '期間' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    const group = within(panel).getByRole('group', { name: '2026 年のクール' })
    expect(within(group).getByRole('button', { name: '冬' })).toBeEnabled()
    expect(within(group).getByRole('button', { name: '春' })).toBeEnabled()
    expect(within(group).getByRole('button', { name: '夏' })).toBeDisabled()
    expect(within(group).getByRole('button', { name: '秋' })).toBeDisabled()
  })

  it('月曜が 1 日の日は「今週」と「今月」が同じ範囲でも、チェックは表示名の 1 件だけに付く', async () => {
    vi.setSystemTime(new Date('2026-06-01T03:00:00Z')) // 2026-06-01（月）
    const user = userEvent.setup()
    renderFilters({ from: '2026-05-31T15:00:00.000Z' })

    await user.click(screen.getByRole('button', { name: '今週' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    expect(within(panel).getByRole('button', { name: '今週' })).toHaveAttribute('aria-pressed', 'true')
    expect(within(panel).getByRole('button', { name: '今月' })).toHaveAttribute('aria-pressed', 'false')
  })

  it('上段の「今週」は月曜 0:00 JST から to なしで、他の条件を保つ', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters({ status: 'finished', to: '2026-01-01T00:00:00.000Z' })

    await user.click(screen.getByRole('button', { name: '〜1/1 9:00 前' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    await user.click(within(panel).getByRole('button', { name: '今週' }))

    expect(getCurrent()).toEqual({ status: 'finished', from: '2026-10-04T15:00:00.000Z', to: undefined })
    expect(screen.getByRole('button', { name: '今週' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '期間: 今週' })).toBeInTheDocument()
  })

  it('日付で指定すると終了日はその日を含み、入力しても閉じない', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters({ genre: [7] })

    await user.click(screen.getByRole('button', { name: '期間' }))
    const panel = await screen.findByRole('dialog', { name: '期間' })
    fireEvent.change(within(panel).getByLabelText('開始日'), { target: { value: '2026-06-01' } })
    fireEvent.change(within(panel).getByLabelText('終了日（この日を含む）'), { target: { value: '2026-06-30' } })

    expect(getCurrent()).toEqual({
      genre: [7],
      from: '2026-05-31T15:00:00.000Z',
      to: '2026-06-30T15:00:00.000Z',
    })
    // URL の to（7/1 0:00 JST）から戻した終了日欄は 6/30
    expect(within(panel).getByLabelText('終了日（この日を含む）')).toHaveValue('2026-06-30')
    expect(screen.getByRole('dialog', { name: '期間' })).toBeInTheDocument()

    fireEvent.change(within(panel).getByLabelText('開始日'), { target: { value: '' } })
    expect(getCurrent().from).toBeUndefined()

    await user.click(within(panel).getByRole('button', { name: '完了' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '期間' })).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: '〜6/30' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '期間: 〜6/30' })).toBeInTheDocument()
  })

  it('期間のチップを外すと from と to が両方消える', async () => {
    const user = userEvent.setup()
    const { getCurrent } = renderFilters({ genre: [7], from: '2026-06-30T15:00:00.000Z', to: '2026-09-30T15:00:00.000Z' })

    await user.click(screen.getByRole('button', { name: '期間: 2026 夏' }))

    expect(getCurrent()).toEqual({ genre: [7], from: undefined, to: undefined })
    expect(screen.getByRole('button', { name: '期間' })).toBeInTheDocument()
  })
})
