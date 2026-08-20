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

function stubServicesFetch() {
  const fetchMock = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify(services), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  )
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

    const group = await screen.findByRole('group', { name: 'サービス' })
    expect(within(group).getByRole('button', { name: 'NTV' })).toBeInTheDocument()
  })

  it('同名のサービスは補助ラベルで一意に区別できる（issue #306）', async () => {
    stubServicesFetch()
    renderInRouter(<ConditionFields draft={emptyDraft()} onChange={() => {}} />)

    const group = await screen.findByRole('group', { name: 'サービス' })
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

    const group = await screen.findByRole('group', { name: 'サービス' })
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

    const group = await screen.findByRole('group', { name: 'サービス' })
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

    const group = await screen.findByRole('group', { name: 'サービス' })
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

    const group = await screen.findByRole('group', { name: 'サービス' })
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

    const group = await screen.findByRole('group', { name: 'サービス' })
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
})
