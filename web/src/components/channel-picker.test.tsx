import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'

import type { Service } from '@/api/generated'
import { ChannelPicker } from '@/components/channel-picker'
import { orderServices } from '@/lib/epg-grid'

function service(overrides: Partial<Service> = {}): Service {
  return {
    id: (overrides.networkId ?? 32736) * 100_000 + (overrides.serviceId ?? 1024),
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
        selected={new Set([3273601024])}
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
      <ChannelPicker services={services} selected={new Set([3273601024])} onChange={vi.fn()} />,
    )
    expect(screen.getByRole('button', { name: 'チャンネル: NHK総合' })).toBeInTheDocument()
    expect(screen.getByText('NHK総合')).toBeInTheDocument()

    rerender(
      <ChannelPicker
        services={services}
        selected={new Set([3273601024, 3273602024])}
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

  it('空集合は「すべて」と全候補のチェック済みとして表示する', async () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByRole('checkbox', { name: 'すべて' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    expect(within(dialog).getByRole('checkbox', { name: /NHKEテレ/ })).toHaveAttribute(
      'aria-checked',
      'true',
    )
  })

  it('全局状態で局を外すと全候補との差分になり、全局へ戻すと空集合に正規化する', async () => {
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
    expect(selected).toEqual(new Set([3273601032]))
    expect(within(dialog).getByRole('checkbox', { name: 'すべて' })).toHaveAttribute(
      'aria-checked',
      'mixed',
    )
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )

    await user.click(within(dialog).getByText('NHK総合'))
    expect(selected).toEqual(new Set())
    expect(onChange).toHaveBeenLastCalledWith(new Set())
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    expect(within(dialog).getByRole('checkbox', { name: 'すべて' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
  })

  it('全局から 0 局にすると URL を変えず、次の局選択で選択集合を渡す', async () => {
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
    await user.click(within(dialog).getByRole('checkbox', { name: 'すべて' }))

    expect(onChange).not.toHaveBeenCalled()
    expect(within(dialog).getByRole('checkbox', { name: 'すべて' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(within(dialog).getByRole('status')).toHaveTextContent('1 つ以上選んでください')
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )

    await user.click(within(dialog).getByText('NHK総合'))
    expect(onChange).toHaveBeenCalledExactlyOnceWith(new Set([3273601024]))
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    expect(within(dialog).getByRole('checkbox', { name: /NHKEテレ/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'チャンネル' })).toBeInTheDocument())
  })

  it('候補に無い id だけが選択中でも、「すべて」を押すと空集合（すべて）へ戻れる', async () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    const onChange = vi.fn()
    function Host(): React.ReactElement {
      const [selected, setSelected] = useState<ReadonlySet<number>>(new Set([400999]))
      return (
        <ChannelPicker
          services={services}
          selected={selected}
          onChange={(next) => {
            onChange(next)
            setSelected(next)
          }}
        />
      )
    }
    render(<Host />)

    const dialog = await openPicker(/チャンネル:/)
    const user = userEvent.setup()
    const all = within(dialog).getByRole('checkbox', { name: 'すべて' })
    // 候補外の id は選択数に数えられず、親は未チェック。押すと全候補の選択になり空集合へ正準化される。
    await user.click(all)
    expect(onChange).toHaveBeenLastCalledWith(new Set())
    // 空集合（= すべて）からもう一度押すと 0 局の一時状態になり、候補外 id が復活しない。
    await user.click(all)
    expect(onChange).toHaveBeenCalledTimes(1)
    await user.click(all)
    expect(onChange).toHaveBeenCalledTimes(2)
    expect(onChange).toHaveBeenLastCalledWith(new Set())
  })

  it('0 局のまま閉じると全局に戻る', async () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    const user = userEvent.setup()
    await user.click(within(dialog).getByRole('checkbox', { name: 'すべて' }))
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const reopened = await openPicker('チャンネル: すべて')
    expect(within(reopened).queryByRole('status')).not.toBeInTheDocument()
    expect(within(reopened).getByRole('checkbox', { name: 'すべて' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
    expect(within(reopened).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'true',
    )
  })

  it('明示選択の最後を外して 0 局のまま閉じると全局を通知する', async () => {
    const onChange = vi.fn()
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    render(
      <ChannelPicker services={services} selected={new Set([3273601024])} onChange={onChange} />,
    )

    const dialog = await openPicker('チャンネル: NHK総合')
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('NHK総合'))

    expect(onChange).not.toHaveBeenCalled()
    expect(within(dialog).getByRole('checkbox', { name: /NHK総合/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    await user.keyboard('{Escape}')

    await waitFor(() => expect(onChange).toHaveBeenCalledExactlyOnceWith(new Set()))
  })

  it('種別見出しは mixed から種別内をすべて付け、押し直すとその種別だけ外す', async () => {
    let selected = new Set([3273601024, 3273602024])
    const onChange = vi.fn((next: ReadonlySet<number>) => {
      selected = new Set(next)
    })
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
      service({ serviceId: 2024, name: 'BS日テレ', channelType: 'BS' }),
      service({ serviceId: 3024, name: 'CS局', channelType: 'CS' }),
    ]
    const { rerender } = render(
      <ChannelPicker services={services} selected={selected} onChange={onChange} />,
    )

    const dialog = await openPicker('チャンネル: 2 局を選択中')
    const ground = within(dialog).getByRole('checkbox', { name: '地上波' })
    expect(ground).toHaveAttribute('aria-checked', 'mixed')

    const user = userEvent.setup()
    await user.click(ground)
    expect(selected).toEqual(new Set([3273601024, 3273601032, 3273602024]))
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    expect(within(dialog).getByRole('checkbox', { name: '地上波' })).toHaveAttribute(
      'aria-checked',
      'true',
    )

    await user.click(within(dialog).getByRole('checkbox', { name: '地上波' }))
    expect(selected).toEqual(new Set([3273602024]))
  })

  it('種別が 1 つだけの候補では見出しをチェックボックスにしない', async () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    render(<ChannelPicker services={services} selected={new Set<number>()} onChange={vi.fn()} />)

    const dialog = await openPicker('チャンネル: すべて')
    expect(within(dialog).getByText('地上波')).toBeInTheDocument()
    expect(within(dialog).queryByRole('checkbox', { name: '地上波' })).not.toBeInTheDocument()
  })

  it('検索中の「すべて」と種別見出しは表示中の候補だけを操作する', async () => {
    let selected = new Set<number>()
    const onChange = vi.fn((next: ReadonlySet<number>) => {
      selected = new Set(next)
    })
    const services = Array.from({ length: 16 }, (_, i) =>
      service({
        serviceId: 1000 + i,
        name: i === 0 || i === 8 ? `対象${i}` : `別局${i}`,
        channelType: i < 8 ? 'GR' : 'BS',
      }),
    )
    const { rerender } = render(
      <ChannelPicker services={services} selected={selected} onChange={onChange} />,
    )

    const dialog = await openPicker('チャンネル: すべて')
    const user = userEvent.setup()
    const search = within(dialog).getByLabelText('チャンネルを絞り込む')
    await user.type(search, '対象')

    const all = within(dialog).getByRole('checkbox', { name: '一致したものをすべて' })
    expect(all).toHaveAttribute('aria-checked', 'true')
    await user.click(all)
    expect(selected).toEqual(new Set(services.filter((_, i) => i !== 0 && i !== 8).map((s) => s.id)))
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    expect(within(dialog).getByRole('checkbox', { name: '一致したものをすべて' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(within(dialog).getByRole('checkbox', { name: /対象0/ })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(within(dialog).getByRole('checkbox', { name: '対象8' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    await user.click(within(dialog).getByRole('checkbox', { name: '地上波' }))
    expect(selected).toEqual(new Set(services.filter((_, i) => i !== 8).map((s) => s.id)))
    rerender(<ChannelPicker services={services} selected={selected} onChange={onChange} />)
    expect(within(dialog).getByRole('checkbox', { name: '一致したものをすべて' })).toHaveAttribute(
      'aria-checked',
      'mixed',
    )
    await user.click(within(dialog).getByRole('checkbox', { name: '一致したものをすべて' }))
    expect(selected).toEqual(new Set())
    expect(onChange).toHaveBeenLastCalledWith(new Set())
  })

  it('検索中の種別見出しも表示中の候補だけを操作する', async () => {
    const services = Array.from({ length: 16 }, (_, i) =>
      service({
        serviceId: 1024 + i,
        name: i === 0 || i === 8 ? `対象${i}` : `他局${i}`,
        channelType: i < 8 ? 'GR' : 'BS',
      }),
    )
    const onChange = vi.fn()
    render(
      <ChannelPicker
        services={services}
        selected={new Set(services.map((s) => s.id))}
        onChange={onChange}
      />,
    )

    const dialog = await openPicker('チャンネル: 16 局を選択中')
    const user = userEvent.setup()
    await user.type(within(dialog).getByLabelText('チャンネルを絞り込む'), '対象')
    await user.click(within(dialog).getByRole('checkbox', { name: '地上波' }))

    expect(onChange).toHaveBeenCalledExactlyOnceWith(new Set(services.filter((_, i) => i !== 0).map((s) => s.id)))
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
      .getAllByRole('checkbox')
      .map((el) => el.textContent?.trim() ?? '')
      .filter((text) => !['すべて', '地上波', 'BS', 'CS', 'SKY'].includes(text))

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
    const badge = within(dialog).getByText('1')
    expect(badge).toBeInTheDocument()
    // text-muted-foreground だと bg-muted との合成後コントラストがライトで
    // 4.5 を割る（issue #308）。jsdom は色を測れないので、退行防止としては
    // クラス名のリテラル比較まで（実測は e2e:design の担当）。
    expect(badge.className).toContain('text-foreground')
    expect(badge.className).not.toContain('text-muted-foreground')
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
