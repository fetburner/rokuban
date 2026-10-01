import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { ArrowLeft, ChevronLeft, ChevronRight, Maximize2, ScanLine, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getListCMLogosQueryKey,
  useAdoptCMLogoCandidate,
  useDeleteCMLogo,
  useDeleteCMLogoCandidate,
  useDeleteCMLogoArea,
  useListCMLogos,
  useListRecordings,
  usePutCMLogoArea,
  useRetryRecordingCMDetection,
  type CMLogoState,
  type Recording,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, EmptyState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { Field, Input, Select } from '@/components/ui/field'
import { useCMDetectEnabled } from '@/lib/capabilities'
import {
  clampCodedRect,
  codedRectFromPoints,
  codedToFrame,
  containsCodedPoint,
  FRAME_ZOOM,
  frameImageBox,
  frameImageOffset,
  frameScale,
  frameToCoded,
  MIN_AREA_SIZE,
  moveCodedRect,
  resizeCodedRect,
  savedAreaMatchesFrame,
  type CodedRect,
  type FrameView,
  type ResizeHandle,
} from '@/lib/cm-logo-frame'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { formatDateTime, formatDuration } from '@/lib/format'
import { cmDetectStageMessage } from '@/lib/cm-detect-stage'
import { mutationErrorMessage } from '@/lib/mutation-error-message'
import { cn } from '@/lib/utils'

/** frameURL は streamer のコマ切り出し URL を組み立てる（OpenAPI 外）。 */
// oxlint-disable-next-line react/only-export-components -- URL の契約をテストで固定する
export function frameURL(recordingId: number, atMs: number): string {
  return `/api/media/recordings/${recordingId}/frame?at=${atMs}`
}

export type CMLogoBucket = 'attention' | 'pending' | 'healthy'

/** cmLogoBucket は一覧の「見るべき順」を API の件数から決める。 */
// oxlint-disable-next-line react/only-export-components -- 一覧の並び契約を単体テストする
export function cmLogoBucket(logo: CMLogoState): CMLogoBucket {
  if (logo.candidate?.state === 'failed' || logo.candidate?.state === 'ready') return 'attention'
  if (logo.candidate?.state === 'running') return 'pending'
  if (logo.failedCount > 0) return 'attention'
  if (logo.pendingCount > 0) return 'pending'
  return 'healthy'
}

/** cmLogoStateSentence は行に置く状態説明を一文へ畳む。 */
// oxlint-disable-next-line react/only-export-components -- 表示順テストから共有する
export function cmLogoStateSentence(logo: CMLogoState): string {
  if (logo.candidate?.state === 'running') return 'ロゴ候補を解析中です'
  if (logo.candidate?.state === 'failed') return cmDetectStageMessage(logo.candidate.stage)
  if (logo.candidate?.state === 'ready') return 'ロゴ候補を確認して採用してください'
  if (logo.failedCount > 0) return cmDetectStageMessage(logo.lastFailureStage)
  if (logo.pendingCount > 0) return `検出待ち ${logo.pendingCount} 件`
  return `録画 ${logo.recordingCount} 件`
}

function serviceKey(networkId: number, serviceId: number): number {
  return networkId * 100000 + serviceId
}

function stateBadge(logo: CMLogoState): { label: string; attention: boolean } {
  const bucket = cmLogoBucket(logo)
  if (logo.candidate?.state === 'ready') return { label: '候補あり', attention: false }
  if (bucket === 'attention') return { label: '要対応', attention: true }
  if (bucket === 'pending') return { label: '検出待ち', attention: false }
  return { label: '問題なし', attention: false }
}

function parseSampleAspectRatio(value: string | null): number {
  if (value === null || value.trim() === '') return 1
  const match = value.trim().match(/^(\d+(?:\.\d+)?)[/:](\d+(?:\.\d+)?)$/)
  if (match) {
    const numerator = Number(match[1])
    const denominator = Number(match[2])
    if (numerator > 0 && denominator > 0) return numerator / denominator
  }
  const ratio = Number(value)
  return Number.isFinite(ratio) && ratio > 0 ? ratio : 1
}

type Frame = {
  url: string
  codedWidth: number
  codedHeight: number
  sampleAspectRatio: number
}

/** useFrame は保存した時刻を確定したときだけ原本のコマを取り寄せる。 */
function useFrame(
  recordingId: number,
  atMs: number | undefined,
): { frame: Frame | null; failed: boolean; loading: boolean } {
  const [frame, setFrame] = useState<Frame | null>(null)
  const [failed, setFailed] = useState(false)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (recordingId <= 0 || atMs === undefined) {
      // oxlint-disable-next-line react/set-state-in-effect -- コマ選択が無効になったとき前の取得状態を捨てる
      setFrame(null)
      // oxlint-disable-next-line react/set-state-in-effect -- コマ選択が無効になったとき前の取得状態を捨てる
      setFailed(false)
      // oxlint-disable-next-line react/set-state-in-effect -- コマ選択が無効になったとき前の取得状態を捨てる
      setLoading(false)
      return
    }
    let cancelled = false
    let objectURL: string | undefined
    // oxlint-disable-next-line react/set-state-in-effect -- 確定したコマの取得開始時に前の画像を消す
    setFrame(null)
    // oxlint-disable-next-line react/set-state-in-effect -- 確定したコマの取得開始時にエラーを消す
    setFailed(false)
    // oxlint-disable-next-line react/set-state-in-effect -- 確定したコマの取得中表示を同期する
    setLoading(true)
    fetch(frameURL(recordingId, atMs))
      .then(async (response) => {
        if (!response.ok) throw new Error(`status ${response.status}`)
        const blob = await response.blob()
        const codedWidth = Number(response.headers.get('X-Coded-Width'))
        const codedHeight = Number(response.headers.get('X-Coded-Height'))
        if (cancelled || codedWidth <= 0 || codedHeight <= 0) throw new Error('missing coded size')
        objectURL = URL.createObjectURL(blob)
        setFrame({
          url: objectURL,
          codedWidth,
          codedHeight,
          sampleAspectRatio: parseSampleAspectRatio(response.headers.get('X-Sample-Aspect-Ratio')),
        })
      })
      .catch(() => {
        if (!cancelled) {
          setFrame(null)
          setFailed(true)
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [recordingId, atMs])

  return { frame, failed, loading }
}

function useBoxSize(ref: React.RefObject<HTMLElement | null>): { width: number; height: number } {
  const [size, setSize] = useState({ width: 0, height: 0 })
  useEffect(() => {
    const element = ref.current
    if (!element) return
    const publish = () => setSize({ width: element.clientWidth, height: element.clientHeight })
    publish()
    const observer = new ResizeObserver(publish)
    observer.observe(element)
    return () => observer.disconnect()
  }, [ref])
  return size
}

function formatPosition(atMs: number | undefined): string {
  if (atMs === undefined) return '時刻を選んでください'
  const total = Math.floor(atMs / 1000)
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}

function recordingLabel(recording: Recording): string {
  return `${formatDateTime(recording.startAt)} ${recording.title}（${formatDuration(recording.durationMs)}）`
}

/**
 * awaitingCandidateAnalysis は、枠が教えられているのに候補の行がまだ無い局を判定する。
 *
 * 枠の保存・候補の破棄・覚えたロゴの削除の直後は、job が CMDetectQueue で待つ間
 * running 行が存在しない。cm_logo_candidate_desired view と同じ条件
 * （枠あり・候補なし・学習が無い、または枠より古い）を画面側でも使い、メモリ state に頼らない。
 */
function awaitingCandidateAnalysis(logo: CMLogoState): boolean {
  if (!logo.logoArea || logo.candidate !== undefined) return false
  return !logo.learnedAt || Date.parse(logo.learnedAt) < Date.parse(logo.logoArea.updatedAt)
}

/** isAwaitingAdoption は worker が採用待ちの局として止めた録画かを返す（再試行では進まない）。 */
function isAwaitingAdoption(recording: Recording): boolean {
  const stage: string | null | undefined = recording.cmDetection.stage
  return recording.cmDetection.state === 'failed' && stage === 'adopt'
}

function recordingCMState(recording: Recording): string {
  switch (recording.cmDetection.state) {
    case 'detected':
      return '検出済み'
    case 'detecting':
      return '検出中、または再試行待ち'
    case 'failed':
      return isAwaitingAdoption(recording) ? cmDetectStageMessage('adopt') : '3 回の試行に失敗しました'
    default:
      return '無効'
  }
}

type DragState =
  | {
      mode: 'draw'
      start: { x: number; y: number }
      origin: CodedRect
    }
  | {
      mode: 'move'
      start: { x: number; y: number }
      origin: CodedRect
    }
  | {
      mode: 'resize'
      handle: ResizeHandle
      start: { x: number; y: number }
      origin: CodedRect
    }

function resizeHandleAt(
  rect: CodedRect,
  point: { x: number; y: number },
  scale: { x: number; y: number },
): ResizeHandle | undefined {
  const toleranceX = Math.max(12 / Math.max(scale.x, 0.001), MIN_AREA_SIZE)
  const toleranceY = Math.max(12 / Math.max(scale.y, 0.001), MIN_AREA_SIZE)
  const corners: Array<[ResizeHandle, number, number]> = [
    ['nw', rect.x, rect.y],
    ['ne', rect.x + rect.w, rect.y],
    ['sw', rect.x, rect.y + rect.h],
    ['se', rect.x + rect.w, rect.y + rect.h],
  ]
  return corners.find(([, x, y]) => Math.abs(point.x - x) <= toleranceX && Math.abs(point.y - y) <= toleranceY)?.[0]
}

function cursorForPoint(
  rect: CodedRect | undefined,
  point: { x: number; y: number },
  scale: { x: number; y: number },
): string {
  if (rect) {
    const handle = resizeHandleAt(rect, point, scale)
    if (handle === 'nw' || handle === 'se') return 'nwse-resize'
    if (handle === 'ne' || handle === 'sw') return 'nesw-resize'
    if (containsCodedPoint(rect, point)) return 'move'
  }
  return 'crosshair'
}

function defaultRect(frame: Frame): CodedRect {
  const w = Math.max(MIN_AREA_SIZE, Math.round(frame.codedWidth / 4))
  const h = Math.max(MIN_AREA_SIZE, Math.round(frame.codedHeight / 4))
  return clampCodedRect(
    {
      x: (frame.codedWidth - w) / 2,
      y: (frame.codedHeight - h) / 2,
      w,
      h,
    },
    frame.codedWidth,
    frame.codedHeight,
    MIN_AREA_SIZE,
  )
}

function FrameOutsideDim({
  rect,
  view,
}: {
  rect: CodedRect
  view: FrameView
}) {
  const scale = frameScale(view)
  const offset = frameImageOffset(view)
  const left = offset.x + rect.x * scale.x
  const top = offset.y + rect.y * scale.y
  const right = left + rect.w * scale.x
  const bottom = top + rect.h * scale.y
  const common = {
    position: 'absolute' as const,
    background: 'color-mix(in oklch, var(--foreground) 38%, transparent)',
    pointerEvents: 'none' as const,
  }
  return (
    <>
      <div style={{ ...common, left: 0, top: 0, width: Math.max(0, left), bottom: 0 }} />
      <div style={{ ...common, left: right, top: 0, right: 0, bottom: 0 }} />
      <div style={{ ...common, left, top: 0, width: Math.max(0, right - left), height: Math.max(0, top) }} />
      <div style={{ ...common, left, top: bottom, width: Math.max(0, right - left), bottom: 0 }} />
    </>
  )
}

function FrameHandle({
  handle,
  point,
}: {
  handle: ResizeHandle
  point: { x: number; y: number }
}) {
  const cursor = handle === 'nw' || handle === 'se' ? 'nwse-resize' : 'nesw-resize'
  return (
    <span
      data-testid={`cm-logo-handle-${handle}`}
      aria-hidden="true"
      className="pointer-events-auto absolute z-20 flex size-11 -translate-x-1/2 -translate-y-1/2 items-center justify-center"
      style={{ left: point.x, top: point.y, cursor }}
    >
      <span className="size-3 border-2 border-background bg-foreground" />
    </span>
  )
}

function CMLogoFrameEditor({
  logo,
  recordings,
  requestedRecordingId,
}: {
  logo: CMLogoState
  recordings: Recording[]
  requestedRecordingId?: number
}) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const frameRef = useRef<HTMLDivElement>(null)
  const dragRef = useRef<DragState | null>(null)
  const appliedFrameKey = useRef<string | null>(null)
  const candidates = useMemo(
    () => recordings.filter((recording) => recording.sizeBytes !== undefined),
    [recordings],
  )
  const initialRecordingId =
    (requestedRecordingId !== undefined && candidates.some((recording) => recording.id === requestedRecordingId)
      ? requestedRecordingId
      : undefined) ??
    (candidates.some((recording) => recording.id === logo.frameRecordingId)
      ? logo.frameRecordingId
      : candidates[0]?.id ?? 0)
  const [recordingId, setRecordingId] = useState(initialRecordingId)
  const selectedRecording = candidates.find((recording) => recording.id === recordingId)
  const durationMs = Math.max(1, selectedRecording?.durationMs ?? 1)
  const [sliderValue, setSliderValue] = useState(Math.round(durationMs / 2))
  const [committedAtMs, setCommittedAtMs] = useState<number | undefined>(Math.round(durationMs / 2))
  const [zoom, setZoom] = useState(1)
  const [rect, setRect] = useState<CodedRect | undefined>(undefined)
  const [cursor, setCursor] = useState('crosshair')
  const box = useBoxSize(frameRef)
  const { frame, failed, loading } = useFrame(recordingId, committedAtMs)
  const saveArea = usePutCMLogoArea()

  useEffect(() => {
    if (recordingId === 0 && initialRecordingId !== 0) {
      // oxlint-disable-next-line react/set-state-in-effect -- API 後に初期録画を確定する
      setRecordingId(initialRecordingId)
    }
  }, [initialRecordingId, recordingId])

  useEffect(() => {
    const middle = Math.round(durationMs / 2)
    // oxlint-disable-next-line react/set-state-in-effect -- 録画選択時にスライダーを中央へ戻す
    setSliderValue(middle)
    // oxlint-disable-next-line react/set-state-in-effect -- 録画選択時に中央のコマを確定する
    setCommittedAtMs(middle)
  }, [durationMs, recordingId])

  useEffect(() => {
    if (!frame) return
    const key = `${recordingId}:${frame.codedWidth}x${frame.codedHeight}`
    if (appliedFrameKey.current === key) return
    appliedFrameKey.current = key
    setRect(
      savedAreaMatchesFrame(logo.logoArea, frame)
        ? clampCodedRect(logo.logoArea!, frame.codedWidth, frame.codedHeight, MIN_AREA_SIZE)
        : undefined,
    )
  }, [frame, logo.logoArea, recordingId])

  const view: FrameView = {
    codedWidth: frame?.codedWidth ?? 0,
    codedHeight: frame?.codedHeight ?? 0,
    boxWidth: box.width,
    boxHeight: box.height,
    sampleAspectRatio: frame?.sampleAspectRatio,
    zoom,
    focus: rect ? { x: rect.x + rect.w / 2, y: rect.y + rect.h / 2 } : undefined,
  }
  const scale = frameScale(view)
  const imageBox = frameImageBox(view)
  const imageOffset = frameImageOffset(view)
  const rectOnScreen = rect ? codedToFrame({ x: rect.x, y: rect.y }, view) : undefined
  const canSave = frame !== null && rect !== undefined && !saveArea.isPending
  const areaMismatch =
    logo.logoArea !== undefined &&
    frame !== null &&
    !savedAreaMatchesFrame(logo.logoArea, frame)

  const codedPoint = (event: React.PointerEvent<HTMLDivElement>) => {
    if (!frame || !frameRef.current) return undefined
    const bounds = frameRef.current.getBoundingClientRect()
    const point = frameToCoded(
      { x: event.clientX - bounds.left, y: event.clientY - bounds.top },
      view,
    )
    return {
      x: Math.min(Math.max(point.x, 0), frame.codedWidth),
      y: Math.min(Math.max(point.y, 0), frame.codedHeight),
    }
  }

  const updateCursor = (event: React.PointerEvent<HTMLDivElement>) => {
    const point = codedPoint(event)
    if (!point) return
    setCursor(cursorForPoint(rect, point, scale))
  }

  const commitSlider = (value: number) => {
    const next = Math.min(Math.max(Math.round(value), 0), durationMs)
    setSliderValue(next)
    setCommittedAtMs(next)
  }

  const numericRect = rect ?? (frame ? defaultRect(frame) : undefined)
  const updateNumeric = (key: keyof CodedRect, value: string) => {
    if (!frame || value === '') return
    const number = Number(value)
    if (!Number.isFinite(number)) return
    const next = clampCodedRect(
      { ...(numericRect ?? defaultRect(frame)), [key]: number },
      frame.codedWidth,
      frame.codedHeight,
      MIN_AREA_SIZE,
    )
    setRect(next)
  }

  const onSave = () => {
    if (!frame || !rect) return
    const clamped = clampCodedRect(rect, frame.codedWidth, frame.codedHeight, MIN_AREA_SIZE)
    saveArea.mutate(
      {
        networkId: logo.networkId,
        serviceId: logo.serviceId,
        data: {
          recordingId,
          ...clamped,
          codedWidth: frame.codedWidth,
          codedHeight: frame.codedHeight,
        },
      },
      {
        onSuccess: () => {
          void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
          void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
          toast({ message: `${logo.serviceName} の解析を依頼しました` })
        },
        onError: (error) =>
          toast({ message: mutationErrorMessage('枠の保存に失敗しました', error), kind: 'error' }),
      },
    )
  }

  if (candidates.length === 0 || recordingId <= 0) {
    return (
      <p className="border-t border-border/60 pt-3 text-muted-foreground" data-testid="cm-logo-no-original">
        原本のある録画がありません。原本を消した録画からはコマを取り寄せられません。
      </p>
    )
  }

  return (
    <section className="flex flex-col gap-4" aria-label="ロゴの枠">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_18rem]">
        <div className="min-w-0">
          <div
            ref={frameRef}
            data-testid="cm-logo-frame"
            className="relative aspect-video touch-none overflow-hidden rounded border border-border bg-muted"
            style={{ cursor }}
            onPointerDown={(event) => {
              const point = codedPoint(event)
              if (!point || !frame) return
              const handle = rect ? resizeHandleAt(rect, point, scale) : undefined
              if (rect && handle) {
                dragRef.current = { mode: 'resize', handle, start: point, origin: rect }
              } else if (rect && containsCodedPoint(rect, point)) {
                dragRef.current = { mode: 'move', start: point, origin: rect }
              } else {
                dragRef.current = {
                  mode: 'draw',
                  start: point,
                  origin: rect ?? { x: point.x, y: point.y, w: MIN_AREA_SIZE, h: MIN_AREA_SIZE },
                }
                setRect(clampCodedRect({ x: point.x, y: point.y, w: MIN_AREA_SIZE, h: MIN_AREA_SIZE }, frame.codedWidth, frame.codedHeight, MIN_AREA_SIZE))
              }
              setCursor('crosshair')
              event.currentTarget.setPointerCapture(event.pointerId)
            }}
            onPointerMove={(event) => {
              const point = codedPoint(event)
              if (!point || !frame) return
              const drag = dragRef.current
              if (!drag) {
                updateCursor(event)
                return
              }
              let next: CodedRect
              if (drag.mode === 'draw') {
                next = clampCodedRect(
                  codedRectFromPoints(drag.start, point),
                  frame.codedWidth,
                  frame.codedHeight,
                  MIN_AREA_SIZE,
                )
              } else if (drag.mode === 'move') {
                next = moveCodedRect(
                  drag.origin,
                  point.x - drag.start.x,
                  point.y - drag.start.y,
                  frame.codedWidth,
                  frame.codedHeight,
                )
              } else {
                next = resizeCodedRect(
                  drag.origin,
                  drag.handle,
                  point,
                  frame.codedWidth,
                  frame.codedHeight,
                  MIN_AREA_SIZE,
                )
              }
              setRect(next)
            }}
            onPointerUp={(event) => {
              dragRef.current = null
              event.currentTarget.releasePointerCapture(event.pointerId)
              updateCursor(event)
            }}
            onPointerCancel={() => {
              dragRef.current = null
            }}
          >
            {frame && (
              <img
                src={frame.url}
                alt={`${logo.serviceName} の ${formatPosition(committedAtMs)} のコマ`}
                draggable={false}
                data-testid="cm-logo-frame-image"
                className="pointer-events-none absolute select-none"
                style={{ left: imageOffset.x, top: imageOffset.y, width: imageBox.width, height: imageBox.height }}
              />
            )}
            {rect && frame && <FrameOutsideDim rect={rect} view={view} />}
            {rect && rectOnScreen && (
              <>
                <div
                  data-testid="cm-logo-rect"
                  className="pointer-events-none absolute z-10 border-2 border-background"
                  style={{
                    left: rectOnScreen.x,
                    top: rectOnScreen.y,
                    width: rect.w * scale.x,
                    height: rect.h * scale.y,
                    boxShadow: '0 0 0 1px var(--foreground)',
                  }}
                />
                {(['nw', 'ne', 'sw', 'se'] as const).map((handle) => (
                  <FrameHandle
                    key={handle}
                    handle={handle}
                    point={codedToFrame(
                      {
                        x: handle.includes('w') ? rect.x : rect.x + rect.w,
                        y: handle.includes('n') ? rect.y : rect.y + rect.h,
                      },
                      view,
                    )}
                  />
                ))}
              </>
            )}
            {!frame && loading && <span className="absolute inset-0 grid place-items-center text-sm text-muted-foreground">コマを取り寄せています</span>}
          </div>

          <input
            aria-label="コマの時刻"
            data-testid="cm-logo-time-slider"
            type="range"
            min={0}
            max={durationMs}
            step={1}
            value={Math.min(sliderValue, durationMs)}
            onChange={(event) => setSliderValue(Number(event.currentTarget.value))}
            onPointerUp={(event) => commitSlider(Number(event.currentTarget.value))}
            onKeyUp={(event) => commitSlider(Number(event.currentTarget.value))}
            onBlur={(event) => commitSlider(Number(event.currentTarget.value))}
            className="mt-2 w-full cursor-pointer"
          />
          <div className="mt-1 flex items-center justify-between text-xs text-muted-foreground">
            <span data-testid="cm-logo-position">時刻 {formatPosition(committedAtMs)}</span>
            <span>{formatDuration(durationMs)}</span>
          </div>
          {failed && <p className="mt-2 text-destructive" role="alert">この時刻のコマを取り寄せできませんでした。別の時刻を選んでください。</p>}
        </div>

        <aside className="flex flex-col gap-3 text-sm">
          <div>
            <LearnedLogo logo={logo} />
          </div>
          <div>
            <h3 className="font-medium">① 時刻と録画</h3>
            <div className="mt-2 flex items-center gap-2">
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label="前のコマ"
                onClick={() => commitSlider(sliderValue - Math.max(1, Math.round(durationMs / 100)))}
              >
                <ChevronLeft />
              </Button>
              <span className="min-w-20 text-center" data-testid="cm-logo-committed-time">{formatPosition(committedAtMs)}</span>
              <Button
                type="button"
                size="icon-sm"
                variant="ghost"
                aria-label="次のコマ"
                onClick={() => commitSlider(sliderValue + Math.max(1, Math.round(durationMs / 100)))}
              >
                <ChevronRight />
              </Button>
            </div>
            <Field label="録画を選ぶ" className="mt-2">
              <Select
                value={recordingId}
                onChange={(event) => setRecordingId(Number(event.currentTarget.value))}
                data-testid="cm-logo-recording-select"
              >
                {candidates.map((recording) => (
                  <option key={recording.id} value={recording.id}>{recordingLabel(recording)}</option>
                ))}
              </Select>
            </Field>
          </div>

          <div>
            <h3 className="font-medium">② 枠の座標（記録上の画素）</h3>
            <div className="mt-2 grid grid-cols-2 gap-2">
              {(['x', 'y', 'w', 'h'] as const).map((key) => (
                <Field key={key} label={key === 'x' ? 'X' : key === 'y' ? 'Y' : key === 'w' ? '幅' : '高さ'}>
                  <Input
                    data-testid={`cm-logo-field-${key}`}
                    type="number"
                    min={0}
                    value={numericRect?.[key] ?? ''}
                    disabled={!frame}
                    onChange={(event) => updateNumeric(key, event.currentTarget.value)}
                  />
                </Field>
              ))}
            </div>
            <div className="mt-2 flex flex-wrap gap-2">
              <Button type="button" size="sm" variant="outline" disabled={!rect} onClick={() => {
                setZoom(FRAME_ZOOM)
              }}>
                <Maximize2 data-icon="inline-start" />
                枠に寄る
              </Button>
              <Button type="button" size="sm" variant="ghost" onClick={() => setZoom(1)}>全体</Button>
            </div>
          </div>

          <div>
            <h3 className="font-medium">③ ロゴを解析</h3>
            {areaMismatch && (
              <p className="mt-1 text-xs text-destructive" role="alert" data-testid="cm-logo-area-mismatch">
                保存済みの枠は {logo.logoArea?.codedWidth}×{logo.logoArea?.codedHeight} 用です。このコマは{' '}
                {frame?.codedWidth}×{frame?.codedHeight} なので、枠はこの録画には使われません。
              </p>
            )}
            <p className="mt-1 text-xs text-muted-foreground">この枠でロゴを解析します。採用するまで今のロゴはそのまま使われます。</p>
            <Button type="button" className="mt-2 w-full" disabled={!canSave} onClick={onSave}>ロゴを解析</Button>
          </div>
        </aside>
      </div>

      <CMLogoCandidatePanel logo={logo} recordings={recordings} />

      <p className="text-xs text-muted-foreground">
        枠の外をドラッグして描き、枠の中をドラッグして動かします。四隅をドラッグすると大きさを変えられます。
      </p>
    </section>
  )
}

function LearnedLogo({ logo }: { logo: CMLogoState }) {
  return (
    <div className="flex items-center gap-3">
      <div className="flex h-12 w-24 items-center justify-center overflow-hidden rounded border border-border bg-muted">
        {logo.previewPng ? (
          <img src={`data:image/png;base64,${logo.previewPng}`} alt={`${logo.serviceName} のロゴ`} className="max-h-full max-w-full object-contain" />
        ) : (
          <ScanLine aria-hidden="true" className="size-5 text-muted-foreground" />
        )}
      </div>
      <div className="text-xs text-muted-foreground">
        <p>{logo.learnedAt ? `学習 ${formatDateTime(logo.learnedAt)}` : '学習していません'}</p>
        <p>{logo.logoArea ? `教えた枠 ${logo.logoArea.w}×${logo.logoArea.h}` : '枠は自動の探索'}</p>
      </div>
    </div>
  )
}

const checkerboardStyle = {
  backgroundColor: 'var(--muted)',
  backgroundImage:
    'linear-gradient(45deg, var(--border) 25%, transparent 25%), linear-gradient(-45deg, var(--border) 25%, transparent 25%), linear-gradient(45deg, transparent 75%, var(--border) 75%), linear-gradient(-45deg, transparent 75%, var(--border) 75%)',
  backgroundPosition: '0 0, 0 8px, 8px -8px, -8px 0',
  backgroundSize: '16px 16px',
}

function NativeLogoPreview({
  previewPng,
  sampleAspectRatio,
  alt,
  testId,
}: {
  previewPng?: string
  sampleAspectRatio: number
  alt: string
  testId: string
}) {
  const [intrinsicSize, setIntrinsicSize] = useState<{ width: number; height: number }>()
  useEffect(() => {
    // oxlint-disable-next-line react/set-state-in-effect -- 候補画像の入れ替え時に前の原寸を捨てる
    setIntrinsicSize(undefined)
  }, [previewPng])
  const sar = Number.isFinite(sampleAspectRatio) && sampleAspectRatio > 0 ? sampleAspectRatio : 1
  return (
    <div
      data-testid={testId}
      className="flex min-h-28 min-w-0 overflow-auto rounded border border-border p-3"
      style={checkerboardStyle}
    >
      {previewPng ? (
        <img
          src={`data:image/png;base64,${previewPng}`}
          alt={alt}
          onLoad={(event) => {
            const image = event.currentTarget
            setIntrinsicSize({ width: image.naturalWidth, height: image.naturalHeight })
          }}
          className="m-auto block max-w-none shrink-0 object-contain"
          style={
            intrinsicSize
              ? { width: intrinsicSize.width * sar, height: intrinsicSize.height, maxHeight: 'none' }
              : undefined
          }
        />
      ) : (
        <span className="m-auto text-xs text-muted-foreground">プレビューなし</span>
      )}
    </div>
  )
}

function CandidateRunning() {
  return (
    <section className="flex flex-col gap-1 border-t border-border/60 pt-3" data-testid="cm-logo-candidate-running" aria-live="polite">
      <span className="w-fit rounded bg-muted px-1.5 py-0.5 text-xs">解析中</span>
      <p className="text-sm">解析中です。数分程度かかることがあります。画面を離れても続きます。</p>
    </section>
  )
}

function CMLogoCandidatePanel({
  logo,
  recordings,
}: {
  logo: CMLogoState
  recordings: Recording[]
}) {
  const candidate = logo.candidate
  const queryClient = useQueryClient()
  const toast = useToast()
  const adopt = useAdoptCMLogoCandidate()
  const discard = useDeleteCMLogoCandidate()
  const [redetect, setRedetect] = useState(true)
  const [adopted, setAdopted] = useState(false)
  const candidateRecordingId = candidate?.state === 'ready' ? (candidate.recordingId ?? 0) : 0
  const candidateRecording = recordings.find((recording) => recording.id === candidateRecordingId)
  const { frame: candidateFrame } = useFrame(candidateRecordingId, candidateRecordingId > 0 ? 0 : undefined)
  const sampleAspectRatio = candidateFrame?.sampleAspectRatio ?? 1

  useEffect(() => {
    if (candidate !== undefined) {
      // oxlint-disable-next-line react/set-state-in-effect -- 新しい候補は採用完了表示を閉じる
      setAdopted(false)
    }
  }, [candidate])

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  }

  if (candidate === undefined) {
    if (awaitingCandidateAnalysis(logo)) return <CandidateRunning />
    if (!adopted || logo.pendingCount <= 0) return null
    return (
      <p className="text-sm" role="status" data-testid="cm-logo-candidate-adopted">
        検出待ち {logo.pendingCount} 件。数分〜数十分かかります
      </p>
    )
  }

  if (candidate.state === 'running') {
    return <CandidateRunning />
  }

  if (candidate.state === 'failed') {
    return (
      <section className="flex flex-col gap-1 border-t border-border/60 pt-3" data-testid="cm-logo-candidate-failed">
        <span className="w-fit rounded bg-destructive/10 px-1.5 py-0.5 text-xs text-destructive">解析失敗</span>
        <p className="text-sm text-destructive" data-testid="cm-logo-candidate-failure-message">
          {cmDetectStageMessage(candidate.stage)}
        </p>
        {(candidate.stage === 'area' || candidate.stage === 'logo' || candidate.stage === 'match') && (
          <p className="text-xs text-muted-foreground">枠を描き直して解析し直してください。</p>
        )}
      </section>
    )
  }

  const sourceLabel = candidateRecording ? recordingLabel(candidateRecording) : '解析に使った録画'
  return (
    <section className="flex flex-col gap-3 border-t border-border/60 pt-3" data-testid="cm-logo-candidate-ready">
      <div className="flex items-center gap-2">
        <span className="w-fit rounded bg-muted px-1.5 py-0.5 text-xs">候補があります</span>
        <p className="text-sm">候補を確認してから採用してください。</p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="min-w-0">
          <h4 className="mb-1 text-xs font-medium">今のロゴ</h4>
          <NativeLogoPreview
            previewPng={logo.previewPng}
            sampleAspectRatio={sampleAspectRatio}
            alt={`${logo.serviceName} の現在のロゴ`}
            testId="cm-logo-current-preview"
          />
        </div>
        <div className="min-w-0">
          <h4 className="mb-1 text-xs font-medium">候補</h4>
          <NativeLogoPreview
            previewPng={candidate.previewPng}
            sampleAspectRatio={sampleAspectRatio}
            alt={`${logo.serviceName} のロゴ候補`}
            testId="cm-logo-candidate-preview"
          />
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        {sourceLabel}・枠 {candidate.w}×{candidate.h}（{candidate.codedWidth}×{candidate.codedHeight}）
      </p>
      <label className="flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          data-testid="cm-logo-candidate-redetect"
          checked={redetect}
          onChange={(event) => setRedetect(event.currentTarget.checked)}
          className="mt-0.5 size-4"
        />
        <span>採用前に検出した録画 {logo.detectedCount} 件（うち原本が残っている {logo.redetectableCount} 件）も検出し直す</span>
      </label>
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          data-testid="cm-logo-candidate-adopt"
          disabled={adopt.isPending || candidate.recordingId === null || candidate.recordingId === undefined}
          onClick={() => adopt.mutate(
            { networkId: logo.networkId, serviceId: logo.serviceId, data: { redetect } },
            {
              onSuccess: () => {
                setAdopted(true)
                invalidate()
                toast({ message: 'ロゴ候補を採用しました' })
              },
              onError: (error) => toast({ message: mutationErrorMessage('ロゴ候補の採用に失敗しました', error), kind: 'error' }),
            },
          )}
        >
          採用
        </Button>
        <Button
          type="button"
          variant="outline"
          data-testid="cm-logo-candidate-discard"
          disabled={discard.isPending}
          onClick={() => discard.mutate(
            { networkId: logo.networkId, serviceId: logo.serviceId },
            {
              onSuccess: () => {
                invalidate()
                toast({ message: '候補を破棄しました。同じ枠で解析し直します' })
              },
              onError: (error) => toast({ message: mutationErrorMessage('ロゴ候補の破棄に失敗しました', error), kind: 'error' }),
            },
          )}
        >
          破棄して描き直す
        </Button>
      </div>
    </section>
  )
}

function AffectedRecordings({ recordings, cmDetectEnabled }: { recordings: Recording[]; cmDetectEnabled: boolean }) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const retry = useRetryRecordingCMDetection()
  return (
    <section className="flex flex-col gap-3" aria-labelledby="cm-affected-recordings">
      <div>
        <h2 id="cm-affected-recordings" className="text-base font-semibold">影響する録画</h2>
        <p className="mt-1 text-sm text-muted-foreground">成功した録画も含め、この局で CM 検出した録画を並べています。</p>
      </div>
      {recordings.length === 0 ? (
        <p className="text-sm text-muted-foreground">録画がありません</p>
      ) : (
        <ul className="divide-y divide-border rounded-lg border border-border">
          {recordings.map((recording) => {
            const canRetry = recording.cmDetection.state === 'failed' && !isAwaitingAdoption(recording) && recording.sizeBytes !== undefined && cmDetectEnabled
            return (
              <li key={recording.id} className="flex flex-wrap items-center gap-x-3 gap-y-2 p-3">
                <div className="min-w-0 flex-1">
                  <Link className="font-medium hover:underline" to="/recordings/$id" params={{ id: String(recording.id) }}>
                    {recording.title}
                  </Link>
                  <p className="text-xs text-muted-foreground">
                    {formatDateTime(recording.startAt)} · {recordingCMState(recording)}
                  </p>
                </div>
                {canRetry && (
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={retry.isPending}
                    onClick={() => retry.mutate({ id: recording.id }, {
                      onSuccess: () => {
                        void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
                        toast({ message: 'CM 検出を再試行します' })
                      },
                      onError: (error) => toast({ message: mutationErrorMessage('CM 検出の再試行に失敗しました', error), kind: 'error' }),
                    })}
                  >
                    再試行
                  </Button>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}

export function CMLogoStationPage() {
  const { networkId: networkParam, serviceId: serviceParam } = useParams({ from: '/cm-logos/$networkId/$serviceId' })
  const search = useSearch({ from: '/cm-logos/$networkId/$serviceId' })
  const networkId = Number(networkParam)
  const serviceId = Number(serviceParam)
  const logoQuery = useListCMLogos({
    query: {
      refetchInterval: (query) => {
        const current = (unwrap(query.state.data) ?? []).find(
          (item) => item.networkId === networkId && item.serviceId === serviceId,
        )
        return current && (current.candidate?.state === 'running' || awaitingCandidateAnalysis(current)) ? 5000 : false
      },
    },
  })
  const logo = (unwrap(logoQuery.data) ?? []).find((item) => item.networkId === networkId && item.serviceId === serviceId)
  const recordingsQuery = useListRecordings({ service: [serviceKey(networkId, serviceId)], limit: 200 })
  const recordings = unwrap(recordingsQuery.data) ?? []
  const cmDetectEnabled = useCMDetectEnabled()
  const deleteLogo = useDeleteCMLogo()
  const deleteArea = useDeleteCMLogoArea()
  const queryClient = useQueryClient()
  const toast = useToast()
  const [advancedOpen, setAdvancedOpen] = useState(false)

  if (!Number.isInteger(networkId) || networkId <= 0 || !Number.isInteger(serviceId) || serviceId <= 0) {
    return <ErrorState>局の指定が正しくありません</ErrorState>
  }
  if (logoQuery.isError) return <ErrorState onRetry={() => void logoQuery.refetch()}>CM ロゴの取得に失敗しました</ErrorState>
  if (recordingsQuery.isError) return <ErrorState onRetry={() => void recordingsQuery.refetch()}>局の録画を取得できませんでした</ErrorState>
  if (logoQuery.isPending || recordingsQuery.isPending || !logo) return <ListSkeleton rows={5} />

  const badge = stateBadge(logo)
  const clearLearnedLogo = () => {
    deleteLogo.mutate({ networkId, serviceId }, {
      onSuccess: () => {
        void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
        void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
        toast({
          message: logo.logoArea
            ? '覚えたロゴを削除しました。枠から新しい候補を作ります'
            : '覚えたロゴを削除しました。次の検出で自動に学習します',
        })
      },
      onError: (error) => toast({ message: mutationErrorMessage('覚えたロゴの削除に失敗しました', error), kind: 'error' }),
    })
  }
  const clearArea = () => {
    deleteArea.mutate({ networkId, serviceId }, {
      onSuccess: () => {
        void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
        toast({ message: '枠を消して自動の探索に戻しました' })
      },
      onError: (error) => toast({ message: mutationErrorMessage('枠の削除に失敗しました', error), kind: 'error' }),
    })
  }

  return (
    <>
      <PageHeader
        title="CM 検出のロゴ"
        leading={<Button variant="ghost" size="icon" aria-label="一覧へ戻る" render={<Link to="/cm-logos" />}><ArrowLeft /></Button>}
      />
      <PageContent className="flex flex-col gap-6 px-4 py-4">
        <section className="flex flex-col gap-2" aria-labelledby="cm-station-heading">
          <div className="flex flex-wrap items-center gap-2">
            <h2 id="cm-station-heading" className="text-lg font-semibold">{logo.serviceName}</h2>
            <span className={cn('rounded px-1.5 py-0.5 text-xs', badge.attention ? 'bg-destructive/10 text-destructive' : 'bg-muted text-foreground')}>
              {badge.label}
            </span>
            <span className="text-sm text-muted-foreground">{logo.site}</span>
          </div>
          <p className="text-sm">{cmLogoStateSentence(logo)}</p>
        </section>

        {!cmDetectEnabled ? (
          <p className="text-sm text-muted-foreground">このデプロイでは CM 検出が無効なので、枠を教える面は出ません。</p>
        ) : (
          <CMLogoFrameEditor
            logo={logo}
            recordings={recordings}
            requestedRecordingId={search.recording}
          />
        )}

        <AffectedRecordings recordings={recordings} cmDetectEnabled={cmDetectEnabled} />

        <details open={advancedOpen} onToggle={(event) => setAdvancedOpen(event.currentTarget.open)} className="border-t border-border pt-4">
          <summary className="cursor-pointer font-medium">高度な操作</summary>
          <div className="mt-3 flex flex-col gap-4 text-sm">
            <LearnedLogo logo={logo} />
            <div>
              <p>覚えたロゴを捨てる</p>
              <p className="text-xs text-muted-foreground">
                {logo.logoArea ? '枠から解析し直して新しい候補を作ります。' : '次の検出で画面からロゴを探し直します。'}
              </p>
              <Button type="button" className="mt-2" size="sm" variant="destructive" disabled={!logo.learnedAt || deleteLogo.isPending} onClick={clearLearnedLogo}>
                <Trash2 data-icon="inline-start" />
                覚えたロゴを捨てる
              </Button>
            </div>
            <div>
              <p>枠を消して自動に戻す</p>
              <p className="text-xs text-muted-foreground">候補と採用待ちを取り消し、自動の学習に戻します。</p>
              <Button type="button" className="mt-2" size="sm" variant="outline" disabled={!logo.logoArea || deleteArea.isPending} onClick={clearArea}>枠を消して自動に戻す</Button>
            </div>
          </div>
        </details>
      </PageContent>
    </>
  )
}

function LogoRow({ logo }: { logo: CMLogoState }) {
  const badge = stateBadge(logo)
  return (
    <li className="rounded-lg border border-border bg-card p-3">
      <Link
        className="flex min-w-0 items-center gap-3 rounded outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        to="/cm-logos/$networkId/$serviceId"
        params={{ networkId: String(logo.networkId), serviceId: String(logo.serviceId) }}
      >
        <div className="flex h-12 w-24 shrink-0 items-center justify-center overflow-hidden rounded border border-border bg-muted">
          {logo.previewPng ? <img src={`data:image/png;base64,${logo.previewPng}`} alt="" className="max-h-full max-w-full object-contain" /> : <ScanLine aria-hidden="true" className="size-5 text-muted-foreground" />}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-medium">{logo.serviceName}</span>
            <span className="text-xs text-muted-foreground">{logo.site}</span>
            {(badge.attention || logo.candidate?.state === 'ready') && (
              <span className={cn('rounded px-1.5 py-0.5 text-xs', badge.attention ? 'bg-destructive/10 text-destructive' : 'bg-muted text-foreground')}>
                {badge.label}
              </span>
            )}
          </div>
          <p className="mt-1 text-sm">{cmLogoStateSentence(logo)}</p>
        </div>
        <span className="shrink-0 text-sm font-medium">直す →</span>
      </Link>
    </li>
  )
}

function LogoSection({
  title,
  logos,
  testId,
  collapsed = false,
}: {
  title: string
  logos: CMLogoState[]
  testId: string
  collapsed?: boolean
}) {
  if (logos.length === 0) return null
  const content = (
    <ul className="flex flex-col gap-2" data-testid={testId}>
      {logos.map((logo) => <LogoRow key={`${logo.networkId}-${logo.serviceId}`} logo={logo} />)}
    </ul>
  )
  if (collapsed) {
    return <details><summary className="cursor-pointer text-base font-semibold">{title}（{logos.length}）</summary><div className="mt-2">{content}</div></details>
  }
  return <section aria-labelledby={`${testId}-heading`}><h2 id={`${testId}-heading`} className="mb-2 text-base font-semibold">{title}</h2>{content}</section>
}

export function CMLogosPage() {
  const search = useSearch({ from: '/cm-logos' })
  const navigate = useNavigate()
  const legacy = search.network !== undefined && search.service !== undefined
  const query = useListCMLogos({ query: { enabled: !legacy } })
  const logos = unwrap(query.data) ?? []
  const cmDetectEnabled = useCMDetectEnabled()

  useEffect(() => {
    if (!legacy) return
    void navigate({
      to: '/cm-logos/$networkId/$serviceId',
      params: { networkId: String(search.network), serviceId: String(search.service) },
      search: search.recording === undefined ? {} : { recording: search.recording },
      replace: true,
    })
  }, [legacy, navigate, search.network, search.recording, search.service])

  if (legacy) return <ListSkeleton rows={3} />

  const attention = logos.filter((logo) => cmLogoBucket(logo) === 'attention')
  const pending = logos.filter((logo) => cmLogoBucket(logo) === 'pending')
  const healthy = logos.filter((logo) => cmLogoBucket(logo) === 'healthy')

  return (
    <>
      <PageHeader title="CM 検出のロゴ" />
      <PageContent className="flex flex-col gap-6 px-4 py-4">
        <p className="text-sm text-muted-foreground">失敗や検出待ちの局から確認し、局ごとのコマで CM ロゴの枠を教えます。</p>
        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()}>CM ロゴの取得に失敗しました</ErrorState>
        ) : query.isPending ? (
          <ListSkeleton />
        ) : logos.length === 0 ? (
          <EmptyState>録画局がありません</EmptyState>
        ) : (
          <div className="flex flex-col gap-6">
            <LogoSection title="要対応" logos={attention} testId="cm-logo-attention" />
            <LogoSection title="検出待ち" logos={pending} testId="cm-logo-pending" />
            <LogoSection title="問題なし" logos={healthy} testId="cm-logo-healthy" collapsed />
          </div>
        )}
        {!cmDetectEnabled && <p className="text-sm text-muted-foreground">このデプロイでは CM 検出が無効なので、枠を教える面は出ません。</p>}
      </PageContent>
    </>
  )
}
