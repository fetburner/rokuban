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
// する保証はない。1034 はリモコン番号だけで他 2 件と区別できるが、同じグループ
// 内の表記を揃えるため物理チャンネルまで付く。
const label = {
  a: '瀬戸内海放送（地上波 5 ・ 21）',
  b: '瀬戸内海放送（地上波 5 ・ 95）',
  c: '瀬戸内海放送（地上波 12 ・ 30）',
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
    // 3 つの「瀬戸内海放送」チップそれぞれの表示テキストがすべて異なることを
    // 確認する。名前だけでは 3 つとも同じ文字列になり、どれを押しているか
    // 分からなかった、という issue の観測そのものの再現。
    const chips = within(group).getAllByRole('button', { name: /瀬戸内海放送/ })
    expect(chips).toHaveLength(3)
    expect(new Set(chips.map((c) => c.textContent)).size).toBe(3)

    // 1032 と 1033 はリモコン番号まで同じ（ワンセグ/サブサービス相当）なので
    // 物理チャンネルまで見て区別する。1034 はリモコン番号だけで区別できるが、
    // 同じグループの表記を揃えるため物理チャンネルまで付く。
    expect(findChipByText(group, label.a)).toBeInTheDocument()
    expect(findChipByText(group, label.b)).toBeInTheDocument()
    expect(findChipByText(group, label.c)).toBeInTheDocument()
  })

  it('補助ラベルの付いたチップを押すと、そのサービスだけが選択・解除される', async () => {
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
})
