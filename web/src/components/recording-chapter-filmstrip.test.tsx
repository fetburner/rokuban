import { fireEvent, render } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RecordingChapterFilmstrip } from '@/components/recording-chapter-filmstrip'

const props = {
  seekTilesUrl: '/seek-tiles.jpg',
  durationSeconds: 600,
  currentSeconds: 300,
  getDisplayedFrameSeconds: () => null,
  isPlaying: false,
  spans: [],
  selectedBoundary: null,
  selectedBoundaryMeaning: '',
  tilesAvailable: false,
  onSeek: () => {},
  onSelectBoundary: () => {},
  onMoveBoundary: () => {},
  onEditAtPlayback: () => {},
}

describe('RecordingChapterFilmstrip の再生位置線', () => {
  it.each([
    { seconds: 229, expected: false },
    { seconds: 230, expected: true },
    { seconds: 300, expected: true },
    { seconds: 370, expected: true },
    { seconds: 371, expected: false },
  ])('表示範囲 230〜370 秒で再生位置 $seconds 秒の線の有無を判定する', ({ seconds, expected }) => {
    const { container, rerender } = render(<RecordingChapterFilmstrip {...props} />)

    rerender(<RecordingChapterFilmstrip {...props} currentSeconds={seconds} />)

    expect(container.querySelector('[data-testid="chapter-filmstrip-playhead"]') !== null).toBe(expected)
    expect(container.querySelector('[data-testid="chapter-filmstrip-overview-playhead"]')).not.toBeNull()
  })
})

describe('RecordingChapterFilmstrip のホバープレビュー', () => {
  it.each([true, false])('tilesAvailable=%s のときに表示可否を判定する', (tilesAvailable) => {
    const view = render(<RecordingChapterFilmstrip {...props} tilesAvailable={tilesAvailable} />)
    const track = view.getByTestId('chapter-filmstrip-track')
    vi.spyOn(track, 'getBoundingClientRect').mockReturnValue(new DOMRect(20, 0, 400, 40))

    fireEvent.pointerMove(track, { pointerType: 'mouse', clientX: 219 })

    expect(view.queryByTestId('chapter-filmstrip-preview') !== null).toBe(tilesAvailable)
  })

  it('帯のクリックと同じ時刻を使い、その位置の下書き区間名を表示する', () => {
    const onSeek = vi.fn()
    const previewProps = { ...props, onSeek, tilesAvailable: true }
    const view = render(<RecordingChapterFilmstrip {...previewProps} />)
    const track = view.getByTestId('chapter-filmstrip-track')
    vi.spyOn(track, 'getBoundingClientRect').mockReturnValue(new DOMRect(20, 0, 400, 40))
    fireEvent.pointerMove(track, { pointerType: 'mouse', clientX: 219 })
    const label = view.getByTestId('chapter-filmstrip-preview-label')
    expect(label).toHaveTextContent('4:59 · 本編')

    const cut = { startMs: 299_633, endMs: 299_666, cut: true }
    view.rerender(<RecordingChapterFilmstrip {...previewProps} spans={[cut]} />)
    expect(label).toHaveTextContent('4:59 · CM')
    view.rerender(<RecordingChapterFilmstrip {...previewProps} spans={[{ ...cut, label: '短い区間' }]} />)
    expect(label).toHaveTextContent('4:59 · 短い区間')

    fireEvent.pointerDown(track, { pointerType: 'mouse', clientX: 219 })
    fireEvent.click(track, { clientX: 219 })
    expect(onSeek).toHaveBeenCalledExactlyOnceWith(299.65)
    expect(label).toBeInTheDocument()
  })

  it('タッチと境界ドラッグ中は表示せず、帯を離れると閉じる', () => {
    const view = render(<RecordingChapterFilmstrip {...props} tilesAvailable spans={[{ startMs: 300_000, endMs: 310_000, cut: true }]} />)
    const track = view.getByTestId('chapter-filmstrip-track')
    const preview = () => view.queryByTestId('chapter-filmstrip-preview')
    vi.spyOn(track, 'getBoundingClientRect').mockReturnValue(new DOMRect(20, 0, 400, 40))

    fireEvent.pointerMove(track, { pointerType: 'touch', clientX: 220 })
    expect(preview()).toBeNull()
    fireEvent.pointerMove(track, { pointerType: 'mouse', clientX: 220 })
    expect(preview()).toBeInTheDocument()
    fireEvent.pointerDown(view.getAllByTestId('chapter-filmstrip-boundary')[0], { pointerType: 'mouse', clientX: 220 })
    expect(preview()).toBeNull()
    fireEvent.pointerMove(track, { pointerType: 'mouse', clientX: 225, buttons: 1 })
    expect(preview()).toBeNull()
    fireEvent.pointerUp(track, { pointerType: 'mouse', clientX: 225 })

    fireEvent.pointerMove(track, { pointerType: 'mouse', clientX: 225 })
    expect(preview()).toBeInTheDocument()
    fireEvent.pointerLeave(track, { pointerType: 'mouse' })
    expect(preview()).toBeNull()
  })
})
