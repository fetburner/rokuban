import { useEffect, useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react'

import type { ChapterSpan } from '@/api/generated'
import { Button } from '@/components/ui/button'
import {
  filmstripTileIndices,
  filmstripTimeToX,
  filmstripXToTime,
  type FilmstripRange,
} from '@/lib/chapter-filmstrip'
import { chapterBoundaries, formatChaptersTime, nudgeBoundary } from '@/lib/chapters'
import {
  SEEK_TILES_INTERVAL_SECONDS,
  seekTileAt,
  seekTileBackgroundSize,
  seekTilesURL,
} from '@/lib/seek-tiles'

const DEFAULT_RANGE_SECONDS = 140

type RecordingChapterFilmstripProps = {
  recordingId: number
  durationSeconds: number
  currentSeconds: number
  spans: ChapterSpan[]
  selectedBoundary: number | null
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  onSeek: (seconds: number) => void
  onSelectBoundary: (seconds: number) => void
  onChangeSpans: (spans: ChapterSpan[]) => void
  onPlayAround: (seconds: number) => void
}

function rangeAround(center: number, length: number, duration: number): FilmstripRange {
  const safeDuration = Number.isFinite(duration) && duration > 0 ? duration : 0
  const safeLength = Math.min(safeDuration, Math.max(Math.min(safeDuration, SEEK_TILES_INTERVAL_SECONDS), length))
  const start = Math.max(0, Math.min(safeDuration - safeLength, center - safeLength / 2))
  return { startSeconds: start, endSeconds: start + safeLength }
}

function formatTime(seconds: number): string {
  return formatChaptersTime(seconds)
}

/** RecordingChapterFilmstrip は原本の時間軸に沿ってシークタイルと編集境界を描く。 */
export function RecordingChapterFilmstrip({
  recordingId,
  durationSeconds,
  currentSeconds,
  spans,
  selectedBoundary,
  tilesAvailable,
  onTileImageLoad,
  onTileImageError,
  onSeek,
  onSelectBoundary,
  onChangeSpans,
  onPlayAround,
}: RecordingChapterFilmstripProps) {
  const boundaries = useMemo(() => chapterBoundaries(spans), [spans])
  const [range, setRange] = useState(() => rangeAround(currentSeconds, DEFAULT_RANGE_SECONDS, durationSeconds))
  const [dragPreviewMs, setDragPreviewMs] = useState<number | null>(null)
  const trackRef = useRef<HTMLDivElement>(null)
  const dragRef = useRef<{ fromSeconds: number; downX: number; moved: boolean } | null>(null)
  const ignoreClickRef = useRef(false)

  useEffect(() => {
    setRange(rangeAround(currentSeconds, DEFAULT_RANGE_SECONDS, durationSeconds))
    // The edit screen mounts after the video metadata has loaded. Reset only when a new
    // recording duration arrives, not every time the playback clock advances.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [recordingId, durationSeconds])

  const tiles = useMemo(
    () => filmstripTileIndices(range, durationSeconds),
    [durationSeconds, range],
  )
  const rangeLength = range.endSeconds - range.startSeconds
  const selectedForDisplay = dragPreviewMs === null ? selectedBoundary : dragPreviewMs / 1000
  const overviewStart = durationSeconds > 0 ? (range.startSeconds / durationSeconds) * 100 : 0
  const overviewWidth = durationSeconds > 0 ? (rangeLength / durationSeconds) * 100 : 100

  const panToX = (clientX: number, element: HTMLDivElement, width: number) => {
    if (durationSeconds <= 0 || width <= 0) return
    const rect = element.getBoundingClientRect()
    const nextCenter = filmstripXToTime(clientX - rect.left, { startSeconds: 0, endSeconds: durationSeconds }, rect.width)
    setRange(rangeAround(nextCenter, rangeLength, durationSeconds))
  }

  const zoom = (factor: number) => {
    if (durationSeconds <= 0) return
    const center = selectedBoundary ?? currentSeconds
    setRange(rangeAround(center, rangeLength * factor, durationSeconds))
  }

  const moveBoundary = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = dragRef.current
    const track = trackRef.current
    if (!drag || !track) return
    const rect = track.getBoundingClientRect()
    const target = filmstripXToTime(event.clientX - rect.left, range, rect.width)
    if (drag) {
      drag.moved ||= Math.abs(event.clientX - drag.downX) > 2
      setDragPreviewMs(Math.round(target * 1000))
      onSelectBoundary(target)
    }
  }

  const startBoundaryDrag = (event: ReactPointerEvent<HTMLButtonElement>, boundary: number) => {
    dragRef.current = { fromSeconds: boundary, downX: event.clientX, moved: false }
    setDragPreviewMs(Math.round(boundary * 1000))
    onSelectBoundary(boundary)
    event.currentTarget.setPointerCapture?.(event.pointerId)
  }

  const finishBoundaryDrag = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = dragRef.current
    if (!drag) return
    const target = (dragPreviewMs ?? Math.round(drag.fromSeconds * 1000)) / 1000
    if (drag.moved) {
      const moved = nudgeBoundary(spans, drag.fromSeconds, target - drag.fromSeconds)
      onChangeSpans(moved)
      onSelectBoundary(target)
      ignoreClickRef.current = true
    }
    setDragPreviewMs(null)
    dragRef.current = null
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId)
    }
  }

  const cancelBoundaryDrag = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const drag = dragRef.current
    if (!drag) return
    setDragPreviewMs(null)
    dragRef.current = null
    onSelectBoundary(drag.fromSeconds)
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId)
    }
  }

  const nudgeSelected = (deltaSeconds: number) => {
    if (selectedBoundary === null) return
    const next = selectedBoundary + deltaSeconds
    onChangeSpans(nudgeBoundary(spans, selectedBoundary, deltaSeconds))
    onSelectBoundary(next)
  }

  const overviewPanStart = (event: ReactPointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture?.(event.pointerId)
    panToX(event.clientX, event.currentTarget, event.currentTarget.clientWidth)
  }

  return (
    <section data-testid="chapter-filmstrip" aria-label="チャプターフィルムストリップ" className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center gap-2">
        <div
          data-testid="chapter-filmstrip-overview"
          className="relative h-2 min-w-0 flex-1 touch-none rounded bg-muted"
          onPointerDown={overviewPanStart}
          onPointerMove={(event) => {
            if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
              panToX(event.clientX, event.currentTarget, event.currentTarget.clientWidth)
            }
          }}
          aria-label="表示している時間範囲"
        >
          {durationSeconds > 0 && spans.filter((span) => span.cut).map((span, index) => (
            <span
              key={`${span.startMs}-${span.endMs}-${index}`}
              className="absolute inset-y-0 bg-chapter-cut"
              style={{
                left: `${(span.startMs / 1000 / durationSeconds) * 100}%`,
                width: `${((span.endMs - span.startMs) / 1000 / durationSeconds) * 100}%`,
              }}
            />
          ))}
          <span
            data-testid="chapter-filmstrip-visible-window"
            className="absolute -inset-y-1 rounded-sm border-2 border-foreground bg-foreground/10"
            style={{ left: `${overviewStart}%`, width: `${overviewWidth}%` }}
          />
        </div>
        <Button type="button" size="sm" variant="outline" onClick={() => setRange({ startSeconds: 0, endSeconds: durationSeconds })} disabled={durationSeconds <= 0}>
          全体
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="フィルムストリップを縮小" onClick={() => zoom(2)} disabled={durationSeconds <= 0}>
          −
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="フィルムストリップを拡大" onClick={() => zoom(0.5)} disabled={durationSeconds <= 0}>
          ＋
        </Button>
      </div>

      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{formatTime(range.startSeconds)}–{formatTime(range.endSeconds)} を表示</span>
        <span>全体 {formatTime(0)}–{formatTime(durationSeconds)}</span>
      </div>

      <div
        ref={trackRef}
        data-testid="chapter-filmstrip-track"
        data-duration-seconds={durationSeconds}
        data-visible-start-seconds={range.startSeconds}
        data-visible-end-seconds={range.endSeconds}
        className="relative h-[4.5rem] min-w-0 touch-none overflow-hidden rounded bg-muted/70"
        onClick={(event) => {
          if (ignoreClickRef.current) {
            ignoreClickRef.current = false
            return
          }
          const rect = event.currentTarget.getBoundingClientRect()
          onSeek(filmstripXToTime(event.clientX - rect.left, range, rect.width))
        }}
      >
        {tiles.map((index) => {
          const seconds = index * SEEK_TILES_INTERVAL_SECONDS
          const rect = seekTileAt(seconds)
          const widthPercent = rangeLength > 0 ? (SEEK_TILES_INTERVAL_SECONDS / rangeLength) * 100 : 0
          const leftPercent = rangeLength > 0 ? ((seconds - range.startSeconds) / rangeLength) * 100 : 0
          return (
            <div
              key={index}
              data-testid="chapter-filmstrip-tile"
              data-time-seconds={seconds}
              className="absolute inset-y-0 overflow-hidden border-r border-background/70 bg-muted"
              style={{ left: `${leftPercent}%`, width: `${widthPercent}%` }}
            >
              {tilesAvailable && rect && (
                <div
                  className="absolute inset-0 bg-no-repeat"
                  style={{
                    backgroundImage: `url(${seekTilesURL(recordingId)})`,
                    backgroundPosition: `${rect.x}px ${rect.y}px`,
                    backgroundSize: seekTileBackgroundSize(),
                  }}
                />
              )}
            </div>
          )
        })}
        {spans.filter((span) => span.cut).map((span, index) => {
          const safeRangeLength = Math.max(rangeLength, 1)
          const left = ((span.startMs / 1000 - range.startSeconds) / safeRangeLength) * 100
          const width = ((span.endMs - span.startMs) / 1000 / safeRangeLength) * 100
          return (
            <span
              key={`${span.startMs}-${span.endMs}-${index}`}
              data-testid="chapter-filmstrip-cut-range"
              className="pointer-events-none absolute inset-y-0 z-10 bg-chapter-cut-muted/55"
              style={{ left: `${left}%`, width: `${width}%` }}
            />
          )
        })}
        {boundaries.map((boundary) => {
          const displayTime = dragPreviewMs !== null && dragRef.current?.fromSeconds === boundary
            ? dragPreviewMs / 1000
            : boundary
          const selected = selectedForDisplay !== null && Math.abs(displayTime - selectedForDisplay) < 0.001
          const x = filmstripTimeToX(displayTime, range, 100)
          return (
            <button
              key={boundary}
              type="button"
              data-testid="chapter-filmstrip-boundary"
              data-time-ms={Math.round(displayTime * 1000)}
              aria-label={`境界 ${formatChaptersTime(displayTime)}`}
              aria-pressed={selected}
              className={`absolute inset-y-0 z-20 w-1 -translate-x-1/2 touch-none border-0 p-0 ${selected ? 'bg-chapter-selection outline outline-2 outline-foreground' : 'bg-foreground'}`}
              style={{ left: `${x}%` }}
              onPointerDown={(event) => startBoundaryDrag(event, boundary)}
              onPointerMove={moveBoundary}
              onPointerUp={finishBoundaryDrag}
              onPointerCancel={cancelBoundaryDrag}
              onClick={(event) => {
                if (ignoreClickRef.current) {
                  ignoreClickRef.current = false
                  event.stopPropagation()
                  return
                }
                onSelectBoundary(boundary)
                event.stopPropagation()
              }}
              onKeyDown={(event) => {
                if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
                event.preventDefault()
                const direction = event.key === 'ArrowRight' ? 1 : -1
                const next = boundaries[Math.max(0, Math.min(boundaries.length - 1, boundaries.indexOf(boundary) + direction))]
                if (next !== undefined) onSelectBoundary(next)
              }}
            />
          )
        })}
      </div>

      <div data-testid="chapter-tuning-controls" className="flex min-h-11 flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground">選んでいる境界</span>
        <output data-testid="chapter-selected-boundary" className="rounded bg-muted px-2 py-1 font-mono">
          {selectedBoundary === null ? '—' : formatChaptersTime(selectedBoundary)}
        </output>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界を1秒戻す" disabled={selectedBoundary === null} onClick={() => nudgeSelected(-1)}>
          −1秒
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界を1フレーム戻す" disabled={selectedBoundary === null} onClick={() => nudgeSelected(-1001 / 30000)}>
          −1フレーム
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界を 1 フレーム進める" disabled={selectedBoundary === null} onClick={() => nudgeSelected(1001 / 30000)}>
          +1フレーム
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界を1秒進める" disabled={selectedBoundary === null} onClick={() => nudgeSelected(1)}>
          +1秒
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界の前後3秒を再生" disabled={selectedBoundary === null} onClick={() => selectedBoundary !== null && onPlayAround(selectedBoundary)}>
          前後3秒を再生
        </Button>
        <Button type="button" size="sm" variant="outline" aria-label="選択中の境界を現在の再生位置に合わせる" disabled={selectedBoundary === null} onClick={() => selectedBoundary !== null && nudgeSelected(currentSeconds - selectedBoundary)}>
          再生位置に合わせる
        </Button>
        <span className="ml-auto text-xs text-muted-foreground">← → で前後の境界へ移る</span>
      </div>

      {tilesAvailable ? null : (
        <img
          src={seekTilesURL(recordingId)}
          alt=""
          className="pointer-events-none absolute size-px opacity-0"
          onLoad={onTileImageLoad}
          onError={onTileImageError}
        />
      )}
    </section>
  )
}
