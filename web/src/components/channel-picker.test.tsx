import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import { ChannelPicker } from '@/components/channel-picker'
import { orderServices } from '@/lib/epg-grid'

function service(overrides: Partial<Service> = {}): Service {
  return {
    networkId: 32736,
    serviceId: 1024,
    name: 'NHK総合1・東京',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 1,
    hasLogoData: false,
    hasPrograms: true,
    ...overrides,
  }
}

/** dialog はピッカーが開いたことを確かめてから取り出す（開閉は非同期）。 */
async function openPicker(triggerName: RegExp | string): Promise<HTMLElement> {
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: triggerName }))
  return screen.findByRole('dialog', { name: 'チャンネル' })
}

describe('ChannelPicker', () => {
  it('選択が無いとき、トリガーに「すべてのチャンネル」が出る', () => {
    render(
      <ChannelPicker services={[service()]} selected={new Set<number>()} onChange={vi.fn()} />,
    )
    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
  })

  it('選択中のサービス名がトリガーに出る（長い名前でも消さない）', () => {
    render(
      <ChannelPicker
        services={[service({ serviceId: 1024, name: 'NHK総合1・東京' })]}
        selected={new Set([1024])}
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', { name: /チャンネル: NHK総合1・東京/ })).toBeInTheDocument()
  })

  it('トリガーの表示は 0 件 / 1 件 / 2 件以上で切り替わる', () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 2024, name: 'BS日テレ', channelType: 'BS' }),
      service({ serviceId: 3024, name: 'CS局', channelType: 'CS' }),
    ]

    const { rerender } = render(
      <ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />,
    )
    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toBeInTheDocument()
    expect(screen.getByText('すべてのチャンネル')).toBeInTheDocument()

    rerender(
      <ChannelPicker services={services} selected={new Set([1024])} onChange={vi.fn()} />,
    )
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.getByText('NHK総合')).toBeInTheDocument()

    rerender(
      <ChannelPicker
        services={services}
        selected={new Set([1024, 2024])}
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', { name: 'チャンネル: 2 局を選択中' })).toBeInTheDocument()
    expect(screen.getByText('2 局を選択中')).toBeInTheDocument()
  })

  it('開くと候補と種別ごとの見出しが出る', async () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合', channelType: 'GR', remoteControlKeyId: 1 }),
      service({ serviceId: 2024, name: 'BS日テレ', channelType: 'BS', remoteControlKeyId: 141 }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByText('地上波')).toBeInTheDocument()
    expect(within(dialog).getByText('BS')).toBeInTheDocument()
    expect(within(dialog).getByText('NHK総合')).toBeInTheDocument()
    expect(within(dialog).getByText('BS日テレ')).toBeInTheDocument()
  })

  it('選ぶと onChange が選択を追加した集合で呼ばれ、ピッカーは閉じない', async () => {
    const onChange = vi.fn()
    const services = [service({ serviceId: 1024, name: 'NHK総合' })]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={onChange} />)

    // 「閉じない」の確認は、必ず開いたことを確かめてから押して、まだ開いていることを見る。
    const dialog = await openPicker('チャンネル: すべて')
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('NHK総合'))

    expect(onChange).toHaveBeenCalledExactlyOnceWith(new Set([1024]))
    // 非同期の空虚な成功を避けるため、閉じていないことを waitFor で確かめる
    // （閉じるアニメーション等が挟まっても安定して判定できるようにする）。
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'チャンネル' })).toBeInTheDocument())
  })

  it('2 局を選ぶと両方選択済みになり、もう一度押すと外れる（トグル、両方向）', async () => {
    let selected = new Set<number>()
    const onChange = vi.fn((next: ReadonlySet<number>) => {
      selected = new Set(next)
    })
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    const { rerender } = render(
      <ChannelPicker services={services} selected={selected} onChange={onChange} />,
    )

    const dialog = await openPicker('チャンネル: すべて')
    const user = userEvent.setup()

    await user.click(within(dialog).getByText('NHK総合'))
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    await user.click(within(dialog).getByText('NHKEテレ'))
    expect(selected).toEqual(new Set([1024, 1032]))

    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    // まだ開いている状態で、もう一度 NHK総合 を押すと外れる
    await user.click(within(dialog).getByText('NHK総合'))
    expect(selected).toEqual(new Set([1032]))
  })

  it('「すべて」を押すと onChange が空集合で呼ばれ、ピッカーは閉じない', async () => {
    const onChange = vi.fn()
    const services = [service({ serviceId: 1024, name: 'NHK総合' })]
    render(
      <ChannelPicker services={services} selected={new Set([1024])} onChange={onChange} />,
    )

    const dialog = await openPicker(/NHK総合/)
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('すべて'))

    expect(onChange).toHaveBeenCalledExactlyOnceWith(new Set())
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'チャンネル' })).toBeInTheDocument())
  })

  it('Esc で閉じ、フォーカスがトリガーに戻る', async () => {
    const services = [service({ serviceId: 1024, name: 'NHK総合' })]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    await openPicker('チャンネル: すべて')
    const user = userEvent.setup()
    await user.keyboard('{Escape}')

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: 'チャンネル: すべて' })).toHaveFocus()
  })

  it('並び順は orderServices と一致する（GR の後に BS、GR 内はリモコン番号順）', async () => {
    const services = [
      service({ serviceId: 3001, name: 'CS局', channelType: 'CS', remoteControlKeyId: 0 }),
      service({ serviceId: 2001, name: 'BS局', channelType: 'BS', remoteControlKeyId: 141 }),
      service({ serviceId: 1003, name: 'GR3', channelType: 'GR', remoteControlKeyId: 3 }),
      service({ serviceId: 1001, name: 'GR1', channelType: 'GR', remoteControlKeyId: 1 }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    const names = within(dialog)
      .getAllByRole('button')
      .map((el) => el.textContent)
      .filter((text): text is string => text !== null && text !== 'すべて')

    // GR かつ remoteControlKeyId > 0 のときはリモコン番号が名前の前に描画される
    // （program-grid.tsx のヘッダと同じ見た目）ので、期待値もそれに合わせる。
    const expectedOrder = orderServices(services).map((s) =>
      s.channelType === 'GR' && s.remoteControlKeyId > 0 ? `${s.remoteControlKeyId}${s.name}` : s.name,
    )
    expect(names).toEqual(expectedOrder)
  })

  it('候補が 15 件以下では絞り込み欄が出ない', async () => {
    const services = Array.from({ length: 15 }, (_, i) =>
      service({ serviceId: 1000 + i, name: `チャンネル${i}`, remoteControlKeyId: i + 1 }),
    )
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    await openPicker('チャンネル: すべて')
    expect(screen.queryByLabelText('チャンネルを絞り込む')).not.toBeInTheDocument()
  })

  it('候補が 16 件では絞り込み欄が出る', async () => {
    const services = Array.from({ length: 16 }, (_, i) =>
      service({ serviceId: 1000 + i, name: `チャンネル${i}`, remoteControlKeyId: i + 1 }),
    )
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    await openPicker('チャンネル: すべて')
    expect(screen.getByLabelText('チャンネルを絞り込む')).toBeInTheDocument()
  })

  it('絞り込みを入力すると候補が減る', async () => {
    const services = Array.from({ length: 16 }, (_, i) =>
      service({ serviceId: 1000 + i, name: `チャンネル${i}`, remoteControlKeyId: i + 1 }),
    )
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByText('チャンネル1')).toBeInTheDocument()
    expect(within(dialog).getByText('チャンネル10')).toBeInTheDocument()

    const user = userEvent.setup()
    await user.type(within(dialog).getByLabelText('チャンネルを絞り込む'), 'チャンネル1')

    // 「チャンネル1」「チャンネル10」〜「チャンネル15」は残り、「チャンネル0」「チャンネル2」等は消える
    expect(within(dialog).getByText('チャンネル1')).toBeInTheDocument()
    expect(within(dialog).getByText('チャンネル10')).toBeInTheDocument()
    expect(within(dialog).queryByText('チャンネル0')).not.toBeInTheDocument()
    expect(within(dialog).queryByText('チャンネル2')).not.toBeInTheDocument()
  })

  it('GR で remoteControlKeyId > 0 のときリモコン番号を出す', async () => {
    const services = [service({ serviceId: 1024, name: 'NHK総合', remoteControlKeyId: 1 })]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByText('1')).toBeInTheDocument()
  })

  it('未知の channelType はコードをそのまま見出しに出す（「その他」に丸めない）', async () => {
    const services = [
      // ServiceChannelType には無い値が来ても落とさない、という契約を確かめる
      service({ serviceId: 9001, name: '未知局', channelType: 'XX' as Service['channelType'] }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByText('XX')).toBeInTheDocument()
  })
})
