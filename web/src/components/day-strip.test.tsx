import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { DayStrip } from '@/components/day-strip'

/** 2026-08-01 は土曜日、2026-08-02 は日曜日（両方のケースを 1 つの固定時刻で覆える）。 */
const now = new Date(2026, 7, 1, 10, 0, 0, 0).getTime()

describe('DayStrip', () => {
  it('days=8 で「今」+ 8 個 = 9 個のボタンが出る', () => {
    render(<DayStrip selected={null} days={8} onSelect={vi.fn()} now={now} />)

    expect(screen.getAllByRole('button')).toHaveLength(9)
  })

  it('selected に対応するボタンだけ aria-pressed="true" になる', () => {
    render(<DayStrip selected={2} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    // 0 番目 = 「今」、以降 offset 0..7
    expect(buttons[0]).toHaveAttribute('aria-pressed', 'false')
    expect(buttons[1]).toHaveAttribute('aria-pressed', 'false')
    expect(buttons[3]).toHaveAttribute('aria-pressed', 'true') // offset 2 は index 3
    expect(buttons.filter((b) => b.getAttribute('aria-pressed') === 'true')).toHaveLength(1)
  })

  it('「今」が選択中のときは先頭ボタンだけ aria-pressed="true" になる', () => {
    render(<DayStrip selected={null} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    expect(buttons[0]).toHaveAttribute('aria-pressed', 'true')
    expect(buttons[1]).toHaveAttribute('aria-pressed', 'false')
  })

  it('クリックで onSelect が正しい offset で呼ばれる（「今」は null）', () => {
    const onSelect = vi.fn()
    render(<DayStrip selected={null} days={8} onSelect={onSelect} now={now} />)

    const buttons = screen.getAllByRole('button')
    fireEvent.click(buttons[0])
    expect(onSelect).toHaveBeenLastCalledWith(null)

    fireEvent.click(buttons[3]) // offset 2
    expect(onSelect).toHaveBeenLastCalledWith(2)
  })

  it('aria-label が曜日を含む（数値だけの読み上げにしない）', () => {
    render(<DayStrip selected={null} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    // now = 2026-08-01（土）。先頭は「今」、次が offset 0 = 8/1(土)、その次が offset 1 = 8/2(日)
    expect(buttons[0]).toHaveAttribute('aria-label', '今')
    expect(buttons[1]).toHaveAttribute('aria-label', '8月1日(土)')
    expect(buttons[2]).toHaveAttribute('aria-label', '8月2日(日)')
  })

  it('見える側の数値・曜日は aria-hidden で二重読みを避ける', () => {
    render(<DayStrip selected={null} days={8} onSelect={vi.fn()} now={now} />)

    const buttons = screen.getAllByRole('button')
    const hidden = buttons[1].querySelector('[aria-hidden="true"]')
    expect(hidden).not.toBeNull()
  })
})
