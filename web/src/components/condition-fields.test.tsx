import { fireEvent, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import { ConditionFields } from '@/components/condition-fields'
import { emptyDraft, type SearchDraft } from '@/lib/program-search'
import { renderInRouter } from '@/test/router'

/**
 * services は issue #306 の実例（「瀬戸内海放送」が 3 つ並ぶ）を縮小したもの。
 * 1032/1033 はリモコン番号まで同じ（ワンセグ/サブサービス相当）にして、
 * 物理チャンネルまで見て区別する経路を確実に通す。
 *
 * さらに **32677/1033**（32676/1033 と serviceId だけ同じで networkId が違う）を
 * 置いてある。チップの選択・解除は `(networkId, serviceId)` の組で引き当てるが、
 * 同じ networkId の兄弟（1032/1033）と同じ serviceId の他ネットワーク
 * （32676/1033 と 32677/1033）が両方フィクスチャに無いと、述語が片側だけを
 * 見る取り違えがテストを通り抜ける（下の 2 本 --- 解除の引き当てと選択済み
 * 判定 --- がこの 2 組を使う）。
 */
const services: Service[] = [
  {
    id: 3273601024,
    networkId: 32736,
    serviceId: 1024,
    name: 'NTV',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 4,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3267601032,
    networkId: 32676,
    serviceId: 1032,
    name: '瀬戸内海放送',
    channelType: 'GR',
    channel: '21',
    remoteControlKeyId: 5,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3267601033,
    networkId: 32676,
    serviceId: 1033,
    name: '瀬戸内海放送',
    channelType: 'GR',
    // 1032 と同じリモコン番号だが物理チャンネルが違う（別の中継局のサブサービス）
    channel: '95',
    remoteControlKeyId: 5,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    id: 3267701034,
    networkId: 32677,
    serviceId: 1034,
    name: '瀬戸内海放送',
    channelType: 'GR',
    channel: '30',
    remoteControlKeyId: 12,
    hasLogoData: false,
    hasPrograms: true,
  },
  {
    // 32676/1033 と serviceId だけ同じで networkId が違う（別の親局のネットワーク）。
    // 物理チャンネルが違うので補助ラベルは物理チャンネルの段で一意になる。
    id: 3267701033,
    networkId: 32677,
    serviceId: 1033,
    name: '瀬戸内海放送',
    channelType: 'GR',
    channel: '31',
    remoteControlKeyId: 12,
    hasLogoData: false,
    hasPrograms: true,
  },
]

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/**
 * stubServicesFetch は `GET /api/sites` と `GET /api/sites/{site}/services` を
 * スタブする。既定は単一サイト（`default`）だけの構成 --- 呼び出し側を増やさずに
 * 既存の全テストが「単一サイト構成で挙動が変わらない」ことの回帰網を兼ねる。
 * 2 サイト構成を確認するテストは `servicesBySite` を渡す
 * （`recording-filters.test.tsx` の `renderFilters` と同じ形）。
 */
function stubServicesFetch(servicesBySite: Record<string, Service[]> = { default: services }) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = new URL(String(input), 'http://localhost')
    if (url.pathname === '/api/sites') {
      return Promise.resolve(jsonResponse(Object.keys(servicesBySite)))
    }
    const match = /^\/api\/sites\/([^/]+)\/services$/.exec(url.pathname)
    if (match && match[1] in servicesBySite) {
      return Promise.resolve(jsonResponse(servicesBySite[match[1] as keyof typeof servicesBySite]))
    }
    return Promise.resolve(new Response('not found', { status: 404 }))
  })
  globalThis.fetch = fetchMock as unknown as typeof fetch
  return fetchMock
}

// 補助ラベルは名前と別のテキストノード（<span>）に置く。ここでは表示テキスト
// （textContent）で比較する --- アクセシブルネームはノード間の空白の入り方が
// 計算エンジン依存で、jsdom（dom-accessibility-api）の結果が実ブラウザと一致
// する保証はない。32677 の 2 件はリモコン番号だけで 32676 の 2 件と区別できるが、
// 同じグループ内の表記を揃えるため全員に物理チャンネルまで付く。
const label = {
  a: '瀬戸内海放送（地上波 5 ・ 21）', // 32676/1032
  b: '瀬戸内海放送（地上波 5 ・ 95）', // 32676/1033
  c: '瀬戸内海放送（地上波 12 ・ 30）', // 32677/1034
  d: '瀬戸内海放送（地上波 12 ・ 31）', // 32677/1033
}

function findChipByText(group: HTMLElement, text: string): HTMLElement {
  const chip = within(group)
    .getAllByRole('button')
    .find((el) => el.textContent === text)
  if (!chip) throw new Error(`チップが見つからない: ${text}`)
  return chip
}

describe('ConditionFields のサービスチップ', () => {
  it('名前が重複しないサービスには補助ラベルを付けない', async () => {
    stubServicesFetch()
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    expect(within(group).getByRole('button', { name: 'NTV' })).toBeInTheDocument()
  })

  it('同名のサービスは補助ラベルで一意に区別できる（issue #306）', async () => {
    stubServicesFetch()
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    // 同名の「瀬戸内海放送」チップそれぞれの表示テキストがすべて異なることを
    // 確認する。名前だけでは全部同じ文字列になり、どれを押しているか
    // 分からなかった、という issue の観測そのものの再現。
    const chips = within(group).getAllByRole('button', { name: /瀬戸内海放送/ })
    expect(chips).toHaveLength(4)
    expect(new Set(chips.map((c) => c.textContent)).size).toBe(4)

    // 同じネットワークの 1032/1033 も、32677 の 1033/1034 もリモコン番号までは
    // 同じ（ワンセグ/サブサービス相当）なので物理チャンネルまで見て区別する。
    // 一部だけ短いラベルにはしない（グループ内の表記を揃える）。
    expect(findChipByText(group, label.a)).toBeInTheDocument()
    expect(findChipByText(group, label.b)).toBeInTheDocument()
    expect(findChipByText(group, label.c)).toBeInTheDocument()
    expect(findChipByText(group, label.d)).toBeInTheDocument()
  })

  it('補助ラベルの付いたチップを押すと、そのサービスだけが選択される', async () => {
    stubServicesFetch()
    const onChange = vi.fn()
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    fireEvent.click(findChipByText(group, label.b))

    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(emptyDraft())
    // 押したのは serviceId 1033（同名・同リモコン番号の 1032 ではない）。
    expect(next.services).toEqual([{ networkId: 32676, serviceId: 1033 }])
  })

  it('補助ラベルの付いたチップを押すと、そのサービスだけが解除される', async () => {
    stubServicesFetch()
    const onChange = vi.fn()
    // 1033（label.b）だけを選択済みにしておく。ここで測るのは「押した 1 件が
    // 消える」だけ --- 選択が 1 件しか無いので、述語が組で比べているかは
    // これでは測れない（それは下の 2 本が測る）。
    const draft: SearchDraft = {
      ...emptyDraft(),
      services: [{ networkId: 32676, serviceId: 1033 }],
    }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    fireEvent.click(findChipByText(group, label.b))

    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(draft)
    expect(next.services).toEqual([])
  })

  it('選択済みのチップの隣（同名・同リモコン番号）を押しても、選択済みのチップは解除されない', async () => {
    stubServicesFetch()
    const onChange = vi.fn()
    const draft: SearchDraft = {
      ...emptyDraft(),
      services: [{ networkId: 32676, serviceId: 1033 }],
    }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    fireEvent.click(findChipByText(group, label.a))

    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(draft)
    // 1033 は選択済みのまま残り、1032 が追加される（= 押した 1 件だけが増える）。
    expect(next.services).toEqual(
      expect.arrayContaining([
        { networkId: 32676, serviceId: 1033 },
        { networkId: 32676, serviceId: 1032 },
      ]),
    )
    expect(next.services).toHaveLength(2)
  })

  it('解除は (networkId, serviceId) の組で引き当てる（同 networkId の兄弟も同 serviceId の他ネットワークも残る）', async () => {
    stubServicesFetch()
    const onChange = vi.fn()
    // 32676/1033 を押して解除するとき、
    // - 32676/1032: networkId だけで比べていると一緒に消える
    // - 32677/1033: serviceId だけで比べていると一緒に消える
    // の 2 件を同時に選択済みにしておく。片側だけの述語はどちらかで落ちる。
    const draft: SearchDraft = {
      ...emptyDraft(),
      services: [
        { networkId: 32676, serviceId: 1032 },
        { networkId: 32676, serviceId: 1033 },
        { networkId: 32677, serviceId: 1033 },
      ],
    }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    fireEvent.click(findChipByText(group, label.b))

    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(draft)
    expect(next.services).toEqual([
      { networkId: 32676, serviceId: 1032 },
      { networkId: 32677, serviceId: 1033 },
    ])
  })

  it('serviceId が同じでも networkId が違うチップは選択済みにならない', async () => {
    stubServicesFetch()
    const onChange = vi.fn()
    // 32676/1033 だけを選択済みにする。選択済み判定（`selected`）が serviceId
    // だけを見ていると、別ネットワークの 32677/1033 のチップも押された見た目に
    // なり、押すと「追加」ではなく「解除」の枝に入る。
    const draft: SearchDraft = {
      ...emptyDraft(),
      services: [{ networkId: 32676, serviceId: 1033 }],
    }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    expect(findChipByText(group, label.b)).toHaveAttribute('aria-pressed', 'true')
    expect(findChipByText(group, label.d)).toHaveAttribute('aria-pressed', 'false')

    // 見た目だけでなく押した先の枝も測る（解除の枝に入ると 32677/1033 は
    // 追加されない）。
    fireEvent.click(findChipByText(group, label.d))
    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(draft)
    expect(next.services).toEqual([
      { networkId: 32676, serviceId: 1033 },
      { networkId: 32677, serviceId: 1033 },
    ])
  })

  it('番組を持たない同名サービスには「番組なし」を足す（issue #306 のレビュー指摘）', async () => {
    // 主サービス（hasPrograms: true）とサブサービス（false）が同じリモコン
    // 番号・物理チャンネルで並ぶ族。識別子だけでは一意になるが、どちらが
    // 主サービスかは伝わらないため、番組を持たない側にヒントを足す。
    const servicesWithSub: Service[] = [
      {
        id: 3273601024,
        networkId: 32736,
        serviceId: 1024,
        name: '瀬戸内海放送',
        channelType: 'GR',
        channel: '27',
        remoteControlKeyId: 5,
        hasLogoData: false,
        hasPrograms: true,
      },
      {
        id: 3273601032,
        networkId: 32736,
        serviceId: 1032,
        name: '瀬戸内海放送',
        channelType: 'GR',
        channel: '27',
        remoteControlKeyId: 5,
        hasLogoData: false,
        hasPrograms: false,
      },
    ]
    stubServicesFetch({ default: servicesWithSub })
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    // textContent で比較する（アクセシブルネームはノード間の空白の入り方が
    // 計算エンジン依存で jsdom が実ブラウザと一致する保証はないため。
    // 上の `label` 定数と同じ規律）。
    expect(
      findChipByText(group, '瀬戸内海放送（地上波 5 ・ 27 ・ #3273601024）'),
    ).toBeInTheDocument()
    expect(
      findChipByText(group, '瀬戸内海放送（地上波 5 ・ 27 ・ #3273601032 ・ 番組なし）'),
    ).toBeInTheDocument()
  })
})

/**
 * issue #290: サービス選択肢は先頭サイト固定（`useCurrentSite()`）ではなく、
 * `GET /api/sites` の全 site から `Service.id` で畳んで作る（保存されたルールが
 * 全 site で評価されるため）。**この 2 サイトのテストは、直す前の実装
 * （`useCurrentSite()` + `useListServices(site)`）では失敗することを確認済み**
 * ---「default にしかスタブが無いから通ってしまう」ではなく、第 2 サイトだけが
 * 持つサービスが実際に選択肢へ出ることを見ている。
 */
describe('ConditionFields のサービス選択肢（複数サイト、issue #290）', () => {
  const site2Only: Service = {
    id: 400101,
    networkId: 4,
    serviceId: 101,
    name: 'site2 だけのチャンネル',
    channelType: 'BS',
    channel: 'BS1',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: true,
  }

  it('第 2 サイトだけが受けるサービスも選択肢に出る', async () => {
    stubServicesFetch({ default: [services[0]], site2: [services[0], site2Only] })
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    // 両サイトにある NTV は合成 id が同じなので候補は 1 つに畳まれる。
    expect(within(group).getAllByRole('button', { name: 'NTV' })).toHaveLength(1)
    // site2 にしか無いサービスも候補に出る（先頭サイト固定のままなら出ない）。
    expect(
      await within(group).findByRole('button', { name: 'site2 だけのチャンネル' }),
    ).toBeInTheDocument()
  })

  it('選んだ site2 限定サービスは (networkId, serviceId) で下書きに載る', async () => {
    const onChange = vi.fn()
    stubServicesFetch({ default: [services[0]], site2: [services[0], site2Only] })
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'チャンネル' })
    fireEvent.click(await within(group).findByRole('button', { name: 'site2 だけのチャンネル' }))

    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    const next = updater(emptyDraft())
    expect(next.services).toEqual([{ networkId: 4, serviceId: 101 }])
  })
})

/**
 * issue #531: `sites`（`rule_sites` 相当）を条件 UI に出す。
 *
 * **レジストリと下書きの和集合が 2 つ以上のときだけ表示する**
 * （レジストリだけでは判定しない）。単一サイト構成では選択肢が 1 つしか無く、
 * 絞る意味が無いコントロールを置かないため。レジストリに無い site が下書きに
 * 残っている場合は、和集合が 2 つになるので見えて外せる。
 */
describe('ConditionFields のサイトチップ（issue #531）', () => {
  it('サイトが 1 つの構成では「サイト」の節を出さない', async () => {
    stubServicesFetch({ default: services })
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    // サービスの節が描画される（＝取得が終わっている）まで待ってから、
    // サイトの節が無いことを確認する。取得中に「無い」と判定すると、
    // 「まだ届いていないだけ」を「出していない」と誤認する空虚な成功になる。
    await screen.findByRole('group', { name: 'チャンネル' })
    expect(screen.queryByRole('group', { name: 'サイト' })).not.toBeInTheDocument()
  })

  it('サイトが 2 つ以上の構成では両方をチップで出し、選択・解除が下書きに反映される', async () => {
    const onChange = vi.fn()
    stubServicesFetch({ tokyo: services, takamatsu: services })
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'サイト' })
    const tokyoChip = within(group).getByRole('button', { name: 'tokyo' })
    const takamatsuChip = within(group).getByRole('button', { name: 'takamatsu' })
    expect(tokyoChip).toHaveAttribute('aria-pressed', 'false')
    expect(takamatsuChip).toHaveAttribute('aria-pressed', 'false')

    fireEvent.click(tokyoChip)
    expect(onChange).toHaveBeenCalledTimes(1)
    let updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    let next = updater(emptyDraft())
    expect(next.sites).toEqual(['tokyo'])

    fireEvent.click(takamatsuChip)
    updater = onChange.mock.calls[1][0] as (d: SearchDraft) => SearchDraft
    next = updater(next)
    expect(next.sites).toEqual(['tokyo', 'takamatsu'])

    // 選択済みのチップをもう一度押すと解除される
    fireEvent.click(tokyoChip)
    updater = onChange.mock.calls[2][0] as (d: SearchDraft) => SearchDraft
    next = updater(next)
    expect(next.sites).toEqual(['takamatsu'])
  })

  it('レジストリから消えた site が下書きに残っていても、チップとして出して外せる（旧「未解決」の解消）', async () => {
    const onChange = vi.fn()
    // レジストリは tokyo/takamatsu の 2 つだが、下書きは既に無い third を含む
    // （`?ruleId=` で開いたルールの rule_sites がレジストリドリフト後の値を
    // 持っていた場合の再現）。
    stubServicesFetch({ tokyo: services, takamatsu: services })
    const draft: SearchDraft = { ...emptyDraft(), sites: ['third'] }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    const group = await screen.findByRole('group', { name: 'サイト' })
    const thirdChip = within(group).getByRole('button', { name: 'third' })
    expect(thirdChip).toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(thirdChip)
    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    expect(updater(draft).sites).toEqual([])
  })

  /**
   * レビュー指摘: 表示の gate をレジストリ単独（`sites.length <= 1`）に
   * 取ると、レジストリが 1 site に縮んだ環境で下書きが別の site を持つ
   * ケースが「見えない」に戻り、上のテストが解消したはずの未解決が復活する。
   * gate は和集合（`options`）で取らなければならない。
   */
  it('レジストリが 1 site に縮んでいても、下書きが別の site を持てばチップを出す（gate は和集合で取る）', async () => {
    const onChange = vi.fn()
    // レジストリは default だけ（単一サイト構成）だが、下書きは別の
    // takamatsu を持つ（レジストリドリフトで 2 サイトから 1 サイトに
    // 縮んだ後、rule_sites がまだ古い値を持っている状況の再現）。
    stubServicesFetch({ default: services })
    const draft: SearchDraft = { ...emptyDraft(), sites: ['takamatsu'] }
    renderInRouter(<ConditionFields draft={draft} onChange={onChange} />)

    await screen.findByRole('group', { name: 'チャンネル' })
    const group = await screen.findByRole('group', { name: 'サイト' })
    const defaultChip = within(group).getByRole('button', { name: 'default' })
    const takamatsuChip = within(group).getByRole('button', { name: 'takamatsu' })
    expect(defaultChip).toHaveAttribute('aria-pressed', 'false')
    expect(takamatsuChip).toHaveAttribute('aria-pressed', 'true')

    // 画面内で外せる（フォークが 400 になっても救済手段がある）。
    fireEvent.click(takamatsuChip)
    expect(onChange).toHaveBeenCalledTimes(1)
    const updater = onChange.mock.calls[0][0] as (d: SearchDraft) => SearchDraft
    expect(updater(draft).sites).toEqual([])
  })
})
