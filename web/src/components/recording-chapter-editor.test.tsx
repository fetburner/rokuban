import { fireEvent, render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ChapterSpan } from '@/api/generated'
import { RecordingChapterEditor } from '@/components/recording-chapter-editor'

const cm: ChapterSpan = { startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }

function renderEditor(
  spans: ChapterSpan[],
  overrides: Partial<Parameters<typeof RecordingChapterEditor>[0]> = {},
) {
  const onSave = vi.fn()
  const onReset = vi.fn()
  const view = render(
    <RecordingChapterEditor
      spans={spans}
      source="auto"
      currentSeconds={0}
      playAround={vi.fn()}
      onSave={onSave}
      onReset={onReset}
      pending={false}
      {...overrides}
    />,
  )
  return { ...view, onSave, onReset }
}

describe('RecordingChapterEditor', () => {
  it('ラベルが空の区間は「切る」を外せない（本編と区別できなくなる）', () => {
    const { container } = renderEditor([{ startMs: 0, endMs: 10_000, cut: true }])
    const checkbox = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!
    expect(checkbox.checked).toBe(true)
    expect(checkbox.disabled).toBe(true)
  })

  it('ラベルがあれば「切る」を外せる（印だけ付ける OP / ED）', () => {
    const { container } = renderEditor([cm])
    const checkbox = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!
    expect(checkbox.disabled).toBe(false)
    fireEvent.click(checkbox)
    expect(checkbox.checked).toBe(false)
  })

  it('境界の −1秒でドラフトが動き、保存で送る区間が変わる', () => {
    const { container, getByRole, onSave } = renderEditor([cm])
    // 先頭の境界（10 秒）を 1 秒戻す。
    fireEvent.click(container.querySelectorAll('[data-testid="chapter-boundary"]')[0].querySelector('[aria-label$="を -1秒"]')!)
    expect(onSave).not.toHaveBeenCalled()
    fireEvent.click(getByRole('button', { name: '保存' }))
    expect(onSave).toHaveBeenCalledTimes(1)
    expect(onSave.mock.calls[0][0]).toEqual([
      { startMs: 9000, endMs: 20_000, label: 'CM', cut: true },
    ])
  })

  it('「ここから / ここまで」で現在位置の区間を足す', () => {
    const onSave = vi.fn()
    const props = {
      spans: [] as ChapterSpan[],
      source: 'auto' as const,
      playAround: vi.fn(),
      onSave,
      onReset: vi.fn(),
      pending: false,
    }
    const { rerender, getByRole } = render(
      <RecordingChapterEditor {...props} currentSeconds={30} />,
    )
    fireEvent.click(getByRole('button', { name: 'ここから' }))
    // 再生が進んでから閉じる（同じ位置では空の区間になるので足さない）。
    rerender(<RecordingChapterEditor {...props} currentSeconds={40} />)
    fireEvent.click(getByRole('button', { name: 'ここまで' }))
    fireEvent.click(getByRole('button', { name: '保存' }))
    expect(onSave.mock.calls[0][0]).toEqual([{ startMs: 30_000, endMs: 40_000, cut: true }])
  })

  it('サーバーの値が変わるとドラフト（境界の一覧）が追随する', () => {
    const { rerender, container, onSave } = renderEditor([cm])
    expect(container.textContent).toContain('0:00:10')
    const next: ChapterSpan[] = [{ startMs: 50_000, endMs: 60_000, label: 'ED', cut: true }]
    rerender(
      <RecordingChapterEditor
        spans={next}
        source="user"
        currentSeconds={0}
        playAround={vi.fn()}
        onSave={onSave}
        onReset={vi.fn()}
        pending={false}
      />,
    )
    // 前の録画の境界が残らない（残ると、保存で前の値を送ってしまう）。
    expect(container.textContent).toContain('0:00:50')
    expect(container.textContent).not.toContain('0:00:10')
  })

  it('「自動に戻す」は所有していないときは押せない', () => {
    const { getByRole } = renderEditor([cm], { source: 'auto' })
    expect(getByRole('button', { name: '自動に戻す' })).toHaveProperty('disabled', true)
  })
})
