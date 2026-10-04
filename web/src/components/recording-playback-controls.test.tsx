import { createRef } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { RecordingPlaybackControls } from '@/components/recording-playback-controls'

function renderControls(playbackRateLocked: boolean, chapterEditing = false, onRateChange = vi.fn(), playbackRate = 1) {
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
    chapterEditing,
    tilePreview: { url: null },
    tilesRequested: false,
    tilesAvailable: false,
    isPlaying: false,
    muted: false,
    volume: 1,
    playbackRate,
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
    onRateChange,
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
  if (chapterEditing) return undefined
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

  it('編集帯の速度巡回は変換中に無効になる', () => {
    renderControls(true, true)
    expect(screen.getByTestId('chapter-edit-playback-rate')).toBeDisabled()
  })

  it('編集帯の速度巡回は通常の速度一覧を進む', () => {
    const onRateChange = vi.fn()
    renderControls(false, true, onRateChange)
    const rate = screen.getByTestId('chapter-edit-playback-rate')
    expect(rate).toBeEnabled()
    fireEvent.click(rate)
    expect(onRateChange).toHaveBeenCalledWith(1.25)
  })

  it('編集帯の速度巡回は一覧外の速度から次に大きい速度へ進む', () => {
    const onRateChange = vi.fn()
    renderControls(false, true, onRateChange, 1.1)
    fireEvent.click(screen.getByTestId('chapter-edit-playback-rate'))
    expect(onRateChange).toHaveBeenCalledWith(1.25)
  })
})
