import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { RecordingChapterFilmstrip } from '@/components/recording-chapter-filmstrip'

describe('RecordingChapterFilmstrip の再生位置線', () => {
  it.each([
    { seconds: 229, expected: false },
    { seconds: 230, expected: true },
    { seconds: 300, expected: true },
    { seconds: 370, expected: true },
    { seconds: 371, expected: false },
  ])('表示範囲 230〜370 秒で再生位置 $seconds 秒の線の有無を判定する', ({ seconds, expected }) => {
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
    }
    const { container, rerender } = render(<RecordingChapterFilmstrip {...props} />)

    rerender(<RecordingChapterFilmstrip {...props} currentSeconds={seconds} />)

    expect(container.querySelector('[data-testid="chapter-filmstrip-playhead"]') !== null).toBe(expected)
    expect(container.querySelector('[data-testid="chapter-filmstrip-overview-playhead"]')).not.toBeNull()
  })
})
