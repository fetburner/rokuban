import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { DayStrip } from '@/components/day-strip'

/** 2026-08-01 は土曜日、2026-08-02 は日曜日（両方のケースを 1 つの固定時刻で覆える）。 */
const now = new Date(2026, 7, 1, 10, 0, 0, 0).getTime()

describe('DayStrip', () => {
  it('days=8 で 8 個のボタンが出る（「今」セルは無い）', () => {
    render(<DayStrip current={0} days={8} onSelect={vi.fn()} now={now} />)

    expect(screen.getAllByRole('button')).toHaveLength(8)
    expect(screen.queryByRole('button', { name: '今' })).not.toBeInTheDocument()
  })

  it('current に対応するセルだけ aria-current="date" になる', () => {
    render(<DayStrip current={2} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    expect(buttons[0]).not.toHaveAttribute('aria-current')
    expect(buttons[1]).not.toHaveAttribute('aria-current')
    expect(buttons[2]).toHaveAttribute('aria-current', 'date') // offset 2 は index 2
    expect(buttons.filter((b) => b.getAttribute('aria-current') === 'date')).toHaveLength(1)
  })

  it('current が 0（今日）でも押下状態の aria-pressed は付かない', () => {
    render(<DayStrip current={0} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    expect(buttons[0]).not.toHaveAttribute('aria-pressed')
    expect(buttons[0]).toHaveAttribute('aria-current', 'date')
  })

  it('クリックで onSelect が正しい offset で呼ばれる', () => {
    const onSelect = vi.fn()
    render(<DayStrip current={0} days={8} onSelect={onSelect} now={now} />)

    const buttons = screen.getAllByRole('button')
    fireEvent.click(buttons[2]) // offset 2
    expect(onSelect).toHaveBeenLastCalledWith(2)

    fireEvent.click(buttons[0]) // offset 0
    expect(onSelect).toHaveBeenLastCalledWith(0)
  })

  it('aria-label が曜日を含む（数値だけの読み上げにしない）', () => {
    render(<DayStrip current={0} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    // now = 2026-08-01（土）。先頭が offset 0 = 8/1(土)、その次が offset 1 = 8/2(日)
    expect(buttons[0]).toHaveAttribute('aria-label', '8月1日(土)')
    expect(buttons[1]).toHaveAttribute('aria-label', '8月2日(日)')
  })

  it('見える側の数値・曜日は aria-hidden で二重読みを避ける', () => {
    render(<DayStrip current={0} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    const hidden = buttons[0].querySelector('[aria-hidden="true"]')
    expect(hidden).not.toBeNull()
  })
})
