import { createRef } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RecordingPlaybackControls } from '@/components/recording-playback-controls'

function renderControls(playbackRateLocked: boolean) {
  const noop = vi.fn()
  const props = {
    profile: 'hd',
    encodedAssets: [],
    playbackMode: 'chase',
    profileOptions: [{ name: 'hd' }, { name: 'sd' }],
    fullscreenRef: createRef<HTMLDivElement>(),
    video: <video />,
    currentSeconds: 0,
    durationSeconds: 100,
    playedFraction: 0,
    chapters: [],
    playingCut: false,
    tilePreview: { url: null },
    tilesRequested: false,
    tilesAvailable: false,
    isPlaying: false,
    muted: false,
    volume: 1,
    playbackRate: 1,
    playbackRateLocked,
    subtitlesEnabled: false,
    skipEnabled: false,
    pictureInPicture: false,
    isFullscreen: false,
    showWatched: false,
    watched: false,
    watchedPending: false,
    controlsVisible: true,
    onTileImageLoad: noop,
    onTileImageError: noop,
    onSeekPointerDown: noop,
    onSeekPointerMove: noop,
    onSeekPointerUp: noop,
    onSeekPointerLeave: noop,
    onSeek: noop,
    onSelectProfile: noop,
    onPreviousChapter: noop,
    onNextChapter: noop,
    onTogglePlay: noop,
    onToggleMute: noop,
    onVolumeChange: noop,
    onRateChange: noop,
    onToggleSubtitles: noop,
    onToggleSkip: noop,
    onTogglePictureInPicture: noop,
    onToggleFullscreen: noop,
    onControlsActivity: noop,
    onHideControls: noop,
    onToolbarFocus: noop,
    onToolbarBlur: noop,
    onShellKeyDown: noop,
  }
  render(<RecordingPlaybackControls {...(props as unknown as Parameters<typeof RecordingPlaybackControls>[0])} />)
  fireEvent.click(screen.getByRole('button', { name: '再生設定' }))
  return screen.getByRole('menu', { name: '再生設定' })
}

describe('速度メニュー行（変換中の固定）', () => {
  it('固定中は aria-disabled でフォーカスに乗り、↓ で通り過ぎ、入れない', () => {
    const menu = renderControls(true)
    const rate = screen.getByRole('menuitem', { name: '再生速度' })
    expect(rate).toHaveAttribute('aria-disabled', 'true')
    expect(rate).not.toBeDisabled()

    // 字幕 -> 再生速度 -> 画質。速度の行にも止まり、さらに ↓ で先へ進める。
    screen.getByRole('menuitemcheckbox', { name: '字幕' }).focus()
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' })
    expect(document.activeElement).toBe(rate)
    fireEvent.keyDown(rate, { key: 'ArrowDown' })
    expect(document.activeElement).toBe(screen.getByRole('menuitem', { name: '画質' }))

    rate.focus()
    fireEvent.click(rate)
    fireEvent.keyDown(rate, { key: 'Enter' })
    fireEvent.keyDown(rate, { key: 'ArrowRight' })
    expect(screen.getByRole('menu', { name: '再生設定' })).toBe(menu)
    expect(screen.queryByRole('menuitemradio', { name: '2x' })).toBeNull()
  })

  it('固定していなければ速度メニューへ入れる', () => {
    renderControls(false)
    const rate = screen.getByRole('menuitem', { name: '再生速度' })
    expect(rate).not.toHaveAttribute('aria-disabled')
    fireEvent.click(rate)
    expect(screen.getByRole('menuitemradio', { name: '2x' })).toBeInTheDocument()
  })
})
