import { useCallback, useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react'

import type { ChapterSpan } from '@/api/generated'
import { Button } from '@/components/ui/button'
import {
  defaultFilmstripRangeSeconds,
  filmstripTicks,
  filmstripTileIndices,
  filmstripTimeToX,
  filmstripXToTime,
  minFilmstripRangeSeconds,
  type FilmstripRange,
} from '@/lib/chapter-filmstrip'
import {
  chapterBoundaries,
  chapterSpanIndexesAtBoundary,
  displayedFrameBoundaryMs,
  normalizeChapterDraft,
  nudgeBoundary,
} from '@/lib/chapters'
import { formatPlaybackTime, formatPlaybackTimeMs } from '@/lib/format'
import {
  SEEK_TILES_COLUMNS,
  SEEK_TILES_HEIGHT,
  SEEK_TILES_INTERVAL_SECONDS,
  SEEK_TILES_WIDTH,
  seekTileCell,
  seekTilesURL,
} from '@/lib/seek-tiles'

type RecordingChapterFilmstripProps = {
  recordingId: number
  durationSeconds: number
  currentSeconds: number
  getDisplayedFrameSeconds: () => number | null
  spans: ChapterSpan[]
  selectedBoundary: number | null
  tilesAvailable: boolean
  onTileImageLoad: () => void
  onTileImageError: () => void
  onSeek: (seconds: number) => void
  onSelectBoundary: (seconds: number) => void
  onChangeSpans: (spans: ChapterSpan[], operatedIndexes?: readonly number[]) => void
  onPlayAround: (seconds: number) => void
}

function rangeAround(center: number, length: number, duration: number, minLength: number): FilmstripRange {
  const safeDuration = Number.isFinite(duration) && duration > 0 ? duration : 0
  const safeLength = Math.min(safeDuration, Math.max(minLength, length))
  const start = Math.max(0, Math.min(safeDuration - safeLength, center - safeLength / 2))
  return { startSeconds: start, endSeconds: start + safeLength }
}

/**
 * RecordingChapterFilmstrip は原本の時間軸に沿ってシークタイルと編集境界を描く。
 *
 * 1 マスは 10 秒のタイル 1 枚をそのまま縮めて出す（マスいっぱいの 16:9）。**拡大の上限は 1 マスが
 * タイルの実画素幅（160px）を超えない範囲**で、それより広げると 1 枚が引き伸ばされて粗くなるだけで
 * 情報が増えない（`minFilmstripRangeSeconds`）。
 */
export function RecordingChapterFilmstrip({
  recordingId,
  durationSeconds,
  currentSeconds,
  getDisplayedFrameSeconds,
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
  // 編集モードに入った時点の再生位置。開いた直後の表示範囲はこの周りにし、再生が進んでも追わない。
  const [openedAt] = useState(currentSeconds)
  const [trackWidth, setTrackWidth] = useState(0)
  const [userRange, setUserRange] = useState<FilmstripRange | null>(null)
  const [dragPreview, setDragPreview] = useState<{ from: number; ms: number; moved: boolean } | null>(null)
  const trackRef = useRef<HTMLDivElement | null>(null)
  const dragRef = useRef<{ fromSeconds: number; downX: number; moved: boolean } | null>(null)
  const ignoreClickRef = useRef(false)
  const observerRef = useRef<ResizeObserver | null>(null)
  const previewSpans = useMemo(() => {
    if (dragPreview === null || !dragPreview.moved) return spans
    const operatedIndexes = chapterSpanIndexesAtBoundary(spans, dragPreview.from)
    const moved = nudgeBoundary(spans, dragPreview.from, dragPreview.ms / 1000 - dragPreview.from)
    return normalizeChapterDraft(moved, operatedIndexes)
  }, [dragPreview, spans])
  const boundaries = useMemo(() => chapterBoundaries(previewSpans), [previewSpans])
  // 幅は実測が要る（jsdom では測れず 0 のまま。その間は既定の長さで描く）。
  const setTrack = useCallback((element: HTMLDivElement | null) => {
    observerRef.current?.disconnect()
    observerRef.current = null
    trackRef.current = element
    if (element === null || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(() => setTrackWidth(element.getBoundingClientRect().width))
    observer.observe(element)
    observerRef.current = observer
  }, [])

  const minLength = minFilmstripRangeSeconds(trackWidth)
  const range = useMemo(
    () => userRange ?? rangeAround(openedAt, defaultFilmstripRangeSeconds(trackWidth), durationSeconds, minLength),
    [durationSeconds, minLength, openedAt, trackWidth, userRange],
  )
  const tiles = useMemo(() => filmstripTileIndices(range, durationSeconds), [durationSeconds, range])
  const ticks = useMemo(() => filmstripTicks(range, trackWidth), [range, trackWidth])
  const rangeLength = range.endSeconds - range.startSeconds
  const selectedForDisplay = dragPreview === null ? selectedBoundary : dragPreview.ms / 1000
  const overviewStart = durationSeconds > 0 ? (range.startSeconds / durationSeconds) * 100 : 0
  const overviewWidth = durationSeconds > 0 ? (rangeLength / durationSeconds) * 100 : 100
  const cellPercent = rangeLength > 0 ? (SEEK_TILES_INTERVAL_SECONDS / rangeLength) * 100 : 0

  const panToX = (clientX: number, element: HTMLDivElement) => {
    if (durationSeconds <= 0) return
    const rect = element.getBoundingClientRect()
    if (rect.width <= 0) return
    const nextCenter = filmstripXToTime(clientX - rect.left, { startSeconds: 0, endSeconds: durationSeconds }, rect.width)
    setUserRange(rangeAround(nextCenter, rangeLength, durationSeconds, minLength))
  }

  const zoom = (factor: number) => {
    if (durationSeconds <= 0) return
    const center = selectedBoundary ?? currentSeconds
    setUserRange(rangeAround(center, rangeLength * factor, durationSeconds, minLength))
  }

  const moveBoundary = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current
    const track = trackRef.current
    if (!drag || !track) return
    const rect = track.getBoundingClientRect()
    const target = filmstripXToTime(event.clientX - rect.left, range, rect.width)
    drag.moved ||= Math.abs(event.clientX - drag.downX) > 2
    // 選択は押した時に済んでいる。動かす間は表示だけ更新し、カードのハイライトを揺らさない。
    setDragPreview({ from: drag.fromSeconds, ms: Math.round(target * 1000), moved: drag.moved })
  }

  const startBoundaryDrag = (event: ReactPointerEvent<HTMLButtonElement>, boundary: number) => {
    dragRef.current = { fromSeconds: boundary, downX: event.clientX, moved: false }
    // ポインターキャプチャは track に掛ける。境界ノードが正規化プレビューで差し替わりうるため。
    // キャプチャの付け替えで離した時の click がシークにならないよう抑止する。
    ignoreClickRef.current = true
    setDragPreview({ from: boundary, ms: Math.round(boundary * 1000), moved: false })
    onSelectBoundary(boundary)
    trackRef.current?.setPointerCapture?.(event.pointerId)
  }

  const finishBoundaryDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current
    if (!drag) return
    const target = (dragPreview?.ms ?? Math.round(drag.fromSeconds * 1000)) / 1000
    if (drag.moved) {
      onChangeSpans(
        nudgeBoundary(spans, drag.fromSeconds, target - drag.fromSeconds),
        chapterSpanIndexesAtBoundary(spans, drag.fromSeconds),
      )
      onSelectBoundary(target)
      ignoreClickRef.current = true
    }
    setDragPreview(null)
    dragRef.current = null
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId)
    }
  }

  const cancelBoundaryDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    const drag = dragRef.current
    if (!drag) return
    setDragPreview(null)
    dragRef.current = null
    ignoreClickRef.current = false
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId)
    }
  }

  const nudgeSelected = (deltaSeconds: number) => {
    if (selectedBoundary === null) return
    onChangeSpans(
      nudgeBoundary(spans, selectedBoundary, deltaSeconds),
      chapterSpanIndexesAtBoundary(spans, selectedBoundary),
    )
    onSelectBoundary(selectedBoundary + deltaSeconds)
  }

  const alignSelectedToPlayback = () => {
    if (selectedBoundary === null) return
    const targetMs = displayedFrameBoundaryMs(getDisplayedFrameSeconds(), currentSeconds)
    const targetSeconds = targetMs / 1000
    onChangeSpans(
      nudgeBoundary(spans, selectedBoundary, targetSeconds - selectedBoundary),
      chapterSpanIndexesAtBoundary(spans, selectedBoundary),
    )
    onSelectBoundary(targetSeconds)
  }

  const overviewPanStart = (event: ReactPointerEvent<HTMLDivElement>) => {
    event.currentTarget.setPointerCapture?.(event.pointerId)
    panToX(event.clientX, event.currentTarget)
  }

  const action = 'h-9 md:h-8'
  return (
    <section data-testid="chapter-filmstrip" aria-label="チャプターフィルムストリップ" className="flex min-w-0 flex-col gap-1.5">
      <div
        data-testid="chapter-filmstrip-overview"
        className="relative my-1 h-2 min-w-0 touch-none rounded bg-muted"
        onPointerDown={overviewPanStart}
        onPointerMove={(event) => {
          if (event.currentTarget.hasPointerCapture?.(event.pointerId)) panToX(event.clientX, event.currentTarget)
        }}
        aria-label="表示している時間範囲"
      >
        {durationSeconds > 0 && previewSpans.filter((span) => span.cut).map((span, index) => (
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

      <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span className="min-w-0 truncate">
          {formatPlaybackTime(range.startSeconds)}–{formatPlaybackTime(range.endSeconds)} を表示
          <span className="hidden md:inline">（全体 {formatPlaybackTime(0)}–{formatPlaybackTime(durationSeconds)} のうち枠の部分）</span>
        </span>
        <span className="flex shrink-0 gap-1">
          <Button type="button" size="sm" variant="outline" className={action} onClick={() => setUserRange({ startSeconds: 0, endSeconds: durationSeconds })} disabled={durationSeconds <= 0}>
            全体
          </Button>
          <Button type="button" size="sm" variant="outline" className={action} aria-label="フィルムストリップを縮小" onClick={() => zoom(2)} disabled={durationSeconds <= 0}>
            −
          </Button>
          <Button type="button" size="sm" variant="outline" className={action} aria-label="フィルムストリップを拡大" onClick={() => zoom(0.5)} disabled={durationSeconds <= 0 || rangeLength <= minLength + 0.01}>
            ＋
          </Button>
        </span>
      </div>

      <div data-testid="chapter-filmstrip-ticks" aria-hidden className="relative h-4 font-mono text-xs text-muted-foreground">
        {ticks.map((tick) => {
          const x = filmstripTimeToX(tick, range, 100)
          return (
            <span
              key={tick}
              className="absolute top-0 whitespace-nowrap"
              style={{ left: `${x}%`, transform: `translateX(${x < 4 ? '0' : x > 96 ? '-100%' : '-50%'})` }}
            >
              {formatPlaybackTime(tick)}
            </span>
          )
        })}
      </div>

      <div
        ref={setTrack}
        data-testid="chapter-filmstrip-track"
        data-duration-seconds={durationSeconds}
        data-visible-start-seconds={range.startSeconds}
        data-visible-end-seconds={range.endSeconds}
        className="relative min-h-8 min-w-0 touch-none overflow-hidden rounded bg-muted/70"
        onPointerMove={moveBoundary}
        onPointerUp={finishBoundaryDrag}
        onPointerCancel={cancelBoundaryDrag}
        onClick={(event) => {
          if (ignoreClickRef.current) {
            ignoreClickRef.current = false
            return
          }
          const rect = event.currentTarget.getBoundingClientRect()
          onSeek(filmstripXToTime(event.clientX - rect.left, range, rect.width))
        }}
      >
        {/* 帯の高さは 1 マス (16:9) の高さ。マスは絶対配置なので、同じ幅の見えない箱で高さを作る。 */}
        <div aria-hidden className="invisible" style={{ width: `${cellPercent}%`, aspectRatio: '16 / 9' }} />
        {tiles.map((index) => {
          const seconds = index * SEEK_TILES_INTERVAL_SECONDS
          const cell = seekTileCell(seconds)
          const leftPercent = rangeLength > 0 ? ((seconds - range.startSeconds) / rangeLength) * 100 : 0
          return (
            <div
              key={index}
              data-testid="chapter-filmstrip-tile"
              data-time-seconds={seconds}
              className="absolute top-0 overflow-hidden border-r border-background/70 bg-muted"
              style={{ left: `${leftPercent}%`, width: `${cellPercent}%`, aspectRatio: '16 / 9', containerType: 'inline-size' }}
            >
              {tilesAvailable && cell && (
                // 格子全体を 1 マスの幅の列数倍に縮め（background-size は %）、位置は 1 マスの幅 (cqw) 単位で送る。
                <div
                  className="size-full bg-no-repeat"
                  style={{
                    backgroundImage: `url(${seekTilesURL(recordingId)})`,
                    backgroundSize: `${SEEK_TILES_COLUMNS * 100}% auto`,
                    backgroundPosition: `${-cell.column * 100}cqw ${-cell.row * (SEEK_TILES_HEIGHT / SEEK_TILES_WIDTH) * 100}cqw`,
                  }}
                />
              )}
            </div>
          )
        })}
        {previewSpans.filter((span) => span.cut).map((span, index) => {
          const safeRangeLength = Math.max(rangeLength, 1)
          const left = ((span.startMs / 1000 - range.startSeconds) / safeRangeLength) * 100
          const width = ((span.endMs - span.startMs) / 1000 / safeRangeLength) * 100
          return (
            <span
              key={`${span.startMs}-${span.endMs}-${index}`}
              data-testid="chapter-filmstrip-cut-range"
              className="pointer-events-none absolute inset-y-0 z-10 border-t-4 border-chapter-cut bg-chapter-cut-muted/55"
              style={{ left: `${left}%`, width: `${width}%` }}
            />
          )
        })}
        {boundaries.map((boundary) => {
          const displayTime = boundary
          const selected = selectedForDisplay !== null && Math.abs(displayTime - selectedForDisplay) < 0.001
          const x = filmstripTimeToX(displayTime, range, 100)
          return (
            <button
              key={boundary}
              type="button"
              data-testid="chapter-filmstrip-boundary"
              data-time-ms={Math.round(displayTime * 1000)}
              aria-label={`境界 ${formatPlaybackTimeMs(displayTime)}`}
              aria-pressed={selected}
              className={`absolute inset-y-0 z-20 w-1 -translate-x-1/2 touch-none border-0 p-0 ${selected ? 'bg-chapter-selection outline outline-2 outline-foreground' : 'bg-foreground'}`}
              style={{ left: `${x}%` }}
              onPointerDown={(event) => startBoundaryDrag(event, boundary)}
              onClick={(event) => {
                event.stopPropagation()
                if (ignoreClickRef.current) {
                  ignoreClickRef.current = false
                  return
                }
                onSelectBoundary(boundary)
              }}
            />
          )
        })}
      </div>

      <div data-testid="chapter-tuning-controls" className="flex min-h-11 flex-wrap items-center gap-1.5 md:gap-2">
        <span className="hidden text-sm text-muted-foreground md:inline">選んでいる境界</span>
        <output data-testid="chapter-selected-boundary" className="rounded bg-muted px-2 py-1 font-mono">
          {selectedBoundary === null ? '—' : formatPlaybackTimeMs(selectedBoundary)}
        </output>
        <Button type="button" size="sm" variant="outline" className={action} aria-label="選択中の境界を1秒戻す" disabled={selectedBoundary === null} onClick={() => nudgeSelected(-1)}>
          −1秒
        </Button>
        <Button type="button" size="sm" variant="outline" className={action} aria-label="選択中の境界を1フレーム戻す" disabled={selectedBoundary === null} onClick={() => nudgeSelected(-1001 / 30000)}>
          <span className="md:hidden">−1f</span><span className="hidden md:inline">−1フレーム</span>
        </Button>
        <Button type="button" size="sm" variant="outline" className={action} aria-label="選択中の境界を 1 フレーム進める" disabled={selectedBoundary === null} onClick={() => nudgeSelected(1001 / 30000)}>
          <span className="md:hidden">+1f</span><span className="hidden md:inline">+1フレーム</span>
        </Button>
        <Button type="button" size="sm" variant="outline" className={action} aria-label="選択中の境界を1秒進める" disabled={selectedBoundary === null} onClick={() => nudgeSelected(1)}>
          +1秒
        </Button>
        <Button type="button" size="sm" variant="outline" className={`${action} hidden md:inline-flex`} aria-label="選択中の境界の前後3秒を再生" disabled={selectedBoundary === null} onClick={() => selectedBoundary !== null && onPlayAround(selectedBoundary)}>
          前後3秒を再生
        </Button>
        <Button type="button" size="sm" variant="outline" className={`${action} hidden md:inline-flex`} aria-label="選択中の境界を現在の再生位置に合わせる" disabled={selectedBoundary === null} onClick={alignSelectedToPlayback}>
          再生位置に合わせる
        </Button>
        <span className="ml-auto hidden text-xs text-muted-foreground md:inline">← → で前後の境界へ移る</span>
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
