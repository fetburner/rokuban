import { act, fireEvent, render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ChapterSpan } from '@/api/generated'
import {
  RecordingChapterEditor,
  type ChapterEditorCommands,
} from '@/components/recording-chapter-editor'
import { FRAME_SECONDS } from '@/lib/chapters'

const cm: ChapterSpan = { startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }

function renderEditor(
  spans: ChapterSpan[],
  overrides: Partial<Parameters<typeof RecordingChapterEditor>[0]> = {},
) {
  const onSave = vi.fn((_spans: ChapterSpan[], _version: string) => Promise.resolve())
  const onReset = vi.fn(() => Promise.resolve())
  const jumpTo = vi.fn()
  const onStatusChange = vi.fn()
  const commandsRef: { current: ChapterEditorCommands | null } = { current: null }
  const props: Parameters<typeof RecordingChapterEditor>[0] = {
    spans,
    version: 'v1',
    detectionPending: false,
    source: 'auto',
    recordingId: 7,
    currentSeconds: 0,
    durationSeconds: 120,
    tilesAvailable: true,
    onTileImageLoad: vi.fn(),
    onTileImageError: vi.fn(),
    playAround: vi.fn(),
    jumpTo,
    onSelectedBoundaryChange: vi.fn(),
    onSave,
    onReset,
    pending: false,
    commandsRef,
    onStatusChange,
    ...overrides,
  }
  const view = render(<RecordingChapterEditor {...props} />)
  return { ...view, onSave, onReset, jumpTo, commandsRef, onStatusChange, props }
}

describe('RecordingChapterEditor の編集専用画面', () => {
  it('ラベルが空の区間は「切る」を外せない', () => {
    const { container } = renderEditor([{ startMs: 0, endMs: 10_000, cut: true }])
    const checkbox = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!
    expect(checkbox.checked).toBe(true)
    expect(checkbox.disabled).toBe(true)
  })

  it('ラベルがあれば「切る」を外せる', () => {
    const { container } = renderEditor([cm])
    const checkbox = container.querySelector<HTMLInputElement>('input[type="checkbox"]')!
    expect(checkbox.disabled).toBe(false)
    fireEvent.click(checkbox)
    expect(checkbox.checked).toBe(false)
  })

  it('区間カードから近い境界へ移動し、filmstripで選んだ境界だけを調整する', () => {
    const { getByRole, getByTestId, jumpTo, onSave, commandsRef } = renderEditor([cm], {
      currentSeconds: 18,
    })
    fireEvent.click(getByRole('button', { name: '0:10 から 0:20 の境界を選ぶ' }))
    expect(jumpTo).toHaveBeenCalledWith(599.5 * FRAME_SECONDS)

    const filmstrip = getByTestId('chapter-filmstrip')
    const startBoundary = filmstrip.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!
    fireEvent.click(startBoundary)
    expect(startBoundary.getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(getByRole('button', { name: '選択中の境界を 1 フレーム進める' }))
    expect(onSave).not.toHaveBeenCalled()
    expect(filmstrip.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10033"]')).not.toBeNull()

    expect(commandsRef.current).not.toBeNull()
  })

  it('矢印キーで前後の境界へ移る', () => {
    const op: ChapterSpan = { startMs: 60_000, endMs: 70_000, label: 'OP', cut: false }
    const { container } = renderEditor([cm, op])
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!
    fireEvent.click(boundary20)
    fireEvent.keyDown(boundary20, { key: 'ArrowRight' })
    const boundary60 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="60000"]',
    )!
    expect(boundary60.getAttribute('aria-pressed')).toBe('true')
    fireEvent.keyDown(boundary60, { key: 'ArrowLeft' })
    expect(boundary20.getAttribute('aria-pressed')).toBe('true')
  })

  it('矢印キーはフォーカスが調整ボタンにあっても効き、ラベル入力の中では効かない', () => {
    const op: ChapterSpan = { startMs: 60_000, endMs: 70_000, label: 'OP', cut: false }
    const { container, getByRole, getAllByLabelText } = renderEditor([cm, op])
    const boundary = (ms: number) => container.querySelector<HTMLButtonElement>(
      `[data-testid="chapter-filmstrip-boundary"][data-time-ms="${ms}"]`,
    )!
    fireEvent.click(boundary(20_000))
    // +1秒 を押した後（フォーカスは調整ボタン）でも → で次の境界へ移る。
    fireEvent.click(getByRole('button', { name: '選択中の境界を1秒進める' }))
    fireEvent.keyDown(getByRole('button', { name: '選択中の境界を1秒進める' }), { key: 'ArrowRight' })
    expect(boundary(60_000).getAttribute('aria-pressed')).toBe('true')
    fireEvent.keyDown(getAllByLabelText('ラベル')[0], { key: 'ArrowLeft' })
    expect(boundary(60_000).getAttribute('aria-pressed')).toBe('true')
  })

  it('選択中の境界を前後再生し、現在の再生位置へ合わせる', () => {
    const playAround = vi.fn()
    const { container, getByRole, getByTestId } = renderEditor([{ ...cm, startMs: 2_000, endMs: 4_000 }], {
      currentSeconds: 66.75 * FRAME_SECONDS,
      playAround,
    })
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="4000"]',
    )!
    fireEvent.click(boundary20)
    fireEvent.click(getByRole('button', { name: '選択中の境界の前後3秒を再生' }))
    expect(playAround).toHaveBeenCalledWith(4)
    fireEvent.click(getByRole('button', { name: '選択中の境界を現在の再生位置に合わせる' }))
    expect(getByTestId('chapter-selected-boundary').textContent).toBe('0:02.202')
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="2202"]')).not.toBeNull()
  })

  it('ページヘッダーから呼ぶ保存は下書きと元の版を送り、成功後は dirty を外す', async () => {
    const { container, getByRole, commandsRef, onSave, onStatusChange } = renderEditor([cm])
    const startBoundary = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!
    fireEvent.click(startBoundary)
    fireEvent.click(getByRole('button', { name: '選択中の境界を 1 フレーム進める' }))

    await act(async () => {
      expect(await commandsRef.current?.save()).toBe(true)
    })
    expect(onSave).toHaveBeenCalledWith(
      [{ startMs: 10_033, endMs: 20_000, label: 'CM', cut: true }],
      'v1',
    )
    expect(onStatusChange).toHaveBeenLastCalledWith({ source: 'auto', dirty: true, stale: false })
  })

  it('「ここから区間を足す / ここまで」で再生位置の範囲を追加する', () => {
    const { rerender, getByRole, commandsRef, props, onSave } = renderEditor([], { currentSeconds: 30 })
    fireEvent.click(getByRole('button', { name: 'ここから区間を足す' }))
    rerender(<RecordingChapterEditor {...props} currentSeconds={40} />)
    fireEvent.click(getByRole('button', { name: 'ここまで' }))
    act(() => {
      void commandsRef.current?.save()
    })
    expect(onSave).toHaveBeenCalledWith([{ startMs: 30_000, endMs: 40_000, cut: true }], 'v1')
  })

  it('未変更ならサーバーの新しい値と版へ追随する', () => {
    const { rerender, container, props } = renderEditor([cm])
    const next: ChapterSpan[] = [{ startMs: 50_000, endMs: 60_000, label: 'ED', cut: true }]
    rerender(<RecordingChapterEditor {...props} spans={next} version="v2" source="user" />)
    expect(container.textContent).toContain('0:50')
    expect(container.textContent).not.toContain('0:10')
    expect(container.querySelector('[data-testid="chapter-stale"]')).toBeNull()
  })

  it('dirty draft はサーバー更新時に保持し、stale 警告から最新値を採用できる', () => {
    const { rerender, container, getByRole, props } = renderEditor([cm])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!)
    fireEvent.click(getByRole('button', { name: '選択中の境界を1秒戻す' }))
    const next: ChapterSpan[] = [{ startMs: 50_000, endMs: 60_000, label: 'ED', cut: true }]
    rerender(<RecordingChapterEditor {...props} spans={next} version="v2" />)
    expect(container.querySelector('[data-testid="chapter-stale"]')).not.toBeNull()
    expect(container.textContent).toContain('0:09')
    fireEvent.click(getByRole('button', { name: '下書きを破棄して最新から編集し直す' }))
    expect(container.querySelector('[data-testid="chapter-stale"]')).toBeNull()
    expect(container.textContent).toContain('0:50')
  })

  it('キャンセル命令は dirty draft を捨てる', () => {
    const { commandsRef, getByRole, container } = renderEditor([cm])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!)
    fireEvent.click(getByRole('button', { name: '選択中の境界を1秒戻す' }))
    act(() => commandsRef.current?.discard())
    expect(container.textContent).toContain('0:10')
    expect(container.textContent).not.toContain('0:09')
  })

  it('検出中は編集 UI を出さない', () => {
    const { getByTestId, queryByTestId } = renderEditor([], { detectionPending: true })
    expect(getByTestId('chapter-detecting')).toBeTruthy()
    expect(queryByTestId('chapter-edit-layout')).toBeNull()
    expect(queryByTestId('chapter-filmstrip')).toBeNull()
  })

  it('タイルが利用できなくても境界編集は残る', () => {
    const onTileImageError = vi.fn()
    const { getByTestId, getByRole } = renderEditor([cm], {
      tilesAvailable: false,
      onTileImageError,
    })
    fireEvent.error(getByTestId('chapter-filmstrip').querySelector('img')!)
    expect(onTileImageError).toHaveBeenCalledOnce()
    expect(getByTestId('chapter-filmstrip').querySelector('[data-testid="chapter-filmstrip-boundary"]')).not.toBeNull()
    expect(getByRole('button', { name: '選択中の境界を 1 フレーム進める' })).toHaveProperty('disabled', false)
  })

  it('自動に戻すは外側のヘッダーから呼ばれ、保存/破棄のUIを内側に重ねない', async () => {
    const { commandsRef, onReset, queryByRole } = renderEditor([cm], { source: 'user' })
    expect(queryByRole('button', { name: '自動に戻す' })).toBeNull()
    await act(async () => {
      expect(await commandsRef.current?.reset()).toBe(true)
    })
    expect(onReset).toHaveBeenCalledOnce()
    expect(queryByRole('button', { name: '保存' })).toBeNull()
  })
})
