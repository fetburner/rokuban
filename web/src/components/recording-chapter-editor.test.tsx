import { act, fireEvent, render, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ChapterSpan, RecordingChapterReview } from '@/api/generated'
import {
  RecordingChapterEditor,
  type ChapterEditorCommands,
} from '@/components/recording-chapter-editor'
import { FRAME_SECONDS } from '@/lib/chapters'

const cm: ChapterSpan = { startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }
const chapterReview: RecordingChapterReview = {
  materialsAvailable: true,
  logoRanges: [],
  autoSpans: [],
  candidates: [
    { kind: 'missed_cm', startMs: 35_125, endMs: 39_000 },
    { kind: 'overcut_logo', startMs: 10_125, endMs: 18_000 },
    { kind: 'overcut_grid', startMs: 25_125, endMs: 29_000 },
  ],
}

function renderEditor(
  spans: ChapterSpan[],
  overrides: Partial<Parameters<typeof RecordingChapterEditor>[0]> = {},
) {
  const onSave = vi.fn((_spans: ChapterSpan[], _version: string) => Promise.resolve())
  const onReset = vi.fn(() => Promise.resolve())
  const jumpTo = vi.fn()
  const onBoundaryAction = vi.fn()
  const onStatusChange = vi.fn()
  const commandsRef: { current: ChapterEditorCommands | null } = { current: null }
  const props: Parameters<typeof RecordingChapterEditor>[0] = {
    spans,
    version: 'v1',
    detectionPending: false,
    source: 'auto',
    seekTilesUrl: '/api/media/recordings/7/seek-tiles?fixture=1',
    currentSeconds: 0,
    isPlaying: false,
    durationSeconds: 120,
    tilesAvailable: true,
    jumpTo,
    onBoundaryAction,
    onSelectedBoundaryChange: vi.fn(),
    onSave,
    onReset,
    pending: false,
    commandsRef,
    onStatusChange,
    ...overrides,
  }
  const view = render(<RecordingChapterEditor {...props} />)
  return { ...view, onSave, onReset, jumpTo, onBoundaryAction, commandsRef, onStatusChange, props }
}

describe('RecordingChapterEditor の編集専用画面', () => {
  it('「切る」を切り替えると要確認の件数・理由・帯がすぐに変わる', () => {
    const view = renderEditor([cm, { startMs: 35_000, endMs: 40_000, label: 'OP', cut: false }], { chapterReview })
    const [cmRow, opRow] = view.getAllByTestId('chapter-span-row')
    const overcut = within(cmRow).getByRole('checkbox', { name: '切る' })
    const missed = within(opRow).getByRole('checkbox', { name: '切る' })
    const status = within(view.getByTestId('chapter-filmstrip')).getByText('切りすぎ 1 件・見逃し 1 件')
    const logoNotes = 'ロゴが出ています。本編かもしれません'
    const missedNotes = 'CM の長さと一致する部分があります'
    const bands = () => view.queryAllByTestId('chapter-review-candidate')
    expect(status).toHaveAttribute('role', 'status')
    expect(within(cmRow).getByText(logoNotes)).toBeInTheDocument()
    expect(within(opRow).getByText(missedNotes)).toBeInTheDocument()
    expect(bands()).toHaveLength(4)

    fireEvent.click(overcut)
    expect(status).toHaveTextContent('切りすぎ 0 件・見逃し 1 件')
    expect(within(cmRow).queryByText(logoNotes)).toBeNull()
    expect(bands()).toHaveLength(2)
    fireEvent.click(missed)
    expect(status).toHaveTextContent('切りすぎ 0 件・見逃し 0 件')
    expect(within(opRow).queryByText(missedNotes)).toBeNull()
    expect(bands()).toHaveLength(0)
    expect(view.getByRole('button', { name: '次の要確認' })).toBeDisabled()

    fireEvent.click(overcut)
    fireEvent.click(missed)
    expect(status).toHaveTextContent('切りすぎ 1 件・見逃し 1 件')
    expect(bands()).toHaveLength(4)
  })

  it('N とボタンは未解決の次の候補へシークし、末尾からは先頭へ戻る', () => {
    const getCurrentSeconds = vi.fn(() => 10.125)
    const view = renderEditor([cm], { chapterReview, currentSeconds: 8, getCurrentSeconds })
    const next = view.getByRole('button', { name: '次の要確認' })
    expect(next).toHaveAttribute('title', '次の要確認（N）')
    fireEvent.keyDown(view.getByRole('textbox', { name: 'ラベル' }), { key: 'n' })
    fireEvent.keyDown(document.body, { key: 'n', ctrlKey: true })
    fireEvent.keyDown(document.body, { key: 'n', repeat: true })
    expect(view.jumpTo).not.toHaveBeenCalled()

    fireEvent.keyDown(document.body, { key: 'n' })
    expect(view.jumpTo).toHaveBeenNthCalledWith(1, 35.125)
    getCurrentSeconds.mockReturnValue(35.125)
    fireEvent.keyDown(document.body, { key: 'n' })
    expect(view.jumpTo).toHaveBeenNthCalledWith(2, 10.125)
    fireEvent.click(next)
    expect(view.jumpTo).toHaveBeenNthCalledWith(3, 10.125)
    expect(view.onBoundaryAction).not.toHaveBeenCalled()
  })

  it('再生位置が候補の先頭の 1 µs 手前に丸められても N で次の候補へ進む', () => {
    const view = renderEditor([cm], { chapterReview, getCurrentSeconds: () => 10.124999 })

    fireEvent.keyDown(document.body, { key: 'n' })

    expect(view.jumpTo).toHaveBeenCalledExactlyOnceWith(35.125)
  })

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

  it('再生位置が終端寄りでも開始ボタンは開始境界を選ぶ', () => {
    const { getByRole, getByTestId, onBoundaryAction, onSave, commandsRef } = renderEditor([cm], {
      currentSeconds: 19,
    })
    const start = getByRole('button', { name: '開始 0:10 の境界を選ぶ' })
    const end = getByRole('button', { name: '終了 0:20 の境界を選ぶ' })
    fireEvent.click(start)
    expect(onBoundaryAction).toHaveBeenCalledWith(10)
    expect(start).toHaveAttribute('aria-pressed', 'true')
    expect(end).toHaveAttribute('aria-pressed', 'false')
    expect(getByTestId('chapter-span-row')).toHaveAttribute('data-selected', 'start')

    const filmstrip = getByTestId('chapter-filmstrip')
    const selectedBoundary = filmstrip.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!
    expect(selectedBoundary).toHaveAttribute('aria-pressed', 'true')
    fireEvent.click(getByRole('button', { name: '選択中の境界を 1 フレーム進める' }))
    expect(onSave).not.toHaveBeenCalled()
    expect(filmstrip.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10033"]')).not.toBeNull()

    expect(commandsRef.current).not.toBeNull()
  })

  it('再生位置が開始寄りでも終了ボタンは終了境界を選ぶ', () => {
    const { getByRole, getByTestId, onBoundaryAction } = renderEditor([cm], { currentSeconds: 11 })
    const end = getByRole('button', { name: '終了 0:20 の境界を選ぶ' })
    fireEvent.click(end)
    expect(onBoundaryAction).toHaveBeenCalledWith(20)
    expect(end).toHaveAttribute('aria-pressed', 'true')
    expect(getByTestId('chapter-span-row')).toHaveAttribute('data-selected', 'end')
  })

  it('選択境界の意味を cut・ラベル・共有境界から表示する', () => {
    const op: ChapterSpan = { startMs: 20_000, endMs: 30_000, label: 'OP', cut: false }
    const standalone = renderEditor([cm])
    fireEvent.click(standalone.getByRole('button', { name: '開始 0:10 の境界を選ぶ' }))
    expect(standalone.getByTestId('chapter-selected-boundary-meaning')).toHaveTextContent('本編の終わり')
    fireEvent.click(standalone.getByRole('button', { name: '終了 0:20 の境界を選ぶ' }))
    expect(standalone.getByTestId('chapter-selected-boundary-meaning')).toHaveTextContent('本編の再開')
    standalone.unmount()

    const cutAndOp = renderEditor([cm, op], { currentSeconds: 19 })
    fireEvent.click(cutAndOp.getByRole('button', { name: '終了 0:20 の境界を選ぶ' }))
    const sharedCutAndOp = cutAndOp.getByTestId('chapter-selected-boundary-meaning')
    expect(sharedCutAndOp).toHaveTextContent('CM の終わり・OP の始まり')
    expect(sharedCutAndOp).not.toHaveTextContent('本編')
    fireEvent.click(cutAndOp.getByRole('button', { name: '終了 0:30 の境界を選ぶ' }))
    expect(cutAndOp.getByTestId('chapter-selected-boundary-meaning')).toHaveTextContent('OP の終わり')
    cutAndOp.unmount()

    const provided: ChapterSpan = { startMs: 20_000, endMs: 30_000, label: '提供', cut: true }
    const adjacentCuts = renderEditor([cm, provided], { currentSeconds: 19 })
    fireEvent.click(adjacentCuts.getByRole('button', { name: '終了 0:20 の境界を選ぶ' }))
    const sharedCuts = adjacentCuts.getByTestId('chapter-selected-boundary-meaning')
    expect(sharedCuts).toHaveTextContent('CM の終わり・提供 の始まり')
    expect(sharedCuts).not.toHaveTextContent('本編')
  })

  it('フィルムストリップの範囲は「〜」でつなぐ', () => {
    const { getByTestId } = renderEditor([cm])
    const filmstrip = getByTestId('chapter-filmstrip')
    expect(filmstrip).toHaveTextContent(/\d+:\d\d〜\d+:\d\d を表示/)
    expect(filmstrip).toHaveTextContent('（全体 0:00〜2:00 のうち枠の部分）')
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

  it('フレーム微調整後も矢印キーで隣の境界へ移れる', () => {
    const op: ChapterSpan = { startMs: 60_000, endMs: 70_000, label: 'OP', cut: false }
    const { container } = renderEditor([cm, op])
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!
    fireEvent.click(boundary20)
    fireEvent.keyDown(window, { key: '.' })

    const nudgedBoundary = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20033"]',
    )!
    expect(nudgedBoundary.getAttribute('aria-pressed')).toBe('true')
    fireEvent.keyDown(nudgedBoundary, { key: 'ArrowRight' })

    const boundary60 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="60000"]',
    )!
    expect(boundary60.getAttribute('aria-pressed')).toBe('true')
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

  it('境界のクリックと微調整は表示位置へ連れて行き、境界フレームの説明を出す', () => {
    const onBoundaryAction = vi.fn()
    const boundarySeconds = 10 + 0.5 * FRAME_SECONDS
    const { container, getByRole, getByTestId } = renderEditor([cm], {
      currentSeconds: boundarySeconds,
      onBoundaryAction,
    })
    const boundary = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!
    expect(getByTestId('chapter-boundary-frame-note').textContent).toContain('次の区間の先頭')
    fireEvent.click(boundary)
    expect(onBoundaryAction).toHaveBeenLastCalledWith(10)
    fireEvent.click(getByRole('button', { name: '選択中の境界を 1 フレーム進める' }))
    expect(onBoundaryAction).toHaveBeenLastCalledWith(10.033)

    const selected = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10033"]',
    )!
    expect(selected.getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(getByRole('button', { name: '選択中の境界を現在の再生位置に合わせる' }))
  })

  it('`,` / `.` で選択境界を1フレーム動かし、入力欄では動かさない', () => {
    const onBoundaryAction = vi.fn()
    const { container, getByRole, getAllByLabelText } = renderEditor([cm], { onBoundaryAction })
    const boundary = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!
    fireEvent.click(boundary)
    fireEvent.keyDown(window, { key: '.' })
    expect(onBoundaryAction.mock.lastCall?.[0]).toBeCloseTo(10 + FRAME_SECONDS, 3)
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10033"]')).not.toBeNull()
    fireEvent.keyDown(getAllByLabelText('ラベル')[0]!, { key: ',' })
    expect(onBoundaryAction.mock.lastCall?.[0]).toBeCloseTo(10 + FRAME_SECONDS, 3)
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10033"]')).not.toBeNull()
    // キー操作は通常の編集ボタンにフォーカスがある場合も有効。
    fireEvent.keyDown(getByRole('button', { name: '選択中の境界を1秒戻す' }), { key: ',' })
    expect(onBoundaryAction.mock.lastCall?.[0]).toBeCloseTo(10, 3)
  })

  describe('±ボタンの長押し', () => {
    const setup = () => {
      const onBoundaryAction = vi.fn()
      const view = renderEditor([cm], { onBoundaryAction })
      fireEvent.click(view.container.querySelector<HTMLButtonElement>(
        '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
      )!)
      const nudge = view.getByRole('button', { name: '選択中の境界を 1 フレーム進める' })
      return { ...view, nudge, onBoundaryAction }
    }
    const boundaryAt = (container: HTMLElement, ms: number) =>
      container.querySelector(`[data-testid="chapter-filmstrip-boundary"][data-time-ms="${ms}"]`)

    it('300ms で離すクリック・タップは 1 回だけ動かす', () => {
      vi.useFakeTimers()
      try {
        const { container, nudge, onBoundaryAction } = setup()
        fireEvent.pointerDown(nudge, { button: 0, pointerId: 1 })
        act(() => vi.advanceTimersByTime(300))
        fireEvent.pointerUp(nudge, { button: 0, pointerId: 1 })
        fireEvent.click(nudge)
        act(() => vi.advanceTimersByTime(1000))
        expect(boundaryAt(container, 10033)).not.toBeNull()
        expect(onBoundaryAction).toHaveBeenCalledTimes(2) // 選択 1 回と nudge 1 回
      } finally {
        vi.useRealTimers()
      }
    })

    it('初回遅延の後は 200ms ごとに送り、元に戻す 1 回で押す前へ戻る', () => {
      vi.useFakeTimers()
      try {
        const { container, getByRole, nudge, onBoundaryAction } = setup()
        fireEvent.pointerDown(nudge, { button: 0, pointerId: 1 })
        act(() => vi.advanceTimersByTime(449))
        expect(onBoundaryAction).toHaveBeenCalledTimes(2) // まだ初回遅延の中
        // 実機では tick の間に描画が挟まる。act を分けて、選択が最新になった状態で次の tick を迎える。
        act(() => vi.advanceTimersByTime(1)) // 450ms
        act(() => vi.advanceTimersByTime(200)) // 650ms
        act(() => vi.advanceTimersByTime(200)) // 850ms
        fireEvent.pointerUp(nudge, { button: 0, pointerId: 1 })
        fireEvent.click(nudge)
        expect(onBoundaryAction).toHaveBeenCalledTimes(5) // 選択 1 回と nudge 4 回
        expect(boundaryAt(container, 10132)).not.toBeNull()
        fireEvent.click(getByRole('button', { name: '元に戻す' }))
        expect(boundaryAt(container, 10000)).not.toBeNull()
        expect(getByRole('button', { name: '元に戻す' })).toBeDisabled()
      } finally {
        vi.useRealTimers()
      }
    })
  })

  it('長押しの途中で境界が合併で消えたら連続送りを止め、残った境界を動かさない', () => {
    vi.useFakeTimers()
    try {
      const next = { ...cm, startMs: 21_500, endMs: 35_000 }
      const { container, getByRole } = renderEditor([cm, next])
      fireEvent.click(container.querySelector<HTMLButtonElement>(
        '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
      )!)
      const nudge = getByRole('button', { name: '選択中の境界を1秒進める' })
      fireEvent.pointerDown(nudge, { button: 0, pointerId: 1 })
      expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="21000"]')).not.toBeNull()
      act(() => vi.advanceTimersByTime(450)) // 22 秒で次の区間と合併して境界が消える
      expect(container.querySelectorAll('[data-testid="chapter-filmstrip-boundary"]')).toHaveLength(2)
      act(() => vi.advanceTimersByTime(200))
      act(() => vi.advanceTimersByTime(200))
      fireEvent.pointerUp(nudge, { button: 0, pointerId: 1 })
      const times = Array.from(container.querySelectorAll('[data-testid="chapter-filmstrip-boundary"]'))
        .map((node) => node.getAttribute('data-time-ms'))
      expect(times).toEqual(['10000', '35000'])
    } finally {
      vi.useRealTimers()
    }
  })

  it('`.` のキーリピートは元に戻す 1 回で押す前へ戻る', () => {
    const { container, getByRole } = renderEditor([cm])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!)
    fireEvent.keyDown(window, { key: '.' })
    fireEvent.keyDown(window, { key: '.', repeat: true })
    fireEvent.keyDown(window, { key: '.', repeat: true })
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10099"]')).not.toBeNull()
    fireEvent.click(getByRole('button', { name: '元に戻す' }))
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]')).not.toBeNull()
    expect(getByRole('button', { name: '元に戻す' })).toBeDisabled()
  })

  it('`,` で動かした境界は元に戻せる', () => {
    const { container, getByRole } = renderEditor([cm])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!)
    fireEvent.keyDown(window, { key: '.' })
    expect(getByRole('button', { name: '元に戻す' })).not.toBeDisabled()
    fireEvent.click(getByRole('button', { name: '元に戻す' }))
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]')).not.toBeNull()
  })

  it('`.` で隣の同じ cut 区間へ重ねると合併し、消えた境界ではなく残った境界を選ぶ', () => {
    const next = { ...cm, startMs: 20_020, endMs: 35_000 }
    const { container, getAllByTestId, onBoundaryAction } = renderEditor([cm, next])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!)
    fireEvent.keyDown(window, { key: '.' })
    expect(getAllByTestId('chapter-span-row')).toHaveLength(1)
    expect(container.querySelectorAll('[data-testid="chapter-filmstrip-boundary"]')).toHaveLength(2)
    expect(onBoundaryAction.mock.lastCall?.[0]).toBeCloseTo(10, 3)
  })

  it('矢印キーで境界の選択を移すと表示位置も止める', () => {
    const onBoundaryAction = vi.fn()
    const op: ChapterSpan = { startMs: 60_000, endMs: 70_000, label: 'OP', cut: false }
    const { container } = renderEditor([cm, op], { onBoundaryAction })
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!
    fireEvent.click(boundary20)
    fireEvent.keyDown(window, { key: 'ArrowRight' })
    expect(onBoundaryAction).toHaveBeenLastCalledWith(60)
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="60000"]')?.getAttribute('aria-pressed')).toBe('true')
  })

  it('表示中の境界フレームだけ、次の区間の先頭だと表示する', () => {
    const expected = 10 + 0.5 * FRAME_SECONDS
    const { getByTestId, rerender, props, queryByTestId } = renderEditor([cm], { currentSeconds: expected })
    expect(getByTestId('chapter-boundary-frame-note').textContent).toContain('境界の直後')
    rerender(<RecordingChapterEditor {...props} currentSeconds={expected + 1} />)
    expect(queryByTestId('chapter-boundary-frame-note')).toBeNull()
    rerender(<RecordingChapterEditor {...props} currentSeconds={expected} isPlaying />)
    expect(queryByTestId('chapter-boundary-frame-note')).toBeNull()
  })

  it('選択中の境界を現在の再生位置に合わせる', () => {
    const { container, getByRole, getByTestId } = renderEditor([{ ...cm, startMs: 2_000, endMs: 4_000 }], {
      currentSeconds: 66.75 * FRAME_SECONDS,
    })
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="4000"]',
    )!
    fireEvent.click(boundary20)
    fireEvent.click(getByRole('button', { name: '選択中の境界を現在の再生位置に合わせる' }))
    expect(getByTestId('chapter-selected-boundary').textContent).toBe('0:02.202')
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="2202"]')).not.toBeNull()
  })

  it('境界の微調整で隣の同じ cut 区間に重なったらまとめ、元に戻すで直前の一手を戻す', () => {
    const next = { ...cm, startMs: 25_500, endMs: 35_000 }
    const { container, getByRole, getAllByTestId } = renderEditor([cm, next], { currentSeconds: 27 })
    const boundary20 = container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!
    fireEvent.click(boundary20)
    for (let i = 0; i < 6; i += 1) {
      fireEvent.click(getByRole('button', { name: '選択中の境界を1秒進める' }))
    }

    expect(getAllByTestId('chapter-span-row')).toHaveLength(1)
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="35000"]'))
      .toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(getByRole('button', { name: '元に戻す' }))
    expect(getAllByTestId('chapter-span-row')).toHaveLength(2)
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="25000"]')).not.toBeNull()
  })

  it('重なりを正規化した下書きを保存し、重なる区間を API に渡さない', async () => {
    const next = { ...cm, startMs: 20_500, endMs: 30_000 }
    const { container, getByRole, commandsRef, onSave } = renderEditor([cm, next])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="20000"]',
    )!)
    fireEvent.click(getByRole('button', { name: '選択中の境界を1秒進める' }))

    await act(async () => {
      expect(await commandsRef.current?.save()).toBe(true)
    })
    expect(onSave).toHaveBeenCalledWith([{ ...cm, endMs: 30_000 }], 'v1')
  })

  it('ラベル入力のフォーカス中の変更を元に戻す 1 手にまとめる', () => {
    const { getByLabelText, getByRole } = renderEditor([cm])
    const label = getByLabelText('ラベル')
    fireEvent.focus(label)
    fireEvent.change(label, { target: { value: 'C' } })
    fireEvent.change(label, { target: { value: 'CM 予定' } })
    fireEvent.blur(label)
    fireEvent.click(getByRole('button', { name: '元に戻す' }))
    expect(label).toHaveValue('CM')
    expect(getByRole('button', { name: '元に戻す' })).toHaveProperty('disabled', true)
  })

  it.each([
    ['i', 'CM の終わりにする', [{ ...cm, startMs: 2_000, endMs: 2_236 }]],
    ['o', 'CM の始まりにする', [{ ...cm, startMs: 2_236, endMs: 4_000 }]],
    ['Backspace', 'この CM を消す', []],
  ] as const)('%s と「%s」は同じ CM 編集を表示フレームに適用し、元に戻せる', async (key, button, expected) => {
    const span = { ...cm, startMs: key === 'Backspace' ? 2_236 : 2_000, endMs: 4_000 }
    const { getByTestId, onSave, commandsRef, props, rerender } = renderEditor([], {
      currentSeconds: 66.75 * FRAME_SECONDS,
      getDisplayedFrameSeconds: () => 67 * FRAME_SECONDS,
    })
    const control = within(getByTestId('chapter-tuning-controls')).getByRole('button', { name: button })
    expect(control.title).toContain(key.length === 1 ? key.toUpperCase() : key)
    expect(getByTestId('chapter-selected-boundary')).toHaveTextContent('—')
    expect(control).toBeEnabled()

    rerender(<RecordingChapterEditor {...props} spans={[span]} />)
    expect(fireEvent.keyDown(document.body, { key })).toBe(false)
    await act(() => commandsRef.current!.save())
    expect(onSave).toHaveBeenLastCalledWith(expected, 'v1')

    fireEvent.keyDown(document.body, { key: 'z', metaKey: true })
    await act(() => commandsRef.current!.save())
    expect(onSave).toHaveBeenLastCalledWith([span], 'v1')

    fireEvent.click(control)
    await act(() => commandsRef.current!.save())
    expect(onSave).toHaveBeenLastCalledWith(expected, 'v1')
  })

  it('O で CM を合併して境界が消えると残った最寄りを選ぶ', () => {
    const { getByTestId } = renderEditor([{ ...cm, endMs: 20_020 }, { ...cm, startMs: 30_000, endMs: 40_000 }], {
      currentSeconds: 39.9,
      getDisplayedFrameSeconds: () => 600 * FRAME_SECONDS,
    })
    expect(getByTestId('chapter-selected-boundary').textContent).toBe('0:40.000')
    fireEvent.keyDown(document.body, { key: 'o' })
    expect(getByTestId('chapter-selected-boundary').textContent).toBe('0:10.000')
  })

  it.each([
    [10_000, '0:10.000'],
    [8_000, '0:08.000'],
  ])('Backspace で CM を消すと選択境界 %i ms が残る場合は保ち、消える場合は最寄りを選ぶ', (boundaryMs, selection) => {
    const { container, getByTestId } = renderEditor([
      { ...cm, startMs: 5_000, endMs: 8_000 },
      cm,
      { ...cm, startMs: 21_000, endMs: 24_000 },
    ], { currentSeconds: 19 })
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      `[data-testid="chapter-filmstrip-boundary"][data-time-ms="${boundaryMs}"]`,
    )!)
    expect(getByTestId('chapter-selected-boundary').textContent).toBe(selection)
    fireEvent.keyDown(document.body, { key: 'Backspace' })
    expect(getByTestId('chapter-selected-boundary').textContent).toBe('0:08.000')
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]')).toBeNull()
  })

  it.each(['i', 'o', 'Backspace'])('ラベル入力中の %s と修飾キー付きの操作は下書きを変えない', async (key) => {
    const { getByLabelText, getByRole, onSave, commandsRef } = renderEditor([cm], { currentSeconds: 15 })
    const label = getByLabelText('ラベル')
    label.focus()
    expect(fireEvent.keyDown(label, { key })).toBe(true)
    label.blur()
    expect(fireEvent.keyDown(document.body, { key, metaKey: true })).toBe(true)
    expect(getByRole('button', { name: '元に戻す' })).toHaveProperty('disabled', true)
    await act(() => commandsRef.current!.save())
    expect(onSave).toHaveBeenCalledWith([cm], 'v1')
  })

  it('ラベル入力にフォーカスがあると Ctrl+Z は下書きの履歴を戻さない', () => {
    const { container, getByRole, getByLabelText } = renderEditor([cm])
    fireEvent.click(container.querySelector<HTMLButtonElement>(
      '[data-testid="chapter-filmstrip-boundary"][data-time-ms="10000"]',
    )!)
    fireEvent.click(getByRole('button', { name: '選択中の境界を1秒進める' }))
    const label = getByLabelText('ラベル')
    fireEvent.focus(label)
    fireEvent.keyDown(label, { key: 'z', ctrlKey: true })
    expect(container.querySelector('[data-testid="chapter-filmstrip-boundary"][data-time-ms="11000"]')).not.toBeNull()
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
    expect(onSave).toHaveBeenCalledWith([{ startMs: 29_997, endMs: 39_973, cut: true }], 'v1')
  })

  it('duration が無限でも「ここから区間を足す / ここまで」で区間を足せる', () => {
    const { rerender, getByRole, commandsRef, props, onSave } = renderEditor([], {
      currentSeconds: 30,
      durationSeconds: Infinity,
    })
    fireEvent.click(getByRole('button', { name: 'ここから区間を足す' }))
    rerender(<RecordingChapterEditor {...props} currentSeconds={40} />)
    fireEvent.click(getByRole('button', { name: 'ここまで' }))
    act(() => {
      void commandsRef.current?.save()
    })
    expect(onSave).toHaveBeenCalledWith([{ startMs: 29_997, endMs: 39_973, cut: true }], 'v1')
  })

  it('「最初からここまで切る」と「ここから最後まで切る」は録画の先頭と終端を使う', () => {
    const first = renderEditor([], { currentSeconds: 30 })
    fireEvent.click(first.getByRole('button', { name: '最初からここまで切る' }))
    act(() => {
      void first.commandsRef.current?.save()
    })
    expect(first.onSave).toHaveBeenCalledWith([{ startMs: 0, endMs: 29_997, cut: true }], 'v1')
    first.unmount()

    const last = renderEditor([], { currentSeconds: 30 })
    fireEvent.click(last.getByRole('button', { name: 'ここから最後まで切る' }))
    act(() => {
      void last.commandsRef.current?.save()
    })
    expect(last.onSave).toHaveBeenCalledWith([{ startMs: 29_997, endMs: 120_000, cut: true }], 'v1')
  })

  it('新しい区間の端点を現在表示中のフレーム先頭へ合わせる', () => {
    const { rerender, getByRole, commandsRef, props, onSave } = renderEditor([], {
      currentSeconds: 66.75 * FRAME_SECONDS,
    })
    fireEvent.click(getByRole('button', { name: 'ここから区間を足す' }))
    rerender(<RecordingChapterEditor {...props} currentSeconds={67.75 * FRAME_SECONDS} />)
    fireEvent.click(getByRole('button', { name: 'ここまで' }))
    act(() => {
      void commandsRef.current?.save()
    })
    expect(onSave).toHaveBeenCalledWith([{ startMs: 2202, endMs: 2236, cut: true }], 'v1')
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
    const { getByTestId, getByRole } = renderEditor([cm], {
      tilesAvailable: false,
    })
    expect(getByTestId('chapter-filmstrip').querySelector('img')).toBeNull()
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
