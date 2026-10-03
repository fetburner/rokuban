import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type FocusEvent as ReactFocusEvent,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
  type RefObject,
} from 'react'

import { Link } from '@tanstack/react-router'

import type { ChapterSpan, EncodedAsset } from '@/api/generated'
import type { LiveAudioChoice } from '@/lib/live'
import {
  Activity,
  Captions,
  Check,
  ChevronLeft,
  ChevronRight,
  FastForward,
  Gauge,
  Maximize,
  Minimize,
  Pause,
  PictureInPicture2,
  Play,
  Settings,
  SkipBack,
  SkipForward,
  Scissors,
  SlidersHorizontal,
  Volume2,
  VolumeX,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { formatChaptersTime } from '@/lib/chapters'
import { formatBytes, formatDate, formatPlaybackTime, formatPlaybackTimeMs } from '@/lib/format'
import { cn } from '@/lib/utils'
import {
  SEEK_TILES_DISPLAY_HEIGHT,
  SEEK_TILES_DISPLAY_WIDTH,
  seekTileBackgroundSize,
  seekTilesURL,
} from '@/lib/seek-tiles'

export type TilePreview = {
  x: number
  y: number
  left: number
  scale: number
  /** ホバー位置の再生位置（秒）。タイルの下の時刻ラベルに使う。 */
  seconds: number
} | null

/** ChaseTimeline は追っかけのシークバーの軸（すべて番組開始からの秒。負は番組開始より前）。 */
export type ChaseTimeline = {
  minSeconds: number
  maxSeconds: number
  /** 録画の始まり。これより前は録っていない。 */
  headSeconds: number
  recordedEndSeconds: number
  plannedEndSeconds: number
  /** 先端の印（録画済みの 1 秒手前）。 */
  liveEdgeSeconds: number
  /** マウスを載せている位置（吹き出しだけを出す。つまみと時刻は動かさない）。 */
  hoverSeconds: number | null
}

/** 設定メニューの階層。`main` が行リストで、残りは「›」で入る下の階層。 */
type MenuView = 'main' | 'speed' | 'quality' | 'audio'

const PLAYBACK_RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2]

type RecordingPlaybackControlsProps = {
  recordingId: number
  profile: string
  encodedAssets: EncodedAsset[]
  playbackMode?: 'encoded' | 'original-vod' | 'chase'
  chaseTimeline?: ChaseTimeline
  profileOptions?: readonly { name: string; label?: string }[]
  audioChoice?: LiveAudioChoice
  onSelectAudio?: (choice: LiveAudioChoice | undefined) => void
  /** 次のエピソード（再生できる行だけ）。バーの右側に「次: 10/1(水)」で出す。 */
  nextEpisode?: { id: number; title: string; startAt: string }
  /** 次のエピソードのリンクを押したとき（移動先の詳細を先にキャッシュへ入れる）。 */
  onNextEpisodeNavigate?: () => void
  /** 番組枠の外を録った部分（シークバー内の割合）。カット版の再生中は描かない。 */
  outsideProgramSegments?: { beforeEndPercent: number; afterStartPercent: number }
  /** 映像の上に重ねる終端カード。 */
  endCard?: ReactNode
  fullscreenRef: RefObject<HTMLDivElement | null>
  video: ReactNode
  currentSeconds: number
  durationSeconds: number
  playedFraction: number
  chapters: ChapterSpan[]
  playingCut: boolean
  chapterEditing?: boolean
  canEditChapters?: boolean
  onEnterChapterEditing?: () => void
  onPlayAround?: () => void
  tilePreview: TilePreview
  tilesRequested: boolean
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  onSeekPointerDown: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerMove: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerUp: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerCancel?: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerLeave: () => void
  onSeek: (seconds: number) => void
  onLiveEdgeSeek?: () => void
  deferKeyboardSeek?: boolean
  onSeekPreview?: (seconds: number) => void
  onSelectProfile: (profile: string) => void
  onPreviousChapter: () => void
  onNextChapter: () => void
  isPlaying: boolean
  muted: boolean
  volume: number
  playbackRate: number
  subtitlesEnabled: boolean
  skipEnabled: boolean
  pictureInPicture: boolean
  isFullscreen: boolean
  showWatched: boolean
  watched: boolean
  watchedPending: boolean
  onPutWatched?: () => void
  onDeleteWatched?: () => void
  onTogglePlay: () => void
  onToggleMute: () => void
  onVolumeChange: (volume: number) => void
  onRateChange: (rate: number) => void
  onToggleSubtitles: () => void
  onToggleSkip: (enabled: boolean) => void
  onTogglePictureInPicture: () => void
  onToggleFullscreen: () => void
  controlsVisible: boolean
  onControlsActivity: () => void
  /** タッチで映像の暗い幕を叩いたとき（スマホの操作表示を閉じる）。 */
  onHideControls: () => void
  onToolbarFocus: (event: ReactFocusEvent<HTMLElement>) => void
  onToolbarBlur: (event: ReactFocusEvent<HTMLElement>) => void
  /** shell 内のキー入力（Tab を含む）。隠れたバーを出してから Tab を処理させる。 */
  onShellKeyDown: () => void
}

/**
 * RecordingPlaybackControls は encoded VOD の再生操作と単一タイムラインを描画する。
 *
 * md 未満（スマホ）とそれ以上で同じ要素の置き場だけを CSS で変える。ボタンを
 * 2 組持たないので、状態とアクセシブル名は 1 つに保たれる。
 */
export function RecordingPlaybackControls({
  recordingId,
  profile,
  encodedAssets,
  playbackMode = 'encoded',
  chaseTimeline,
  profileOptions,
  audioChoice,
  onSelectAudio,
  nextEpisode,
  onNextEpisodeNavigate,
  outsideProgramSegments,
  endCard,
  fullscreenRef,
  video,
  currentSeconds,
  durationSeconds,
  playedFraction,
  chapters,
  playingCut,
  chapterEditing = false,
  canEditChapters = false,
  onEnterChapterEditing,
  onPlayAround,
  tilePreview,
  tilesRequested,
  tilesAvailable,
  onTileImageLoad,
  onTileImageError,
  onSeekPointerDown,
  onSeekPointerMove,
  onSeekPointerUp,
  onSeekPointerCancel,
  onSeekPointerLeave,
  onSeek,
  onLiveEdgeSeek,
  deferKeyboardSeek = false,
  onSeekPreview,
  onSelectProfile,
  onPreviousChapter,
  onNextChapter,
  isPlaying,
  muted,
  volume,
  playbackRate,
  subtitlesEnabled,
  skipEnabled,
  pictureInPicture,
  isFullscreen,
  showWatched,
  watched,
  watchedPending,
  onPutWatched,
  onDeleteWatched,
  onTogglePlay,
  onToggleMute,
  onVolumeChange,
  onRateChange,
  onToggleSubtitles,
  onToggleSkip,
  onTogglePictureInPicture,
  onToggleFullscreen,
  controlsVisible,
  onControlsActivity,
  onHideControls,
  onToolbarFocus,
  onToolbarBlur,
  onShellKeyDown,
}: RecordingPlaybackControlsProps) {
  const [menuView, setMenuView] = useState<MenuView | null>(null)
  const [chaptersOpen, setChaptersOpen] = useState(false)
  const menuOpen = menuView !== null
  // 設定メニューとチャプター一覧は同じ置き場（小窓 / シート）を使うので、同時には開かない。
  const popoverOpen = menuOpen || chaptersOpen
  const gearRef = useRef<HTMLButtonElement>(null)
  const chapterButtonRef = useRef<HTMLButtonElement>(null)
  // 幕を押したポインタの種類（click には pointerType が載らないブラウザがある）。
  const scrimPointerTypeRef = useRef('')
  const pendingKeyboardSeek = useRef<number | null>(null)
  const rangeMin = chaseTimeline?.minSeconds ?? 0
  const rangeMax = chaseTimeline?.maxSeconds ?? durationSeconds
  const range = Math.max(0, rangeMax - rangeMin)
  const seconds = Math.max(rangeMin, Math.min(rangeMax, currentSeconds))
  const axisFraction = (value: number) => (range > 0 ? Math.max(0, Math.min(1, (value - rangeMin) / range)) : 0)
  const seekFraction = axisFraction(seconds)
  // 追っかけの時刻は番組の長さに合わせて分で数える（ラフ: 「60:00」「70:12」）。
  const formatAxisTime = (value: number) => formatPlaybackTime(value, !chaseTimeline)
  const chaseExtended = chaseTimeline !== undefined && chaseTimeline.recordedEndSeconds > chaseTimeline.plannedEndSeconds
  const volumeValue = muted ? 0 : volume
  const hasChapters = !playingCut && chapters.length > 0
  const pictureInPictureEnabled =
    typeof document !== 'undefined' && document.pictureInPictureEnabled === true

  // 目盛りの中ほどのラベル（先端 / 予定終端）を印の位置に置く。印に合わせると行からはみ出すなら
  // 行の中へ寄せ、左端の「0:00」と重なるなら左端を隠す。延長中は右端の先端ラベルを隠さず、予定の
  // ラベルをその手前まで寄せる。通常時は右端の「… まで（予定）」と重なるなら右端を隠す。
  // 幅は描画ごと（録画中は毎秒）と行の大きさが変わったとき（全画面・窓の幅）に測り直す。
  const axisLabelsRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    const row = axisLabelsRef.current
    if (!row) return
    const place = () => {
      const mark = row.querySelector<HTMLElement>('[data-axis-mark]')
      const start = row.querySelector<HTMLElement>('[data-axis-start]')
      const end = row.querySelector<HTMLElement>('[data-axis-end]')
      if (!mark || !start || !end) return
      const gap = 8
      const rowWidth = row.clientWidth
      const width = mark.offsetWidth
      const pointer = mark.querySelector<HTMLElement>('[data-axis-pointer]')
      const anchor = pointer ? pointer.offsetLeft + pointer.offsetWidth / 2 : width / 2
      const rightLimit = (chaseExtended ? rowWidth - end.offsetWidth - gap : rowWidth) - width
      const left = Math.max(0, Math.min(rightLimit, Number(mark.dataset.axisMark) * rowWidth - anchor))
      mark.style.left = `${left}px`
      start.style.visibility = left < start.offsetWidth + gap ? 'hidden' : ''
      end.style.visibility = !chaseExtended && left + width > rowWidth - end.offsetWidth - gap ? 'hidden' : ''
    }
    place()
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(place)
    observer.observe(row)
    return () => observer.disconnect()
  })

  const closeMenu = (focusGear: boolean) => {
    setMenuView(null)
    if (focusGear) gearRef.current?.focus()
  }
  const closeChapters = (focusButton: boolean) => {
    setChaptersOpen(false)
    if (focusButton) chapterButtonRef.current?.focus()
  }

  // 開いている小窓の外を押したら閉じる（開いた本人のボタンは自分で開閉するので除く）。
  // スマホの幕もここで閉じる。
  useEffect(() => {
    if (!popoverOpen) return
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node
      const popover = fullscreenRef.current?.querySelector('[data-player-popover]')
      if (popover?.contains(target) || gearRef.current?.contains(target) || chapterButtonRef.current?.contains(target)) {
        return
      }
      setMenuView(null)
      setChaptersOpen(false)
    }
    document.addEventListener('pointerdown', onPointerDown, true)
    return () => document.removeEventListener('pointerdown', onPointerDown, true)
  }, [popoverOpen, fullscreenRef])

  const seekByKeyboard = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    let target: number | undefined
    switch (event.key) {
      case 'ArrowLeft':
        target = (pendingKeyboardSeek.current ?? seconds) - 10
        break
      case 'ArrowRight':
        target = (pendingKeyboardSeek.current ?? seconds) + 10
        break
      case 'Home':
        target = rangeMin
        break
      case 'End':
        target = chaseTimeline?.liveEdgeSeconds ?? durationSeconds
        break
      default:
        return
    }
    event.preventDefault()
    event.stopPropagation()
    const bounded = Math.max(rangeMin, Math.min(rangeMax, target))
    if (deferKeyboardSeek) {
      pendingKeyboardSeek.current = bounded
      onSeekPreview?.(bounded)
    } else {
      onSeek(bounded)
    }
  }

  const finishKeyboardSeek = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (!deferKeyboardSeek || pendingKeyboardSeek.current === null) return
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
    event.preventDefault()
    event.stopPropagation()
    onSeek(pendingKeyboardSeek.current)
    pendingKeyboardSeek.current = null
  }

  // フォーカスが帯を離れたら、キーを離す前でも保留中のシークを確定する（捨てない）。
  const commitPendingKeyboardSeek = () => {
    if (!deferKeyboardSeek || pendingKeyboardSeek.current === null) return
    onSeek(pendingKeyboardSeek.current)
    pendingKeyboardSeek.current = null
  }

  const hoverSpan =
    tilePreview && !playingCut
      ? chapters.find((span) => tilePreview.seconds * 1000 >= span.startMs && tilePreview.seconds * 1000 < span.endMs)
      : undefined
  // 区間の隙間が本編（chapters は本編の区間を持たない。recording-player.tsx の props 注記）。
  const entries = hasChapters ? chapterEntries(chapters, durationSeconds) : []
  const currentEntryIndex = entries.findIndex((entry) => seconds >= entry.start && seconds < entry.end)
  const currentChapterName = hasChapters ? (entries[currentEntryIndex]?.label ?? '本編') : undefined
  const ghost = 'text-white hover:bg-white/15 hover:text-white'
  const watchedAction = watched ? onDeleteWatched : onPutWatched
  const showControls = controlsVisible || popoverOpen

  return (
    <div
      className="relative w-full"
      data-testid="recording-player-shell"
      onPointerMove={chapterEditing ? undefined : onControlsActivity}
      onKeyDown={chapterEditing ? undefined : onShellKeyDown}
    >
      <div
        ref={fullscreenRef}
        data-testid="recording-player-frame"
        className="relative aspect-video w-full overflow-hidden rounded bg-black"
        onPointerMove={onControlsActivity}
      >
        {video}
        {endCard}
        {chapterEditing ? (
          // 編集中は操作バーを簡素な 1 本に差し替える。**`<video>` は同じ場所に置いたままにする**
          // （別の木に描くと作り直され、再生位置が 0 に戻って止まる）。
          <div className="absolute inset-x-0 bottom-0 flex items-center gap-3 bg-gradient-to-t from-black/85 to-transparent px-3 pt-8 pb-2 text-white">
            <Button type="button" variant="ghost" size="icon" className={ghost} aria-label={isPlaying ? '一時停止' : '再生'} onClick={onTogglePlay}>
              {isPlaying ? <Pause /> : <Play />}
            </Button>
            <span data-testid="chapter-edit-playhead" className="font-mono text-sm">
              {formatPlaybackTimeMs(seconds)}
              <span className="hidden md:inline"> / {formatPlaybackTime(durationSeconds)}</span>
            </span>
            <Button
              type="button"
              variant="secondary"
              className="ml-auto rounded-full"
              onClick={onPlayAround}
              disabled={onPlayAround === undefined}
            >
              <span className="md:hidden">前後 3 秒</span>
              <span className="hidden md:inline">前後 3 秒を再生</span>
            </Button>
            <span className="hidden rounded-full bg-white/15 px-3 py-2 text-sm md:inline-flex" aria-label="CM自動スキップは編集中に停止">
              CM を飛ばさない（編集中）
            </span>
          </div>
        ) : (
        <>
        {/*
          スマホ（md 未満）では枠全体に暗い幕を敷き、中央に前後チャプターと再生、右上に CC と
          歯車、下に時刻・✓・全画面とシークバーを置く。md 以上は下端の帯 1 本にまとめる。
        */}
        <div
          data-testid="player-controls"
          className={cn(
            'absolute inset-0 z-10 flex flex-col justify-end bg-black/25 text-white transition-opacity duration-150',
            'md:top-auto md:bg-transparent md:bg-gradient-to-t md:from-black/95 md:via-black/70 md:to-transparent md:px-3 md:pt-10 md:pb-1',
            showControls ? 'opacity-100' : 'pointer-events-none opacity-0',
          )}
          aria-hidden={!showControls}
          inert={!showControls}
          onFocusCapture={onToolbarFocus}
          onBlurCapture={onToolbarBlur}
          onPointerDown={(event) => {
            scrimPointerTypeRef.current = event.pointerType
          }}
          // 幕そのものを押したときだけ（ボタンは除く）。pointerup で幕を消すと、続く click が
          // 下の <video> に落ちて再生 / 一時停止してしまうので、click で処理する。
          // タッチは操作を閉じ、マウスは映像のクリックと同じく再生 / 一時停止する。
          onClick={(event) => {
            if (event.target !== event.currentTarget) return
            if (scrimPointerTypeRef.current === 'mouse') onTogglePlay()
            else onHideControls()
          }}
        >
          <div
            data-testid="player-controls-bottom"
            className="flex flex-col bg-gradient-to-t from-black/80 to-transparent px-2.5 pt-8 pb-1 md:bg-none md:p-0"
          >
            <div
              data-testid={chaseTimeline ? 'chase-timeline-track' : undefined}
              className={cn('order-last mt-1 md:order-none md:mt-0 md:mb-1', chaseTimeline && 'relative')}
            >
              <div
                role="slider"
                aria-label="シークバー"
                aria-valuemin={rangeMin}
                aria-valuemax={Math.max(rangeMin, rangeMax)}
                aria-valuenow={seconds}
                aria-valuetext={chaseTimeline
                  ? `${formatAxisTime(seconds)} / 録画済み ${formatAxisTime(chaseTimeline.recordedEndSeconds)}`
                  : `${formatPlaybackTime(seconds)} / ${formatPlaybackTime(durationSeconds)}`}
                tabIndex={0}
                data-testid="seek-scrub"
                className={cn(
                  'group relative cursor-pointer touch-none outline-none focus-visible:ring-2 focus-visible:ring-white',
                  chaseTimeline ? 'h-6' : 'h-4',
                )}
                onKeyDown={seekByKeyboard}
                onKeyUp={finishKeyboardSeek}
                onBlur={commitPendingKeyboardSeek}
                onPointerDown={onSeekPointerDown}
                onPointerMove={onSeekPointerMove}
                onPointerUp={onSeekPointerUp}
                onPointerCancel={onSeekPointerCancel ?? onSeekPointerUp}
                onPointerLeave={onSeekPointerLeave}
              >
                {chaseTimeline ? (
                  // 追っかけ: 録っていない部分（録画開始より前・先端より後ろ）は点線、録画済みは灰、
                  // 見たところ（録画開始からつまみまで）は白。
                  <div className="pointer-events-none absolute inset-x-0 top-1/2 h-1 -translate-y-1/2">
                    {[
                      { id: 'before', left: 0, right: axisFraction(chaseTimeline.headSeconds) },
                      { id: 'after', left: axisFraction(chaseTimeline.recordedEndSeconds), right: 1 },
                    ]
                      .filter((segment) => segment.right > segment.left)
                      .map((segment) => (
                        <div
                          key={segment.id}
                          data-testid={`chase-timeline-unrecorded-${segment.id}`}
                          className="absolute inset-y-0 bg-[repeating-linear-gradient(to_right,white_0_3px,transparent_3px_6px)] opacity-40"
                          style={{ left: `${segment.left * 100}%`, width: `${(segment.right - segment.left) * 100}%` }}
                        />
                      ))}
                    <div
                      data-testid="chase-timeline-recorded"
                      className="absolute inset-y-0 bg-white/40"
                      style={{
                        left: `${axisFraction(chaseTimeline.headSeconds) * 100}%`,
                        width: `${(axisFraction(chaseTimeline.recordedEndSeconds) - axisFraction(chaseTimeline.headSeconds)) * 100}%`,
                      }}
                    />
                    <div
                      data-testid="chase-timeline-played"
                      className="absolute inset-y-0 bg-white"
                      style={{
                        left: `${axisFraction(chaseTimeline.headSeconds) * 100}%`,
                        width: `${Math.max(0, seekFraction - axisFraction(chaseTimeline.headSeconds)) * 100}%`,
                      }}
                    />
                  </div>
                ) : (
                  <div className="pointer-events-none absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 overflow-hidden rounded-full bg-white/30">
                    <div className="h-full bg-white" style={{ width: `${playedFraction * 100}%` }} />
                  </div>
                )}
                {chaseTimeline && chaseTimeline.hoverSeconds !== null && (
                  <div
                    data-testid="chase-hover-label"
                    className="pointer-events-none absolute bottom-full z-20 mb-2 rounded bg-black/85 px-2 py-1 text-xs whitespace-nowrap"
                    style={{
                      left: `${axisFraction(chaseTimeline.hoverSeconds) * 100}%`,
                      transform: `translateX(-${axisFraction(chaseTimeline.hoverSeconds) * 100}%)`,
                    }}
                  >
                    <span className="font-mono">{formatAxisTime(chaseTimeline.hoverSeconds)}</span>
                    {chaseTimeline.hoverSeconds > chaseTimeline.recordedEndSeconds || chaseTimeline.hoverSeconds < chaseTimeline.headSeconds
                      ? ' · まだ録画されていません'
                      : ''}
                  </div>
                )}
                {outsideProgramSegments && !playingCut && (
                  <div
                    data-testid="recorded-outside-program-range"
                    aria-hidden="true"
                    className="pointer-events-none absolute inset-x-0 top-1/2 z-10 h-1 -translate-y-1/2"
                  >
                    {[
                      { id: 'before', left: 0, width: outsideProgramSegments.beforeEndPercent },
                      {
                        id: 'after',
                        left: outsideProgramSegments.afterStartPercent,
                        width: 100 - outsideProgramSegments.afterStartPercent,
                      },
                    ]
                      .filter((segment) => segment.width > 0)
                      .map((segment) => (
                        // 番組の外は実線のバーを破線にして「番組ではない部分」と読ませる（┄┄）。
                        <div
                          key={segment.id}
                          data-testid={`recorded-${segment.id}-program`}
                          className="absolute inset-y-0 bg-black/70 bg-[repeating-linear-gradient(to_right,white_0_3px,transparent_3px_6px)]"
                          style={{ left: `${segment.left}%`, width: `${segment.width}%` }}
                        />
                      ))}
                  </div>
                )}
                {chapters.length > 0 && !playingCut && durationSeconds > 0 && (
                  <div className="pointer-events-none absolute inset-x-0 top-1/2 h-1 -translate-y-1/2">
                    {chapters.map((span) => {
                      const left = (span.startMs / 1000 / durationSeconds) * 100
                      const width = ((span.endMs - span.startMs) / 1000 / durationSeconds) * 100
                      return (
                        <div
                          key={`${span.startMs}-${span.endMs}`}
                          data-testid="chapter-marker"
                          data-cut={span.cut ? 'true' : 'false'}
                          title={`${chapterLabel(span)} ${formatChaptersTime(
                            span.startMs / 1000,
                          )}–${formatChaptersTime(span.endMs / 1000)}`}
                          className={`absolute -inset-y-0.5 min-w-0.5 rounded-sm ${span.cut ? 'bg-orange-400' : 'bg-sky-300'}`}
                          style={{ left: `${left}%`, width: `${width}%` }}
                        />
                      )
                    })}
                  </div>
                )}
                <div
                  data-testid="seek-thumb"
                  className="pointer-events-none absolute top-1/2 size-3 -translate-x-1/2 -translate-y-1/2 rounded-full bg-white shadow"
                  style={{ left: `${(chaseTimeline ? seekFraction : playedFraction) * 100}%` }}
                />
                {!playingCut && tilesRequested && (
                  <img
                    src={seekTilesURL(recordingId)}
                    alt=""
                    className="pointer-events-none absolute size-px opacity-0"
                    onLoad={onTileImageLoad}
                    onError={onTileImageError}
                  />
                )}
                {tilePreview && tilesAvailable && (
                  <div
                    className="pointer-events-none absolute bottom-full z-20 mb-2 flex origin-bottom-left flex-col items-center gap-1"
                    style={{
                      left: tilePreview.left,
                      width: SEEK_TILES_DISPLAY_WIDTH,
                      transform: tilePreview.scale < 1 ? `scale(${tilePreview.scale})` : undefined,
                    }}
                  >
                    <div
                      data-testid="seek-tile-preview"
                      className="overflow-hidden rounded border border-white/30 bg-black shadow-lg"
                      style={{ width: SEEK_TILES_DISPLAY_WIDTH, height: SEEK_TILES_DISPLAY_HEIGHT }}
                    >
                      <div
                        className="h-full w-full bg-no-repeat"
                        style={{
                          backgroundImage: `url(${seekTilesURL(recordingId)})`,
                          backgroundPosition: `${tilePreview.x}px ${tilePreview.y}px`,
                          backgroundSize: seekTileBackgroundSize(),
                        }}
                      />
                    </div>
                    <span data-testid="seek-tile-label" className="rounded bg-black/80 px-1.5 text-xs">
                      {formatPlaybackTime(tilePreview.seconds)}
                      {hoverSpan ? ` · ${chapterLabel(hoverSpan)}` : ''}
                    </span>
                  </div>
                )}
              </div>
              {chaseTimeline && (
                <>
                  <div
                    aria-hidden="true"
                    data-testid="chase-timeline-planned-end"
                    className="pointer-events-none absolute top-1/2 z-[2] h-4 -translate-y-1/2 border-l-2 border-dashed border-white"
                    style={{ left: `${axisFraction(chaseTimeline.plannedEndSeconds) * 100}%` }}
                  />
                  <button
                    type="button"
                    data-testid="chase-live-edge"
                    aria-label="録画の先端へ"
                    title={`録画の先端 ${formatAxisTime(chaseTimeline.recordedEndSeconds)}（押すと先端へ）`}
                    onPointerDown={(event) => event.stopPropagation()}
                    onClick={(event) => {
                      event.stopPropagation()
                      onLiveEdgeSeek?.()
                    }}
                    className="absolute top-1/2 z-[3] size-6 -translate-x-1/2 -translate-y-1/2 rounded-full focus-visible:outline-2 focus-visible:outline-white"
                    style={{ left: `${axisFraction(chaseTimeline.liveEdgeSeconds) * 100}%` }}
                  >
                    <span aria-hidden="true" className="mx-auto block h-4 w-0.5 bg-red-500" />
                  </button>
                </>
              )}
            </div>
            {/*
              軸の目盛り（デスクトップだけ。スマホは時刻の表示で足りる）。左端と右端のほかに、通常は先端の
              時刻だけを先端の印の真下に（「押すと先端へ」の説明は印のアクセシブル名と title に置く）、
              延長中は「予定 … ¦」の ¦ を予定終端の印に合わせて置く（ラフ 7。ラフ 6 の文言は利用者の決定で時刻だけに変えた）。
              置き場は実レイアウトの幅で決める（上の `axisLabelsRef` の effect）。
            */}
            {chaseTimeline && (
              <div
                ref={axisLabelsRef}
                data-testid="chase-timeline-labels"
                className="relative mb-1 hidden h-4 text-xs whitespace-nowrap text-white/75 md:block"
              >
                <span data-axis-start className="absolute left-0 font-mono">{formatAxisTime(rangeMin)}</span>
                {chaseExtended ? (
                  <>
                    <span
                      data-testid="chase-timeline-planned-label"
                      data-axis-mark={axisFraction(chaseTimeline.plannedEndSeconds)}
                      className="absolute"
                    >
                      予定 <span className="font-mono">{formatAxisTime(chaseTimeline.plannedEndSeconds)}</span>{' '}
                      <span data-axis-pointer>¦</span>
                    </span>
                    <span data-testid="chase-timeline-end" data-axis-end className="absolute right-0 text-red-400">
                      延長中 · 先端 <span className="font-mono">{formatAxisTime(chaseTimeline.recordedEndSeconds)}</span>
                    </span>
                  </>
                ) : (
                  <>
                    <span
                      data-testid="chase-live-edge-label"
                      data-axis-mark={axisFraction(chaseTimeline.liveEdgeSeconds)}
                      className="absolute font-mono text-red-400"
                    >
                      {formatAxisTime(chaseTimeline.recordedEndSeconds)}
                    </span>
                    <span data-testid="chase-timeline-end" data-axis-end className="absolute right-0">
                      <span className="font-mono">{formatAxisTime(chaseTimeline.plannedEndSeconds)}</span> まで（予定）
                    </span>
                  </>
                )}
              </div>
            )}

            <div data-testid="player-controls-row" className="flex min-h-9 items-center gap-0.5 md:min-h-10 md:gap-1">
              {/*
                DOM 順はデスクトップの見た目（再生 → 前 → 次）にそろえ、Tab 順を見た目と一致させる。
                スマホは枠の中央に前 → 再生 → 次で大きく出すため、order で並べ替える。
              */}
              <div
                data-testid={hasChapters ? 'chapter-navigation' : undefined}
                className="pointer-events-none absolute inset-x-0 top-1/2 flex -translate-y-1/2 items-center justify-center gap-10 md:pointer-events-auto md:static md:translate-y-0 md:gap-0">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className={cn(ghost, 'pointer-events-auto order-1 size-14 rounded-full bg-black/45 md:order-none md:size-8 md:rounded-lg md:bg-transparent')}
                  aria-label={isPlaying ? '一時停止' : '再生'}
                  onClick={onTogglePlay}
                >
                  {isPlaying ? <Pause className="size-7 md:size-4" /> : <Play className="size-7 md:size-4" />}
                </Button>
                {hasChapters && (
                  <>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      className={cn(ghost, 'pointer-events-auto size-11 rounded-full bg-black/35 md:size-8 md:rounded-lg md:bg-transparent')}
                      aria-label="前のチャプター"
                      onClick={onPreviousChapter}
                    >
                      <SkipBack className="size-5 md:size-4" />
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      className={cn(ghost, 'pointer-events-auto order-2 size-11 rounded-full bg-black/35 md:order-none md:size-8 md:rounded-lg md:bg-transparent')}
                      aria-label="次のチャプター"
                      onClick={onNextChapter}
                    >
                      <SkipForward className="size-5 md:size-4" />
                    </Button>
                  </>
                )}
              </div>
              {/* 端末の音量ボタンで足りるので、スマホにはミュート / 音量を置かない。 */}
              <div className="group/volume hidden items-center md:flex">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className={ghost}
                  aria-label={muted || volume === 0 ? 'ミュート解除' : 'ミュート'}
                  onClick={onToggleMute}
                >
                  {muted || volume === 0 ? <VolumeX /> : <Volume2 />}
                </Button>
                <input
                  aria-label="音量"
                  type="range"
                  min={0}
                  max={1}
                  step={0.05}
                  value={volumeValue}
                  onChange={(event) => onVolumeChange(Number(event.target.value))}
                  className="h-6 w-0 opacity-0 transition-all focus-visible:w-16 focus-visible:opacity-100 group-hover/volume:w-16 group-hover/volume:opacity-100 accent-white"
                />
              </div>
              <span data-testid="playback-time" className="shrink-0 px-1 font-mono text-xs whitespace-nowrap md:px-2 md:text-sm">
                {chaseTimeline ? (
                  <>
                    {formatAxisTime(seconds)} / <span className="hidden md:inline">録画済み </span>
                    {formatAxisTime(chaseTimeline.recordedEndSeconds)}
                  </>
                ) : (
                  `${formatPlaybackTime(seconds)} / ${formatPlaybackTime(durationSeconds)}`
                )}
              </span>
              {/*
                チャプターがある録画だけ名前を出し、押すとプレイヤー内のチャプター一覧（見るだけ）を開く。
                チャプターが無い録画（カット版を含む）は名前も「›」も出さない。
              */}
              {currentChapterName !== undefined && (
                <button
                  ref={chapterButtonRef}
                  type="button"
                  data-testid="playback-chapter"
                  className="flex min-h-8 min-w-0 items-center gap-0.5 rounded px-1 text-xs text-white/85 outline-none hover:text-white focus-visible:ring-2 focus-visible:ring-white md:text-sm"
                  aria-label={`チャプター: ${currentChapterName}`}
                  aria-haspopup="menu"
                  aria-expanded={chaptersOpen}
                  aria-controls={`chapter-list-${recordingId}`}
                  onClick={() => {
                    onControlsActivity()
                    setMenuView(null)
                    setChaptersOpen((open) => !open)
                  }}
                >
                  <span className="truncate">· {currentChapterName}</span>
                  <ChevronRight className="hidden size-3.5 shrink-0 md:block" />
                </button>
              )}
              <div className="flex-1" />
              {nextEpisode && (
                <Link
                  to="/recordings/$id"
                  params={{ id: String(nextEpisode.id) }}
                  data-testid="next-episode-link"
                  aria-label={`次のエピソード: ${nextEpisode.title}`}
                  className="inline-flex min-h-9 shrink-0 items-center gap-1.5 rounded-full px-2 text-xs text-white outline-none hover:bg-white/15 focus-visible:ring-2 focus-visible:ring-white md:bg-white/10 md:px-3"
                  onClick={onNextEpisodeNavigate}
                >
                  <FastForward className="size-4" aria-hidden />
                  <span className="hidden md:inline">次: {formatDate(nextEpisode.startAt)}</span>
                </Link>
              )}
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className={cn(
                  ghost,
                  'absolute top-1.5 right-11 md:relative md:top-auto md:right-auto',
                  subtitlesEnabled &&
                    'after:absolute after:inset-x-2 after:bottom-1 after:h-0.5 after:rounded-full after:bg-orange-400',
                )}
                aria-label="字幕"
                aria-pressed={subtitlesEnabled}
                onClick={onToggleSubtitles}
              >
                <Captions />
              </Button>
              {showWatched && (
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className={cn(ghost, watched && 'bg-white/20')}
                  // トグルは固定の名前 + aria-pressed で状態を伝える（名前も入れ替えると
                  // 「押されている・未視聴に戻す」のように状態が二重に読まれる）。
                  aria-label="視聴済み"
                  aria-pressed={watched}
                  title={watched ? '未視聴に戻す' : '視聴済みにする'}
                  disabled={watchedPending || watchedAction === undefined}
                  onClick={watchedAction}
                >
                  <Check />
                </Button>
              )}
              <Button
                ref={gearRef}
                type="button"
                variant="ghost"
                size="icon"
                className={cn(ghost, 'absolute top-1.5 right-1.5 aria-expanded:bg-white/20 aria-expanded:text-white md:static')}
                aria-label="再生設定"
                aria-haspopup="menu"
                aria-expanded={menuOpen}
                aria-controls={`playback-settings-${recordingId}`}
                onClick={() => {
                  onControlsActivity()
                  setChaptersOpen(false)
                  setMenuView((view) => (view === null ? 'main' : null))
                }}
              >
                <Settings />
              </Button>
              {pictureInPictureEnabled && (
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className={cn(ghost, 'hidden md:inline-flex')}
                  aria-label={pictureInPicture ? 'ピクチャーインピクチャーを終了' : 'ピクチャーインピクチャー'}
                  onClick={onTogglePictureInPicture}
                >
                  <PictureInPicture2 />
                </Button>
              )}
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className={ghost}
                aria-label={isFullscreen ? '全画面を終了' : '全画面表示'}
                onClick={onToggleFullscreen}
              >
                {isFullscreen ? <Minimize /> : <Maximize />}
              </Button>
            </div>
          </div>
        </div>
        {/*
          メニューは常に枠（全画面要素）の内側に描く。外に描くと全画面中に設定を変えられない。
          スマホのシートは fixed なので枠の overflow には切られない。
        */}
        {menuView !== null && (
          <PlaybackSettingsMenu
            id={`playback-settings-${recordingId}`}
            view={menuView}
            onViewChange={setMenuView}
            onClose={closeMenu}
            profile={profile}
            encodedAssets={encodedAssets}
            playbackMode={playbackMode}
            profileOptions={profileOptions}
            audioChoice={audioChoice}
            onSelectAudio={onSelectAudio}
            playbackRate={playbackRate}
            subtitlesEnabled={subtitlesEnabled}
            skipEnabled={skipEnabled}
            showSkip={hasChapters}
            canEditChapters={canEditChapters}
            onEnterChapterEditing={onEnterChapterEditing}
            pictureInPictureEnabled={pictureInPictureEnabled}
            pictureInPicture={pictureInPicture}
            onSelectProfile={onSelectProfile}
            onRateChange={onRateChange}
            onToggleSubtitles={onToggleSubtitles}
            onToggleSkip={onToggleSkip}
            onTogglePictureInPicture={onTogglePictureInPicture}
            onFocusCapture={onToolbarFocus}
            onBlurCapture={onToolbarBlur}
          />
        )}
        {chaptersOpen && hasChapters && (
          <ChapterListMenu
            id={`chapter-list-${recordingId}`}
            entries={entries}
            currentIndex={currentEntryIndex}
            onJump={(start) => {
              onSeek(start)
              closeChapters(true)
            }}
            onClose={closeChapters}
            onFocusCapture={onToolbarFocus}
            onBlurCapture={onToolbarBlur}
          />
        )}
        </>
        )}
      </div>
    </div>
  )
}

type ChapterEntry = { start: number; end: number; label: string }

/** chapterEntries は区間と隙間（本編）を時刻順に並べる。終端は duration が分かるときだけ閉じる。 */
function chapterEntries(spans: ChapterSpan[], durationSeconds: number): ChapterEntry[] {
  const out: ChapterEntry[] = []
  let cursor = 0
  for (const span of [...spans].sort((a, b) => a.startMs - b.startMs)) {
    const start = span.startMs / 1000
    const end = span.endMs / 1000
    if (start > cursor) out.push({ start: cursor, end: start, label: '本編' })
    out.push({ start, end, label: chapterLabel(span) })
    cursor = Math.max(cursor, end)
  }
  if (durationSeconds > cursor) out.push({ start: cursor, end: durationSeconds, label: '本編' })
  return out
}

/** popoverClass は設定メニューとチャプター一覧が共有する置き場（md 以上は小窓、md 未満は画面下のシート）。 */
function popoverClass(align: 'left' | 'right') {
  return cn(
    'fixed inset-x-0 bottom-0 z-50 max-h-[50dvh] overflow-y-auto rounded-t-2xl bg-card pt-2 pb-[max(1.5rem,env(safe-area-inset-bottom))] text-[15px] text-foreground shadow-lg',
    'md:absolute md:bottom-18 md:z-30 md:max-h-[calc(100%-5.5rem)] md:w-75 md:rounded-xl md:bg-black/85 md:py-2 md:text-sm md:text-white md:backdrop-blur-sm',
    align === 'right' ? 'md:right-3.5 md:left-auto' : 'md:right-auto md:left-3.5',
  )
}

/** moveMenuFocus は ↑ / ↓ / Home / End で見た目の順に項目を移る。扱ったキーなら true。 */
function moveMenuFocus(key: string, menu: HTMLElement): boolean {
  const items = orderedMenuItems(menu)
  const index = items.indexOf(document.activeElement as HTMLElement)
  const focusAt = (i: number) => items[(i + items.length) % items.length]?.focus()
  switch (key) {
    case 'ArrowDown':
      focusAt(index + 1)
      return true
    case 'ArrowUp':
      focusAt(index < 0 ? -1 : index - 1)
      return true
    case 'Home':
      focusAt(0)
      return true
    case 'End':
      focusAt(-1)
      return true
    default:
      return false
  }
}

type ChapterListMenuProps = {
  id: string
  entries: ChapterEntry[]
  currentIndex: number
  onJump: (start: number) => void
  onClose: (focusButton: boolean) => void
  onFocusCapture: (event: ReactFocusEvent<HTMLElement>) => void
  onBlurCapture: (event: ReactFocusEvent<HTMLElement>) => void
}

/**
 * ChapterListMenu は時刻の横のチャプター名から開く、見るためのチャプター一覧。設定メニューと同じ
 * 置き場に「時刻・チャプター名」を時刻順に並べ、いまのチャプターに ✓ を付ける。行を押すとその位置へ
 * 飛ぶ。編集の操作は置かない（チャプターを直す入口は別に持つ）。
 */
function ChapterListMenu({ id, entries, currentIndex, onJump, onClose, onFocusCapture, onBlurCapture }: ChapterListMenuProps) {
  const menuRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const menu = menuRef.current
    if (!menu) return
    ;(menu.querySelector<HTMLElement>('[aria-checked="true"]') ?? orderedMenuItems(menu)[0])?.focus()
  }, [])
  return (
    <>
      <div data-testid="playback-settings-scrim" aria-hidden className="fixed inset-0 z-40 bg-black/45 md:hidden" />
      <div
        ref={menuRef}
        id={id}
        role="menu"
        aria-label="チャプター"
        data-testid="chapter-list"
        data-player-popover
        className={popoverClass('left')}
        onKeyDown={(event) => {
          const menu = menuRef.current
          if (!menu) return
          if (event.key === 'Escape') onClose(true)
          else if (!moveMenuFocus(event.key, menu)) return
          event.preventDefault()
          event.stopPropagation()
        }}
        onFocusCapture={onFocusCapture}
        onBlurCapture={onBlurCapture}
      >
        <div aria-hidden className="mx-auto mt-1 mb-2 h-1 w-9 rounded-full bg-border md:hidden" />
        {entries.map((entry, index) => (
          <button
            key={`${entry.start}-${entry.end}`}
            type="button"
            role="menuitemradio"
            aria-checked={index === currentIndex}
            className="flex min-h-12 w-full items-center gap-3 px-5 text-left outline-none active:bg-muted focus-visible:bg-muted md:min-h-10 md:px-4 md:hover:bg-white/10 md:focus-visible:bg-white/15 md:active:bg-white/15"
            onClick={() => onJump(entry.start)}
          >
            <span className="flex w-5 shrink-0 justify-center">
              {index === currentIndex && <Check className="size-5" aria-hidden />}
            </span>
            <span className="w-14 shrink-0 font-mono text-xs text-muted-foreground md:text-white/70">
              {formatPlaybackTime(entry.start)}
            </span>
            <span className="flex-1 truncate">{entry.label}</span>
          </button>
        ))}
      </div>
    </>
  )
}

type PlaybackSettingsMenuProps = {
  id: string
  view: MenuView
  onViewChange: (view: MenuView) => void
  onClose: (focusGear: boolean) => void
  profile: string
  encodedAssets: EncodedAsset[]
  playbackMode: 'encoded' | 'original-vod' | 'chase'
  profileOptions?: readonly { name: string; label?: string }[]
  audioChoice?: LiveAudioChoice
  onSelectAudio?: (choice: LiveAudioChoice | undefined) => void
  playbackRate: number
  subtitlesEnabled: boolean
  skipEnabled: boolean
  showSkip: boolean
  canEditChapters: boolean
  onEnterChapterEditing?: () => void
  pictureInPictureEnabled: boolean
  pictureInPicture: boolean
  onSelectProfile: (profile: string) => void
  onRateChange: (rate: number) => void
  onToggleSubtitles: () => void
  onToggleSkip: (enabled: boolean) => void
  onTogglePictureInPicture: () => void
  onFocusCapture: (event: ReactFocusEvent<HTMLElement>) => void
  onBlurCapture: (event: ReactFocusEvent<HTMLElement>) => void
}

/**
 * PlaybackSettingsMenu は「アイコン・項目名・現在値 ›」の行リスト。2 択はスイッチ、3 択以上は
 * 「›」で同じ枠の中身を「‹ 見出し」+ 選択肢へ差し替える。md 以上は歯車の真上の黒い小窓、
 * md 未満は画面の下からのシート（背後に幕）になる。
 */
function PlaybackSettingsMenu({
  id,
  view,
  onViewChange,
  onClose,
  profile,
  encodedAssets,
  playbackMode,
  profileOptions,
  audioChoice,
  onSelectAudio,
  playbackRate,
  subtitlesEnabled,
  skipEnabled,
  showSkip,
  canEditChapters,
  onEnterChapterEditing,
  pictureInPictureEnabled,
  pictureInPicture,
  onSelectProfile,
  onRateChange,
  onToggleSubtitles,
  onToggleSkip,
  onTogglePictureInPicture,
  onFocusCapture,
  onBlurCapture,
}: PlaybackSettingsMenuProps) {
  const menuRef = useRef<HTMLDivElement>(null)
  // 下の階層から戻ったとき、入ったときの行にフォーカスを戻す。
  const [returnRow, setReturnRow] = useState<MenuView | null>(null)

  // 階層が替わるたびに、印の付いた項目（戻り先の行・選択中の選択肢）か見た目で先頭の項目へ。
  useEffect(() => {
    const menu = menuRef.current
    if (!menu) return
    const marked = menu.querySelector<HTMLElement>('[data-menu-focus="true"]')
    ;(marked ?? orderedMenuItems(menu)[0])?.focus()
  }, [view])

  const enter = (next: MenuView) => onViewChange(next)
  const back = () => {
    setReturnRow(view)
    onViewChange('main')
  }
  const choose = (apply: () => void) => {
    apply()
    onClose(true)
  }

  const onKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const menu = menuRef.current
    if (!menu) return
    if (moveMenuFocus(event.key, menu)) {
      event.preventDefault()
      event.stopPropagation()
      return
    }
    switch (event.key) {
      case 'Escape':
        if (view === 'main') onClose(true)
        else back()
        break
      case 'ArrowLeft':
        if (view === 'main') return
        back()
        break
      case 'ArrowRight': {
        const submenu = (document.activeElement as HTMLElement | null)?.dataset.submenu as MenuView | undefined
        if (view !== 'main' || submenu === undefined) return
        enter(submenu)
        break
      }
      default:
        return
    }
    event.preventDefault()
    event.stopPropagation()
  }

  const selectedAsset = encodedAssets.find((asset) => asset.profile === profile)
  const selectedLiveProfile = profileOptions?.find((option) => option.name === profile)
  const usesLiveProfiles = playbackMode === 'original-vod' || playbackMode === 'chase'
  const selectedProfileLabel = usesLiveProfiles
    ? selectedLiveProfile?.label ?? profile
    : selectedAsset ? assetLabel(selectedAsset) : profile
  const showQuality = usesLiveProfiles
    ? (profileOptions?.length ?? 0) > 1
    : encodedAssets.length > 0
  const selectedAudioLabel = audioChoice === 'main' ? '主音声' : audioChoice === 'sub' ? '副音声' : '標準'
  const rates = PLAYBACK_RATES.includes(playbackRate) ? PLAYBACK_RATES : [...PLAYBACK_RATES, playbackRate].sort((a, b) => a - b)
  const row =
    'flex min-h-13 w-full items-center gap-4 px-5 text-left outline-none active:bg-muted focus-visible:bg-muted md:min-h-11 md:gap-3.5 md:px-4 md:hover:bg-white/10 md:focus-visible:bg-white/15 md:active:bg-white/15'
  const value = 'flex shrink-0 items-center gap-1 text-sm text-muted-foreground md:text-white/70'
  const header =
    'flex h-12 w-full items-center gap-2.5 border-b border-border px-3 font-semibold outline-none focus-visible:bg-muted md:mb-1 md:h-11 md:border-white/15 md:focus-visible:bg-white/15'
  const option = cn(row, 'min-h-12 md:min-h-10 md:gap-3')

  const submenuRow = (key: 'speed' | 'quality' | 'audio', icon: ReactNode, label: string, current: string) => (
    <button
      type="button"
      role="menuitem"
      aria-label={label}
      aria-describedby={`${id}-${key}-value`}
      data-submenu={key}
      data-menu-focus={returnRow === key ? 'true' : undefined}
      className={row}
      onClick={() => enter(key)}
    >
      {icon}
      <span className="flex-1">{label}</span>
      <span id={`${id}-${key}-value`} className={value}>
        {current}
        <ChevronRight className="size-4" aria-hidden />
      </span>
    </button>
  )
  const switchRow = (icon: ReactNode, label: string, checked: boolean, toggle: () => void) => (
    <button
      type="button"
      role="menuitemcheckbox"
      aria-label={label}
      aria-checked={checked}
      className={row}
      onClick={toggle}
    >
      {icon}
      <span className="flex-1">{label}</span>
      <span
        aria-hidden
        className={cn(
          'relative h-5 w-9 shrink-0 rounded-full transition-colors',
          checked ? 'bg-orange-400' : 'bg-input md:bg-white/30',
        )}
      >
        <span
          className={cn(
            'absolute top-0.5 size-4 rounded-full bg-white shadow-sm transition-[left]',
            checked ? 'left-4.5' : 'left-0.5',
          )}
        />
      </span>
    </button>
  )
  const backRow = (label: string) => (
    <button type="button" role="menuitem" aria-label={`戻る（${label}）`} className={header} onClick={back}>
      <ChevronLeft className="size-5" aria-hidden />
      {label}
    </button>
  )
  const radioRow = (key: string, label: string, selected: boolean, apply: () => void, size?: string) => (
    <button
      key={key}
      type="button"
      role="menuitemradio"
      aria-checked={selected}
      data-menu-focus={selected ? 'true' : undefined}
      className={option}
      onClick={() => choose(apply)}
    >
      <span className="flex w-5 shrink-0 justify-center">{selected && <Check className="size-5" aria-hidden />}</span>
      <span className="flex-1">{label}</span>
      {size !== undefined && <span className="shrink-0 font-mono text-xs text-muted-foreground md:text-white/60">{size}</span>}
    </button>
  )
  const icon = 'size-5 shrink-0'
  const qualityRow = showQuality && submenuRow(
    'quality',
    <SlidersHorizontal className={icon} aria-hidden />,
    '画質',
    selectedProfileLabel,
  )
  const audioRow = playbackMode === 'original-vod' && submenuRow(
    'audio',
    <Volume2 className={icon} aria-hidden />,
    '音声',
    selectedAudioLabel,
  )
  const speedRow = submenuRow('speed', <Gauge className={icon} aria-hidden />, '再生速度', rateLabel(playbackRate))
  const subtitlesRow = switchRow(<Captions className={icon} aria-hidden />, '字幕', subtitlesEnabled, onToggleSubtitles)
  const skipRow = showSkip && switchRow(
    <Activity className={icon} aria-hidden />,
    'CM を飛ばす',
    skipEnabled,
    () => onToggleSkip(!skipEnabled),
  )
  const chapterEditRow = canEditChapters && (
    <button
      type="button"
      role="menuitem"
      aria-label="チャプターを直す"
      className={row}
      onClick={() => {
        onClose(false)
        onEnterChapterEditing?.()
      }}
    >
      <Scissors className={icon} aria-hidden />
      <span className="flex-1">チャプターを直す</span>
    </button>
  )

  return (
    <>
      <div
        data-testid="playback-settings-scrim"
        aria-hidden
        className="fixed inset-0 z-40 bg-black/45 md:hidden"
      />
      <div
        ref={menuRef}
        id={id}
        role="menu"
        aria-label={view === 'main' ? '再生設定' : view === 'speed' ? '再生速度' : view === 'quality' ? '画質' : '音声'}
        data-testid="playback-settings"
        data-player-popover
        className={popoverClass('right')}
        onKeyDown={onKeyDown}
        onFocusCapture={onFocusCapture}
        onBlurCapture={onBlurCapture}
      >
        <div aria-hidden className="mx-auto mt-1 mb-2 h-1 w-9 rounded-full bg-border md:hidden" />
        {view === 'main' && (
          <>
            {/* デスクトップは CM・字幕・再生速度・（音声）・画質の順で、画質を歯車に近い最下段に置く。
                スマホのシートは画質を一番上に置く（ラフ）。並びだけを CSS で逆にするので、音声は
                どちらでも画質の隣（デスクトップは直前、スマホは直後）に入る。 */}
            <div role="none" className="flex flex-col-reverse md:flex-col">
              {skipRow}
              {subtitlesRow}
              {speedRow}
              {audioRow}
              {qualityRow}
            </div>
            {pictureInPictureEnabled && (
              <>
                <div role="separator" className="my-1 border-t border-border md:hidden" />
                <button
                  type="button"
                  role="menuitem"
                  className={cn(row, 'md:hidden')}
                  onClick={() => choose(onTogglePictureInPicture)}
                >
                  <PictureInPicture2 className={icon} aria-hidden />
                  <span className="flex-1">
                    {pictureInPicture ? 'ピクチャー・イン・ピクチャーを終了' : 'ピクチャー・イン・ピクチャー'}
                  </span>
                </button>
              </>
            )}
            {chapterEditRow && (
              <>
                <div role="separator" className="my-1 border-t border-border" />
                {chapterEditRow}
              </>
            )}
          </>
        )}
        {view === 'speed' && (
          <>
            {backRow('再生速度')}
            {rates.map((rate) =>
              radioRow(String(rate), rateLabel(rate), rate === playbackRate, () => onRateChange(rate)),
            )}
          </>
        )}
        {view === 'quality' && (
          <>
            {backRow('画質')}
            {usesLiveProfiles
              ? profileOptions?.map((option) =>
                  radioRow(option.name, option.label ?? option.name, option.name === profile, () => onSelectProfile(option.name)),
                )
              : encodedAssets.map((asset) =>
                  radioRow(
                    asset.profile,
                    assetLabel(asset),
                    asset.profile === profile,
                    () => onSelectProfile(asset.profile),
                    // サイズが取れない資産も選択肢は隠さず、サイズだけ省く（値札の規律）。
                    asset.sizeBytes === undefined ? undefined : formatBytes(asset.sizeBytes),
                  ),
                )}
          </>
        )}
        {view === 'audio' && (
          <>
            {backRow('音声')}
            {radioRow('standard', '標準', audioChoice === undefined, () => onSelectAudio?.(undefined))}
            {radioRow('main', '主音声', audioChoice === 'main', () => onSelectAudio?.('main'))}
            {radioRow('sub', '副音声', audioChoice === 'sub', () => onSelectAudio?.('sub'))}
          </>
        )}
      </div>
    </>
  )
}

/** orderedMenuItems は表示中のメニュー項目を見た目の上から順に返す（スマホは並びを CSS で逆にしている）。 */
function orderedMenuItems(menu: HTMLElement): HTMLElement[] {
  const all = Array.from(menu.querySelectorAll<HTMLElement>('[role^="menuitem"]'))
  const shown = all.filter((el) => el.getClientRects().length > 0)
  // レイアウトの無い環境（jsdom）では全項目が 0 矩形になるので、DOM 順のまま使う。
  if (shown.length === 0) return all
  return shown.sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
}

function rateLabel(rate: number): string {
  return rate === 1 ? '標準' : `${rate}x`
}

function assetLabel(asset: EncodedAsset): string {
  return asset.cut === true ? `カット版（${asset.profile}）` : asset.profile
}

function chapterLabel(span: ChapterSpan): string {
  return span.label ?? (span.cut ? 'CM' : 'チャプター')
}
