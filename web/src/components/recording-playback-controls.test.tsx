import { createRef } from 'react'
import type { ComponentProps } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import type { ChapterSpan } from '@/api/generated'
import { RecordingPlaybackControls } from '@/components/recording-playback-controls'
import type { PlaybackAudioOption } from '@/components/recording-playback-controls'
import { fixedPlaybackTimeline, liveProgramPlaybackTimeline } from '@/lib/playback-timeline'

type ControlsProps = ComponentProps<typeof RecordingPlaybackControls>

function controlProps(overrides: Partial<ControlsProps> = {}): ControlsProps {
  const noop = vi.fn()
  return {
    profile: 'hd',
    encodedAssets: [],
    timeline: fixedPlaybackTimeline(100),
    canChangePlaybackRate: true,
    profileOptions: [{ name: 'hd' }, { name: 'sd' }],
    frameRef: createRef<HTMLDivElement>(),
    video: <video />,
    currentSeconds: 0,
    durationSeconds: 100,
    playedFraction: 0,
    chapters: [],
    playingCut: false,
    isPlaying: false,
    muted: false,
    volume: 1,
    playbackRate: 1,
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
    ...overrides,
  }
}

function renderControls(
  playbackRateLocked: boolean,
  chapterEditing = false,
  onRateChange = vi.fn(),
  playbackRate = 1,
  chapters: ChapterSpan[] = [],
  options: { canChangePlaybackRate?: boolean; audioOptions?: readonly PlaybackAudioOption[] } = {},
) {
  const props = controlProps({
    canChangePlaybackRate: options.canChangePlaybackRate ?? true,
    ...(options.audioOptions === undefined ? {} : { audioOptions: options.audioOptions }),
    chapters,
    chapterEditing,
    playbackRate,
    playbackRateLocked,
    onRateChange,
  })
  render(<RecordingPlaybackControls {...props} />)
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

  it('チャプター一覧と再生バーの区間表示は共通の再生時刻書式を使う', () => {
    renderControls(false, false, vi.fn(), 1, [
      { startMs: 330_000, endMs: 360_000, label: 'ニュース', cut: false },
    ])

    expect(screen.getByTestId('chapter-marker')).toHaveAttribute('title', 'ニュース 5:30〜6:00')
    fireEvent.click(screen.getByRole('button', { name: 'チャプター: 本編' }))
    expect(screen.getByRole('menuitemradio', { name: /5:30.*ニュース/ })).toBeInTheDocument()
  })
})

describe('再生元固有の行は渡された事実から出す', () => {
  it('速度を変えられないときは速度行を出さない', () => {
    renderControls(false, false, vi.fn(), 1, [], { canChangePlaybackRate: false })

    expect(screen.queryByRole('menuitem', { name: '再生速度' })).toBeNull()
  })

  it('音声の選択肢を渡したときだけ音声行を出す', () => {
    const audioOptions: readonly PlaybackAudioOption[] = [
      { value: undefined, label: '標準' },
      { value: 'main', label: '主音声' },
      { value: 'sub', label: '副音声' },
    ]
    renderControls(false, false, vi.fn(), 1, [], { audioOptions })

    fireEvent.click(screen.getByRole('menuitem', { name: '音声' }))
    expect(screen.getByRole('menuitemradio', { name: '標準' })).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('menuitemradio', { name: '主音声' })).toBeInTheDocument()
    expect(screen.getByRole('menuitemradio', { name: '副音声' })).toBeInTheDocument()
  })
})

const chaseTimeline = {
  kind: 'chase',
  minSeconds: 0,
  maxSeconds: 100,
  canSeek: true,
  extended: false,
  headSeconds: 0,
  recordedEndSeconds: 80,
  plannedEndSeconds: 80,
  liveEdgeSeconds: 79,
  hoverSeconds: null,
  hoverLabel: null,
} as const

describe('共有シークバーのポインタ操作', () => {
  const setSeekbarRect = (seekbar: HTMLElement) => {
    vi.spyOn(seekbar, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      right: 100,
      top: 0,
      bottom: 20,
      width: 100,
      height: 20,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    } as DOMRect)
  }

  it('ドラッグ中の直接シークは pointercancel で開始位置に戻し、その後の move を無視する', () => {
    const onSeek = vi.fn()
    const onScrubbingChange = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({
      currentSeconds: 8,
      seekDuringDrag: true,
      onScrubStart: () => 10,
      onScrubbingChange,
      onSeek,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 20 })
    fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 70 })
    fireEvent.pointerCancel(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 95 })

    expect(onSeek.mock.calls).toEqual([[20], [70], [10]])
    expect(onScrubbingChange.mock.calls).toEqual([[true], [false]])
    fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 60 })
    expect(onSeek).toHaveBeenCalledTimes(3)

    fireEvent.pointerDown(seekbar, { pointerId: 2, pointerType: 'mouse', clientX: 20 })
    fireEvent.pointerUp(seekbar, { pointerId: 2, pointerType: 'mouse', clientX: 85 })
    expect(onSeek).toHaveBeenLastCalledWith(85)
  })

  it('プレビューだけのスクラブは pointercancel で確定しない', () => {
    const onSeek = vi.fn()
    const onScrubPreview = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({ onSeek, onScrubPreview })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 20 })
    fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 70 })
    fireEvent.pointerCancel(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 95 })

    expect(onScrubPreview.mock.calls).toEqual([[20], [70], [null]])
    expect(onSeek).not.toHaveBeenCalled()
  })

  it('時間軸のホバーは帯を離れたら消える', () => {
    const onTimelineHoverChange = vi.fn()
    const timeline = chaseTimeline
    render(<RecordingPlaybackControls {...controlProps({ timeline, onTimelineHoverChange })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerMove(seekbar, { pointerType: 'mouse', clientX: 60 })
    expect(onTimelineHoverChange).toHaveBeenLastCalledWith(60)
    fireEvent.pointerLeave(seekbar)
    expect(onTimelineHoverChange).toHaveBeenLastCalledWith(null)
  })

  it('掴んでいる間の pointerleave はホバー・プレビュー・タイルを消さない', () => {
    const onTimelineHoverChange = vi.fn()
    const onScrubPreview = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({
      timeline: chaseTimeline,
      recordingId: 92,
      seekTilesEnabled: true,
      onTimelineHoverChange,
      onScrubPreview,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)
    fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 20 })
    fireEvent.load(screen.getByTestId('seek-tiles-image'))
    fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 30 })
    expect(screen.getByTestId('seek-tile-preview')).toBeInTheDocument()
    onTimelineHoverChange.mockClear()
    onScrubPreview.mockClear()

    fireEvent.pointerLeave(seekbar, { pointerId: 1, pointerType: 'mouse' })

    expect(onTimelineHoverChange).not.toHaveBeenCalled()
    expect(onScrubPreview).not.toHaveBeenCalled()
    expect(screen.getByTestId('seek-tile-preview')).toBeInTheDocument()
  })

  it('掴んでいない pointerleave はホバーとタイルだけを消し、キー操作のプレビューには触らない', () => {
    const onTimelineHoverChange = vi.fn()
    const onScrubPreview = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({
      timeline: chaseTimeline,
      recordingId: 92,
      seekTilesEnabled: true,
      onTimelineHoverChange,
      onScrubPreview,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)
    fireEvent.pointerMove(seekbar, { pointerType: 'mouse', clientX: 20 })
    fireEvent.load(screen.getByTestId('seek-tiles-image'))
    expect(screen.getByTestId('seek-tile-preview')).toBeInTheDocument()

    fireEvent.pointerLeave(seekbar, { pointerType: 'mouse' })

    expect(onTimelineHoverChange).toHaveBeenLastCalledWith(null)
    expect(screen.queryByTestId('seek-tile-preview')).toBeNull()
    expect(onScrubPreview).not.toHaveBeenCalled()
  })

  it('追っかけ軸: タッチで掴んでいる間はホバーを出し、離すと消す', () => {
    const onTimelineHoverChange = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({ timeline: chaseTimeline, onTimelineHoverChange })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 20 })
    fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 40 })
    expect(onTimelineHoverChange.mock.calls).toEqual([[20], [40]])
    fireEvent.pointerUp(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 40 })
    expect(onTimelineHoverChange).toHaveBeenLastCalledWith(null)
  })

  describe('ライブ番組軸', () => {
    // 軸は 0〜3600 秒、録画済み（選択可能）は 0〜600 秒。幅 100 なので 1px = 36 秒。
    const live = liveProgramPlaybackTimeline({
      startAt: '2026-01-01T00:50:00+09:00',
      endAt: '2026-01-01T01:50:00+09:00',
      nowMs: Date.parse('2026-01-01T01:00:00+09:00'),
      recordingId: 9,
      recordingStartedAt: '2026-01-01T00:50:00+09:00',
      hoverSeconds: null,
    })!

    it('タッチで掴んでいる間は選択可能範囲の中だけホバーを出し、離すと消す', () => {
      const onTimelineHoverChange = vi.fn()
      render(<RecordingPlaybackControls {...controlProps({ timeline: live, onTimelineHoverChange })} />)
      const seekbar = screen.getByTestId('live-program-timeline')
      setSeekbarRect(seekbar)

      fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 10 })
      expect(onTimelineHoverChange).toHaveBeenLastCalledWith(360)
      // 録画済みの先（ライブ端より後ろ）は選べないのでホバーも出さない。
      fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 50 })
      expect(onTimelineHoverChange).toHaveBeenLastCalledWith(null)
      fireEvent.pointerMove(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 12 })
      expect(onTimelineHoverChange).toHaveBeenLastCalledWith(432)
      fireEvent.pointerUp(seekbar, { pointerId: 1, pointerType: 'touch', clientX: 12 })
      expect(onTimelineHoverChange).toHaveBeenLastCalledWith(null)
    })

    it('選択可能範囲の外では掴めず、確定もしない', () => {
      const onSeek = vi.fn()
      const onScrubbingChange = vi.fn()
      render(<RecordingPlaybackControls {...controlProps({ timeline: live, onSeek, onScrubbingChange })} />)
      const seekbar = screen.getByTestId('live-program-timeline')
      setSeekbarRect(seekbar)

      fireEvent.pointerDown(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 50 })
      fireEvent.pointerUp(seekbar, { pointerId: 1, pointerType: 'mouse', clientX: 50 })

      expect(onScrubbingChange).not.toHaveBeenCalled()
      expect(onSeek).not.toHaveBeenCalled()
    })
  })

  it('空の変換表（tileTimeAtSeconds が null）ではタイルを問い合わせない', () => {
    render(<RecordingPlaybackControls {...controlProps({
      recordingId: 92,
      seekTilesEnabled: true,
      tileTimeAtSeconds: () => null,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerMove(seekbar, { pointerType: 'mouse', clientX: 20 })

    expect(screen.queryByTestId('seek-tiles-image')).toBeNull()
  })

  it('タイルはマウスホバー時だけ要求し、ラベルと座標変換を共有部品で保つ', () => {
    render(<RecordingPlaybackControls {...controlProps({
      recordingId: 92,
      seekTilesEnabled: true,
      tileTimeAtSeconds: (seconds) => seconds + 70,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerMove(seekbar, { pointerType: 'touch', clientX: 20 })
    expect(screen.queryByTestId('seek-tiles-image')).toBeNull()

    fireEvent.pointerMove(seekbar, { pointerType: 'mouse', clientX: 20 })
    const image = screen.getByTestId('seek-tiles-image')
    expect(image).toHaveAttribute('src', '/api/media/recordings/92/seek-tiles')
    fireEvent.load(image!)

    expect(screen.getByTestId('seek-tile-label')).toHaveTextContent('0:20')
    expect(screen.getByTestId('seek-tile-preview').querySelector('.bg-no-repeat')).toHaveStyle({
      backgroundPosition: '-288px 0px',
    })
  })

  it('キーボード seek は前のポインタ位置のタイルプレビューを消す', () => {
    const onSeekPreview = vi.fn()
    render(<RecordingPlaybackControls {...controlProps({
      recordingId: 92,
      seekTilesEnabled: true,
      deferKeyboardSeek: true,
      onSeekPreview,
    })} />)
    const seekbar = screen.getByTestId('seek-scrub')
    setSeekbarRect(seekbar)

    fireEvent.pointerMove(seekbar, { pointerType: 'mouse', clientX: 20 })
    fireEvent.load(screen.getByTestId('seek-tiles-image'))
    expect(screen.getByTestId('seek-tile-preview')).toBeInTheDocument()

    fireEvent.keyDown(seekbar, { key: 'ArrowRight' })

    expect(onSeekPreview).toHaveBeenLastCalledWith(10)
    expect(screen.queryByTestId('seek-tile-preview')).toBeNull()
  })

  it('編集画面の先読みは時間軸の尺が未確定でも行う', () => {
    render(<RecordingPlaybackControls {...controlProps({
      recordingId: 92,
      timeline: fixedPlaybackTimeline(0),
      durationSeconds: 0,
      seekTilesEnabled: true,
      requestSeekTilesOnMount: true,
    })} />)

    expect(screen.getByTestId('seek-tiles-image')).toHaveAttribute(
      'src',
      '/api/media/recordings/92/seek-tiles',
    )
  })
})
