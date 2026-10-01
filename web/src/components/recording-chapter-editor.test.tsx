import { act, fireEvent, render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ChapterSpan } from '@/api/generated'
import { RecordingChapterEditor } from '@/components/recording-chapter-editor'

const cm: ChapterSpan = { startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }

function renderEditor(
  spans: ChapterSpan[],
  overrides: Partial<Parameters<typeof RecordingChapterEditor>[0]> = {},
) {
  const onSave = vi.fn((_spans: ChapterSpan[], _version: string) => Promise.resolve())
  const onReset = vi.fn()
  const jumpTo = vi.fn()
  const view = render(
    <RecordingChapterEditor
      spans={spans}
      version="v1"
      detectionPending={false}
      source="auto"
      currentSeconds={0}
      playAround={vi.fn()}
      jumpTo={jumpTo}
      onSave={onSave}
      onReset={onReset}
      pending={false}
      {...overrides}
    />,
  )
  return { ...view, onSave, onReset, jumpTo }
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

  it('境界行と区間行の時刻を押すと対応する位置へジャンプする', () => {
    const { container, jumpTo } = renderEditor([cm])
    const boundaryTime = container.querySelector('[data-testid="chapter-boundary"] button')!
    fireEvent.click(boundaryTime)
    expect(jumpTo).toHaveBeenCalledWith(10)

    const spanTime = container.querySelector('[data-testid="chapter-span-row"] button')!
    fireEvent.click(spanTime)
    expect(jumpTo).toHaveBeenCalledTimes(2)
    expect(jumpTo).toHaveBeenLastCalledWith(10)
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
    const onSave = vi.fn((_spans: ChapterSpan[], _version: string) => Promise.resolve())
    const props = {
      spans: [] as ChapterSpan[],
      version: 'v1',
      detectionPending: false,
      source: 'auto' as const,
      playAround: vi.fn(),
      jumpTo: vi.fn(),
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
        version="v2"
        detectionPending={false}
        source="user"
        currentSeconds={0}
        playAround={vi.fn()}
        jumpTo={vi.fn()}
        onSave={onSave}
        onReset={vi.fn()}
        pending={false}
      />,
    )
    // 前の録画の境界が残らない（残ると、保存で前の値を送ってしまう）。
    expect(container.textContent).toContain('0:00:50')
    expect(container.textContent).not.toContain('0:00:10')
  })

  it('下書きがあるときサーバーの値が変わっても黙って捨てず、知らせて保存を止める', () => {
    const { rerender, container, getByRole, getByTestId, queryByTestId, onSave } = renderEditor([cm])
    // 下書きを作る（境界を 1 秒戻す）。
    fireEvent.click(
      container.querySelectorAll('[data-testid="chapter-boundary"]')[0].querySelector('[aria-label$="を -1秒"]')!,
    )
    const next: ChapterSpan[] = [{ startMs: 50_000, endMs: 60_000, label: 'ED', cut: true }]
    rerender(
      <RecordingChapterEditor
        spans={next}
        version="v2"
        detectionPending={false}
        source="auto"
        currentSeconds={0}
        playAround={vi.fn()}
        jumpTo={vi.fn()}
        onSave={onSave}
        onReset={vi.fn()}
        pending={false}
      />,
    )
    expect(getByTestId('chapter-stale').textContent).toContain('サーバー側の内容が変わりました')
    // 下書き（9 秒）が残っている。
    expect(container.textContent).toContain('0:00:09')
    expect(getByRole('button', { name: '保存' })).toHaveProperty('disabled', true)
    // 破棄するとサーバーの新しい内容になる。
    fireEvent.click(getByRole('button', { name: '変更を破棄' }))
    expect(queryByTestId('chapter-stale')).toBeNull()
    expect(container.textContent).toContain('0:00:50')
  })

  it('自分の保存が成功したら、丸められたサーバーの値を採用して stale にならない', async () => {
    // サーバーは境界をフレーム境界へ丸めて保存する（9000 → 9009）ので、届く値は
    // 下書きと一致しない。クライアントで丸めを複製せず、保存成功後の次の値を採用する。
    const { rerender, container, getByRole, queryByTestId, onSave } = renderEditor([cm])
    fireEvent.click(
      container.querySelectorAll('[data-testid="chapter-boundary"]')[0].querySelector('[aria-label$="を -1秒"]')!,
    )
    await act(async () => {
      fireEvent.click(getByRole('button', { name: '保存' }))
    })
    const saved: ChapterSpan[] = [{ startMs: 9009, endMs: 20_020, label: 'CM', cut: true }]
    rerender(
      <RecordingChapterEditor
        spans={saved}
        version="v2"
        detectionPending={false}
        source="user"
        currentSeconds={0}
        playAround={vi.fn()}
        jumpTo={vi.fn()}
        onSave={onSave}
        onReset={vi.fn()}
        pending={false}
      />,
    )
    expect(queryByTestId('chapter-stale')).toBeNull()
    expect(container.textContent).toContain('0:00:09')
    expect(getByRole('button', { name: '保存' })).toHaveProperty('disabled', true) // dirty でない
  })

  it('保存には下書きの基にした版を渡す', () => {
    const { container, getByRole, onSave } = renderEditor([cm])
    fireEvent.click(
      container.querySelectorAll('[data-testid="chapter-boundary"]')[0].querySelector('[aria-label$="を -1秒"]')!,
    )
    fireEvent.click(getByRole('button', { name: '保存' }))
    expect(onSave.mock.calls[0][1]).toBe('v1')
  })

  it('検出中は編集 UI を出さず理由を表示する', () => {
    const { getByTestId, queryByRole } = renderEditor([], { detectionPending: true })
    expect(getByTestId('chapter-detecting')).toBeTruthy()
    expect(queryByRole('button', { name: '保存' })).toBeNull()
  })

  it('「自動に戻す」は所有していないときは押せない', () => {
    const { getByRole } = renderEditor([cm], { source: 'auto' })
    expect(getByRole('button', { name: '自動に戻す' })).toHaveProperty('disabled', true)
  })
})
