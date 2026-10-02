import {
  useState,
  type FocusEvent as ReactFocusEvent,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
  type RefObject,
} from 'react'

import type { ChapterSpan, EncodedAsset } from '@/api/generated'
import {
  Check,
  Maximize,
  Minimize,
  Pause,
  PictureInPicture2,
  Play,
  Settings,
  SkipBack,
  SkipForward,
  Volume2,
  VolumeX,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { formatChaptersTime } from '@/lib/chapters'
import { formatBytes } from '@/lib/format'
import { recordingFileURL } from '@/lib/playback-position'
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

type RecordingPlaybackControlsProps = {
  recordingId: number
  profile: string
  encodedAssets: EncodedAsset[]
  fullscreenRef: RefObject<HTMLDivElement | null>
  video: ReactNode
  currentSeconds: number
  durationSeconds: number
  playedFraction: number
  chapters: ChapterSpan[]
  playingCut: boolean
  tilePreview: TilePreview
  tilesRequested: boolean
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  onSeekPointerDown: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerMove: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerUp: (event: ReactPointerEvent<HTMLDivElement>) => void
  onSeekPointerLeave: () => void
  onSeek: (seconds: number) => void
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
  onToolbarFocus: (event: ReactFocusEvent<HTMLElement>) => void
  onToolbarBlur: (event: ReactFocusEvent<HTMLElement>) => void
  /** shell 内のキー入力（Tab を含む）。隠れたバーを出してから Tab を処理させる。 */
  onShellKeyDown: () => void
}

/** RecordingPlaybackControls は encoded VOD の再生操作と単一タイムラインを描画する。 */
export function RecordingPlaybackControls({
  recordingId,
  profile,
  encodedAssets,
  fullscreenRef,
  video,
  currentSeconds,
  durationSeconds,
  playedFraction,
  chapters,
  playingCut,
  tilePreview,
  tilesRequested,
  tilesAvailable,
  onTileImageLoad,
  onTileImageError,
  onSeekPointerDown,
  onSeekPointerMove,
  onSeekPointerUp,
  onSeekPointerLeave,
  onSeek,
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
  onToolbarFocus,
  onToolbarBlur,
  onShellKeyDown,
}: RecordingPlaybackControlsProps) {
  const [settingsOpen, setSettingsOpen] = useState(false)
  const seconds = Math.max(0, Math.min(durationSeconds || 0, currentSeconds))
  const volumeValue = muted ? 0 : volume
  const hasChapters = !playingCut && chapters.length > 0
  const pictureInPictureEnabled =
    typeof document !== 'undefined' && document.pictureInPictureEnabled === true
  const downloadFilename = `recording-${recordingId}-${profile}.mp4`

  const seekByKeyboard = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    let target: number | undefined
    switch (event.key) {
      case 'ArrowLeft':
        target = seconds - 10
        break
      case 'ArrowRight':
        target = seconds + 10
        break
      case 'Home':
        target = 0
        break
      case 'End':
        target = durationSeconds
        break
      default:
        return
    }
    event.preventDefault()
    event.stopPropagation()
    onSeek(Math.max(0, Math.min(durationSeconds, target)))
  }

  const hoverSpan =
    tilePreview && !playingCut
      ? chapters.find((span) => tilePreview.seconds * 1000 >= span.startMs && tilePreview.seconds * 1000 < span.endMs)
      : undefined
  const selectedAsset = encodedAssets.find((asset) => asset.profile === profile)
  const downloadSize = selectedAsset?.sizeBytes === undefined ? undefined : formatBytes(selectedAsset.sizeBytes)
  const ghost = 'text-white hover:bg-white/15 hover:text-white'
  const settingsPanel = settingsOpen ? (
    <section
      id={`playback-settings-${recordingId}`}
      role="region"
      aria-label="再生設定"
      data-testid="playback-settings"
      className={cn(
        'z-30 grid gap-3 border border-border bg-background p-3 text-sm text-foreground shadow-lg',
        isFullscreen
          ? 'absolute inset-x-2 bottom-24 max-h-[60%] overflow-auto rounded-lg md:right-2 md:left-auto md:w-80'
          : 'md:absolute md:right-2 md:bottom-[5.5rem] md:w-80 md:rounded-lg',
      )}
      onFocusCapture={onToolbarFocus}
      onBlurCapture={onToolbarBlur}
    >
      <label className="grid grid-cols-[6rem_1fr] items-center gap-2">
        <span>画質</span>
        <select
          aria-label="画質"
          value={profile}
          onChange={(event) => onSelectProfile(event.target.value)}
          className="h-9 min-w-0 rounded border border-border bg-background px-2"
        >
          {encodedAssets.map((asset) => (
            <option key={asset.profile} value={asset.profile}>
              {assetOptionLabel(asset)}
            </option>
          ))}
        </select>
      </label>
      <div className="flex items-center gap-3 sm:hidden">
        <Button type="button" variant="outline" className="min-h-9" onClick={onToggleMute}>
          {muted || volume === 0 ? 'ミュート解除' : 'ミュート'}
        </Button>
        <label className="flex min-w-0 flex-1 items-center gap-2">
          <span>音量</span>
          <input
            aria-label="音量"
            type="range"
            min={0}
            max={1}
            step={0.05}
            value={volumeValue}
            onChange={(event) => onVolumeChange(Number(event.target.value))}
            className="h-6 min-w-0 flex-1 accent-foreground"
          />
        </label>
      </div>
      <label className="grid grid-cols-[6rem_1fr] items-center gap-2">
        <span>再生速度</span>
        <select
          aria-label="再生速度"
          value={String(playbackRate)}
          onChange={(event) => onRateChange(Number(event.target.value))}
          className="h-9 rounded border border-border bg-background px-2"
        >
          {![0.5, 0.75, 1, 1.25, 1.5, 1.75, 2].includes(playbackRate) && (
            <option value={playbackRate}>{playbackRate}x</option>
          )}
          {[0.5, 0.75, 1, 1.25, 1.5, 1.75, 2].map((rate) => (
            <option key={rate} value={rate}>{rate}x</option>
          ))}
        </select>
      </label>
      <Button
        type="button"
        variant="outline"
        className="justify-start"
        aria-pressed={subtitlesEnabled}
        onClick={onToggleSubtitles}
      >
        字幕 {subtitlesEnabled ? 'オン' : 'オフ'}
      </Button>
      {hasChapters && (
        <>
          <label className="flex min-h-9 items-center gap-2">
            <input
              type="checkbox"
              aria-label="CM を飛ばす"
              checked={skipEnabled}
              onChange={(event) => onToggleSkip(event.target.checked)}
              className="size-5 accent-foreground"
            />
            CM を飛ばす
          </label>
          <div className="flex gap-2 md:hidden">
            <Button type="button" variant="outline" className="min-h-11 flex-1" onClick={onPreviousChapter}>
              前のチャプター
            </Button>
            <Button type="button" variant="outline" className="min-h-11 flex-1" onClick={onNextChapter}>
              次のチャプター
            </Button>
          </div>
        </>
      )}
      {pictureInPictureEnabled && (
        <Button
          type="button"
          variant="outline"
          className="justify-start md:hidden"
          onClick={onTogglePictureInPicture}
        >
          <PictureInPicture2 />
          {pictureInPicture ? 'ピクチャーインピクチャーを終了' : 'ピクチャーインピクチャー'}
        </Button>
      )}
      <a
        href={recordingFileURL(recordingId, profile)}
        download={downloadFilename}
        aria-label="encoded 動画をダウンロード"
        className="flex min-h-9 items-center justify-between gap-3 text-primary underline-offset-2 hover:underline"
      >
        <span>この版をダウンロード</span>
        {downloadSize !== undefined && <span className="text-muted-foreground">{downloadSize}</span>}
      </a>
    </section>
  ) : null
  const watchedAction = watched ? onDeleteWatched : onPutWatched
  const showControls = controlsVisible || settingsOpen

  return (
    <div
      className="relative w-full max-w-3xl"
      data-testid="recording-player-shell"
      onPointerMove={onControlsActivity}
      onKeyDown={onShellKeyDown}
    >
      <div
        ref={fullscreenRef}
        data-testid="recording-player-frame"
        className="relative aspect-video w-full overflow-hidden rounded bg-black"
        onPointerMove={onControlsActivity}
      >
        {video}
        <div
          data-testid="player-controls"
          className={cn(
            'absolute inset-x-0 bottom-0 z-10 bg-gradient-to-t from-black/95 via-black/70 to-transparent px-2 pt-10 pb-1 text-white transition-opacity duration-150 sm:px-3',
            showControls ? 'opacity-100' : 'pointer-events-none opacity-0',
          )}
          aria-hidden={!showControls}
          inert={!showControls}
          onFocusCapture={onToolbarFocus}
          onBlurCapture={onToolbarBlur}
        >
          <div
            role="slider"
            aria-label="シークバー"
            aria-valuemin={0}
            aria-valuemax={Math.max(0, durationSeconds)}
            aria-valuenow={seconds}
            aria-valuetext={`${formatPlaybackTime(seconds)} / ${formatPlaybackTime(durationSeconds)}`}
            tabIndex={0}
            data-testid="seek-scrub"
            className="group relative mb-1 h-4 cursor-pointer touch-none outline-none before:absolute before:inset-x-0 before:top-1/2 before:h-1 before:-translate-y-1/2 before:rounded-full before:bg-white/40 after:absolute after:inset-x-0 after:top-1/2 after:h-1 after:-translate-y-1/2 after:rounded-full after:bg-transparent focus-visible:ring-2 focus-visible:ring-white"
            onKeyDown={seekByKeyboard}
            onPointerDown={onSeekPointerDown}
            onPointerMove={onSeekPointerMove}
            onPointerUp={onSeekPointerUp}
            onPointerCancel={onSeekPointerUp}
            onPointerLeave={onSeekPointerLeave}
          >
            <div className="pointer-events-none absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 overflow-hidden rounded-full bg-white/30">
              <div className="h-full bg-white" style={{ width: `${playedFraction * 100}%` }} />
            </div>
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
              style={{ left: `${playedFraction * 100}%` }}
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

          <div data-testid="player-controls-row" className="flex min-h-10 items-center gap-1 sm:gap-2">
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className={ghost}
              aria-label={isPlaying ? '一時停止' : '再生'}
              onClick={onTogglePlay}
            >
              {isPlaying ? <Pause /> : <Play />}
            </Button>
            {hasChapters && (
              <div data-testid="chapter-navigation" className="hidden items-center md:flex">
                <Button type="button" variant="ghost" size="icon" className={ghost} aria-label="前のチャプター" onClick={onPreviousChapter}>
                  <SkipBack />
                </Button>
                <Button type="button" variant="ghost" size="icon" className={ghost} aria-label="次のチャプター" onClick={onNextChapter}>
                  <SkipForward />
                </Button>
              </div>
            )}
            <div className="group/volume hidden items-center sm:flex">
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
            <span data-testid="playback-time" className="min-w-0 whitespace-nowrap text-xs">
              {formatPlaybackTime(seconds)} / {formatPlaybackTime(durationSeconds)}
            </span>
            <div className="flex-1" />
            {showWatched && (
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className={cn(ghost, watched && 'bg-white/20')}
                aria-label={watched ? '未視聴に戻す' : '視聴済みにする'}
                aria-pressed={watched}
                title={watched ? '未視聴に戻す' : '視聴済みにする'}
                disabled={watchedPending || watchedAction === undefined}
                onClick={watchedAction}
              >
                <Check />
              </Button>
            )}
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className={cn(ghost, settingsOpen && 'bg-white/20')}
              aria-label="再生設定"
              aria-expanded={settingsOpen}
              aria-controls={`playback-settings-${recordingId}`}
              onClick={() => {
                onControlsActivity()
                setSettingsOpen((open) => !open)
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
        {isFullscreen && settingsPanel}
      </div>

      {!isFullscreen && settingsPanel}
    </div>
  )
}

function chapterLabel(span: ChapterSpan): string {
  return span.label ?? (span.cut ? 'CM' : 'チャプター')
}

function formatPlaybackTime(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0:00'
  const seconds = Math.floor(value)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remaining = seconds % 60
  return hours > 0
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remaining).padStart(2, '0')}`
    : `${minutes}:${String(remaining).padStart(2, '0')}`
}

function assetOptionLabel(asset: EncodedAsset): string {
  return asset.sizeBytes === undefined ? asset.profile : `${asset.profile} (${formatBytes(asset.sizeBytes)})`
}
