import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useSearch } from '@tanstack/react-router'
import { ScanLine, Trash2 } from 'lucide-react'

import {
  getListCMLogosQueryKey,
  useDeleteCMLogo,
  useDeleteCMLogoArea,
  useListCMLogos,
  usePutCMLogoArea,
  type CMLogoState,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, EmptyState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { useCMDetectEnabled } from '@/lib/capabilities'
import {
  clampCodedRect,
  codedRectFromPoints,
  codedToFrame,
  containsCodedPoint,
  FRAME_ZOOM,
  frameImageBox,
  frameScale,
  frameToCoded,
  MIN_AREA_SIZE,
  moveCodedRect,
  savedAreaMatchesFrame,
  type CodedRect,
} from '@/lib/cm-logo-frame'
import { formatDateTime } from '@/lib/format'
import { cmDetectStageMessage } from '@/lib/cm-detect-stage'
import { mutationErrorMessage } from '@/lib/mutation-error-message'
import {
  SEEK_TILES_COLUMNS,
  SEEK_TILES_HEIGHT,
  SEEK_TILES_INTERVAL_SECONDS,
  seekTilesURL,
} from '@/lib/seek-tiles'

/** frameURL は streamer のコマ切り出し URL を組み立てる（OpenAPI 外）。 */
// oxlint-disable-next-line react/only-export-components -- テスト可能な URL 組み立て関数をページ契約と同じ場所に置く
export function frameURL(recordingId: number, atMs: number): string {
  return `/api/media/recordings/${recordingId}/frame?at=${atMs}`
}

function stateLabel(state: CMLogoState['state']): string {
  switch (state) {
    case 'learned':
      return '学習済み'
    case 'failed':
      return '失敗あり'
    default:
      return '未学習'
  }
}

/** 1 本以上の録画がある局だけを並べる（サーバーもその条件で返す）。 */
function stationKey(logo: CMLogoState): string {
  return `${logo.networkId}-${logo.serviceId}`
}

/** formatPosition はコマの位置を m:ss で出す。 */
function formatPosition(atMs: number): string {
  const total = Math.floor(atMs / 1000)
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  return `${minutes}:${String(seconds).padStart(2, '0')}`
}

type Frame = { url: string; codedWidth: number; codedHeight: number }

/**
 * useFrame は指定位置の原寸のコマを取り寄せる。
 *
 * **記録上の大きさは応答ヘッダから読む**（`X-Coded-Width` / `X-Coded-Height`）。
 * 画像の画素数でも同じ値になるが、サーバーが返す値だけが「検出側が枠を当てる
 * 解像度」と一致する保証を持つ。
 */
function useFrame(recordingId: number, atMs: number | null): { frame: Frame | null; failed: boolean } {
  const [frame, setFrame] = useState<Frame | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    if (recordingId <= 0 || atMs === null) {
      // 外部入力（選択中のコマ）の変更に合わせて取得状態を同期する。
      // oxlint-disable-next-line react/set-state-in-effect -- コマ選択時に前の画像を破棄する
      setFrame(null)
      setFailed(false)
      return
    }
    let cancelled = false
    let objectURL: string | null = null
    setFailed(false)
    fetch(frameURL(recordingId, atMs))
      .then(async (response) => {
        if (!response.ok) throw new Error(`status ${response.status}`)
        const blob = await response.blob()
        const codedWidth = Number(response.headers.get('X-Coded-Width'))
        const codedHeight = Number(response.headers.get('X-Coded-Height'))
        if (cancelled || codedWidth <= 0 || codedHeight <= 0) throw new Error('missing coded size')
        objectURL = URL.createObjectURL(blob)
        setFrame({ url: objectURL, codedWidth, codedHeight })
      })
      .catch(() => {
        if (!cancelled) {
          setFrame(null)
          setFailed(true)
        }
      })
    return () => {
      cancelled = true
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [recordingId, atMs])

  return { frame, failed }
}

/** useBoxSize は表示枠の実寸（CSS px）を返す。jsdom では 0 のまま（判定は e2e）。 */
function useBoxSize(ref: React.RefObject<HTMLElement | null>): { width: number; height: number } {
  const [size, setSize] = useState({ width: 0, height: 0 })
  useEffect(() => {
    const element = ref.current
    if (!element) return
    const publish = () =>
      setSize({ width: element.clientWidth, height: element.clientHeight })
    publish()
    const observer = new ResizeObserver(publish)
    observer.observe(element)
    return () => observer.disconnect()
  }, [ref])
  return size
}

/**
 * LogoTutor は 1 局の枠を教える面。シークタイル帯で場面を選び、原寸のコマの上で
 * 枠を描く・動かす。
 *
 * **帯はポインタ操作の補助として `aria-hidden` にする。** キーボードの経路は
 * 「前のコマ / 次のコマ」のボタンが持つ（プレイヤーのシーク帯と同じ分担）。
 */
function LogoTutor({
  logo,
  recordingId,
}: {
  logo: CMLogoState
  /** ディープリンクで指定された録画。0 なら原本のある最新の録画を使う。 */
  recordingId: number
}) {
  const frameRecordingId = recordingId > 0 ? recordingId : logo.frameRecordingId
  const [atMs, setAtMs] = useState<number | null>(null)
  const [zoom, setZoom] = useState(FRAME_ZOOM)
  const [rect, setRect] = useState<CodedRect | null>(null)
  const boxRef = useRef<HTMLDivElement>(null)
  const dragRef = useRef<{ mode: 'draw' | 'move'; start: { x: number; y: number }; origin: CodedRect } | null>(
    null,
  )
  const box = useBoxSize(boxRef)
  const { frame, failed } = useFrame(frameRecordingId, atMs)
  const queryClient = useQueryClient()
  const toast = useToast()
  const saveArea = usePutCMLogoArea()
  const clearArea = useDeleteCMLogoArea()

  const view = {
    codedWidth: frame?.codedWidth ?? 0,
    codedHeight: frame?.codedHeight ?? 0,
    boxWidth: box.width,
    boxHeight: box.height,
    zoom,
  }
  const area = logo.logoArea
  const areaFitsFrame = savedAreaMatchesFrame(area, view)

  // コマが変わったら、保存済みの枠がその解像度に合うときだけ書き戻す。
  // **同じ大きさのコマを取り直したときは触らない**（一覧の再取得で編集中の枠を
  // 消さないため）。
  const appliedFrameKey = useRef<string | null>(null)
  useEffect(() => {
    const key = frame
      ? `${frameRecordingId}:${frame.codedWidth}x${frame.codedHeight}`
      : `${frameRecordingId}:none`
    if (appliedFrameKey.current === key) return
    appliedFrameKey.current = key
    const fits =
      frame !== null &&
      savedAreaMatchesFrame(area, { codedWidth: frame.codedWidth, codedHeight: frame.codedHeight })
    setRect(fits ? { x: area!.x, y: area!.y, w: area!.w, h: area!.h } : null)
  }, [frame, area, frameRecordingId])

  const stepTo = (index: number) => {
    if (index < 0) return
    setAtMs((index * SEEK_TILES_INTERVAL_SECONDS + SEEK_TILES_INTERVAL_SECONDS / 2) * 1000)
  }

  /** 帯の押した位置 → タイルの番号。格子は画像の実際の大きさから出す。 */
  const tileFromEvent = (event: React.MouseEvent<HTMLImageElement>): number | null => {
    const image = event.currentTarget
    if (image.naturalWidth <= 0 || image.naturalHeight <= 0) return null
    const bounds = image.getBoundingClientRect()
    if (bounds.width <= 0 || bounds.height <= 0) return null
    const column = Math.floor(((event.clientX - bounds.left) / bounds.width) * SEEK_TILES_COLUMNS)
    const row = Math.floor(
      ((event.clientY - bounds.top) / bounds.height) * (image.naturalHeight / SEEK_TILES_HEIGHT),
    )
    if (row < 0) return null
    return row * SEEK_TILES_COLUMNS + Math.min(Math.max(column, 0), SEEK_TILES_COLUMNS - 1)
  }

  const codedPoint = (event: React.PointerEvent<HTMLDivElement>) => {
    const element = boxRef.current
    if (!element || !frame) return null
    const bounds = element.getBoundingClientRect()
    return frameToCoded({ x: event.clientX - bounds.left, y: event.clientY - bounds.top }, view)
  }

  const imageBox = frameImageBox(view)
  const scale = frameScale(view)
  const rectOnScreen = rect && scale > 0 ? codedToFrame({ x: rect.x, y: rect.y }, view) : null
  const canSave = rect !== null && rect.w >= MIN_AREA_SIZE && rect.h >= MIN_AREA_SIZE && !saveArea.isPending

  const onSave = () => {
    if (!rect || !frame) return
    const clamped = clampCodedRect(rect, frame.codedWidth, frame.codedHeight)
    saveArea.mutate(
      {
        networkId: logo.networkId,
        serviceId: logo.serviceId,
        data: {
          x: clamped.x,
          y: clamped.y,
          w: clamped.w,
          h: clamped.h,
          codedWidth: frame.codedWidth,
          codedHeight: frame.codedHeight,
        },
      },
      {
        onSuccess: () => {
          void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
          toast({
            message: `${logo.serviceName} の枠を保存しました。学習済みのロゴを捨て、次の検出でこの枠から学習します。`,
          })
        },
        onError: (error) =>
          toast({ message: mutationErrorMessage('枠の保存に失敗しました', error), kind: 'error' }),
      },
    )
  }

  const onClear = () => {
    clearArea.mutate(
      { networkId: logo.networkId, serviceId: logo.serviceId },
      {
        onSuccess: () => {
          setRect(null)
          void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
          toast({ message: `${logo.serviceName} の枠を消しました。自動の探索に戻ります。` })
        },
        onError: (error) =>
          toast({ message: mutationErrorMessage('枠の削除に失敗しました', error), kind: 'error' }),
      },
    )
  }

  if (frameRecordingId <= 0) {
    return (
      <p className="border-t border-border/60 pt-3 text-muted-foreground" data-testid="cm-logo-no-original">
        原本のある録画がありません。原本を消した録画からはコマを取り寄せられません。
      </p>
    )
  }

  return (
    <section className="flex flex-col gap-3 border-t border-border/60 pt-3" aria-label="ロゴの枠">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_16rem]">
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              size="sm"
              variant="ghost"
              disabled={atMs === null || atMs <= 0}
              onClick={() => setAtMs(Math.max((atMs ?? 0) - SEEK_TILES_INTERVAL_SECONDS * 1000, 0))}
            >
              前のコマ
            </Button>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() =>
                setAtMs(
                  atMs === null
                    ? (SEEK_TILES_INTERVAL_SECONDS / 2) * 1000
                    : atMs + SEEK_TILES_INTERVAL_SECONDS * 1000,
                )
              }
            >
              次のコマ
            </Button>
            <span className="text-xs text-muted-foreground" data-testid="cm-logo-position">
              {atMs === null ? '場面を選んでください' : `位置 ${formatPosition(atMs)}`}
            </span>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={!frame}
              onClick={() => setZoom(zoom === FRAME_ZOOM ? 1 : FRAME_ZOOM)}
            >
              {zoom === FRAME_ZOOM ? '全体表示' : '右上を拡大'}
            </Button>
          </div>

          {/*
            コマの表示枠。**座標はこの枠が自分で持つ**（poster やタイルは SAR を
            焼き込んでいるので重ねられない）。表示枠の寸法は実測でしか取れないため、
            配線の合否は web/e2e/cm-logo-area.mjs が見る。
          */}
          <div
            ref={boxRef}
            data-testid="cm-logo-frame"
            className="relative aspect-video touch-none overflow-hidden rounded border border-border bg-muted"
            onPointerDown={(event) => {
              const point = codedPoint(event)
              if (!point || !frame) return
              if (rect && containsCodedPoint(rect, point)) {
                dragRef.current = { mode: 'move', start: point, origin: rect }
              } else {
                dragRef.current = { mode: 'draw', start: point, origin: rect ?? { x: 0, y: 0, w: 1, h: 1 } }
                setRect({ x: point.x, y: point.y, w: 0, h: 0 })
              }
              event.currentTarget.setPointerCapture(event.pointerId)
            }}
            onPointerMove={(event) => {
              const drag = dragRef.current
              const point = codedPoint(event)
              if (!drag || !point || !frame) return
              setRect(
                drag.mode === 'draw'
                  ? codedRectFromPoints(drag.start, point)
                  : moveCodedRect(drag.origin, point.x - drag.start.x, point.y - drag.start.y, frame.codedWidth, frame.codedHeight),
              )
            }}
            onPointerUp={(event) => {
              const drag = dragRef.current
              dragRef.current = null
              event.currentTarget.releasePointerCapture(event.pointerId)
              // 動かさずに押しただけの点は枠にしない。
              if (drag?.mode === 'draw' && rect && (rect.w < MIN_AREA_SIZE || rect.h < MIN_AREA_SIZE)) {
                setRect(null)
              }
            }}
          >
            {frame && (
              <img
                src={frame.url}
                alt={`${logo.serviceName} の ${atMs === null ? '' : formatPosition(atMs)} のコマ`}
                draggable={false}
                data-testid="cm-logo-frame-image"
                className="pointer-events-none absolute top-0 right-0 select-none"
                style={{ width: imageBox.width, height: imageBox.height }}
              />
            )}
            {rect && rectOnScreen && (
              <div
                data-testid="cm-logo-rect"
                className="pointer-events-none absolute border-2 border-primary"
                style={{ left: rectOnScreen.x, top: rectOnScreen.y, width: rect.w * scale, height: rect.h * scale }}
              />
            )}
          </div>

          {failed && (
            <p className="text-destructive" role="alert">
              この位置のコマを取り寄せできませんでした。別の場面を選んでください。
            </p>
          )}

          {/* 場面を選ぶ帯。ポインタ操作の補助（キーボードは前/次のコマ）なので aria-hidden。 */}
          <div aria-hidden="true" data-testid="cm-logo-tiles" className="w-full cursor-crosshair">
            <img
              src={seekTilesURL(frameRecordingId)}
              alt=""
              className="w-full rounded border border-border"
              onClick={(event) => {
                const index = tileFromEvent(event)
                if (index !== null) stepTo(index)
              }}
            />
          </div>
          <p className="text-xs text-muted-foreground">
            タイルを押すとその場面の原寸のコマを取り寄せます。コマの上でドラッグすると枠を描き、
            枠の中をドラッグすると動かせます。
          </p>
        </div>

        <aside className="flex flex-col gap-2 text-sm">
          <div>
            <h4 className="mb-1 font-medium">覚えたロゴ</h4>
            <div className="flex size-16 items-center justify-center overflow-hidden rounded border border-border bg-muted">
              {logo.previewPng ? (
                <img
                  src={`data:image/png;base64,${logo.previewPng}`}
                  alt={`${logo.serviceName} のロゴ`}
                  className="max-h-full max-w-full object-contain"
                />
              ) : (
                <ScanLine aria-hidden="true" className="size-6 text-muted-foreground" />
              )}
            </div>
            <p className="mt-1 text-muted-foreground">
              {logo.learnedAt ? `学習 ${formatDateTime(logo.learnedAt)}` : '学習していません'}
            </p>
            <p className="text-muted-foreground">
              {area ? `教えた枠 ${area.w}×${area.h}（${area.codedWidth}×${area.codedHeight}）` : '枠は自動の探索'}
            </p>
          </div>

          {logo.failedCount > 0 && (
            <div>
              <h4 className="mb-1 font-medium">直近の失敗理由</h4>
              <p className="break-all text-destructive" data-testid="cm-logo-failure-message">
                {cmDetectStageMessage(logo.lastFailureStage)}
              </p>
            </div>
          )}

          {area && frame && !areaFitsFrame && (
            <p className="text-destructive" role="alert" data-testid="cm-logo-area-mismatch">
              保存済みの枠は {area.codedWidth}×{area.codedHeight} 用です。このコマは{' '}
              {frame.codedWidth}×{frame.codedHeight} なので、枠はこの録画には使われません。
            </p>
          )}

          <div className="flex flex-wrap gap-2">
            <Button type="button" size="sm" disabled={!canSave} onClick={onSave}>
              枠を保存
            </Button>
            <Button
              type="button"
              size="sm"
              variant="secondary"
              disabled={!area || clearArea.isPending}
              onClick={onClear}
            >
              自動に戻す
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            保存すると、その局の覚えたロゴを捨てて次の検出で枠の中を学習し直します。
          </p>
        </aside>
      </div>
    </section>
  )
}

function LogoRow({
  logo,
  open,
  recordingId,
  onToggle,
}: {
  logo: CMLogoState
  open: boolean
  recordingId: number
  onToggle: () => void
}) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const remove = useDeleteCMLogo()

  return (
    <li className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4" data-testid="cm-logo-row">
      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          aria-expanded={open}
          onClick={onToggle}
          className="min-w-0 flex-1 text-left"
          data-testid="cm-logo-toggle"
        >
          <span className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
            <span className="font-medium">{logo.serviceName}</span>
            <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
              {stateLabel(logo.state)}
            </span>
            <span className="text-xs text-muted-foreground">
              Network {logo.networkId} / Service {logo.serviceId}
            </span>
          </span>
          <span className="mt-1 block text-sm text-muted-foreground">
            録画 {logo.recordingCount} 件
            {logo.failedCount > 0 && ` · 検出失敗 ${logo.failedCount} 件`}
            {logo.learnedAt && ` · 学習 ${formatDateTime(logo.learnedAt)}`}
          </span>
        </button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={remove.isPending || !logo.learnedAt}
          onClick={() => {
            remove.mutate(
              { networkId: logo.networkId, serviceId: logo.serviceId },
              {
                onSuccess: () => {
                  void queryClient.invalidateQueries({ queryKey: getListCMLogosQueryKey() })
                  toast({
                    message: `${logo.serviceName} のロゴを削除しました。次の検出で再学習します。`,
                  })
                },
                onError: (error) =>
                  toast({
                    message: mutationErrorMessage('ロゴの削除に失敗しました', error),
                    kind: 'error',
                  }),
              },
            )
          }}
        >
          <Trash2 data-icon="inline-start" />
          ロゴを削除
        </Button>
      </div>
      {/* 警告は閉じたままでも見える（枠と解像度が違う録画はここに出る）。 */}
      {logo.failedCount > 0 && (
        <p className="break-all text-destructive" data-testid="cm-logo-warning">
          {cmDetectStageMessage(logo.lastFailureStage)}
        </p>
      )}
      {open && <LogoTutor logo={logo} recordingId={recordingId} />}
    </li>
  )
}

/**
 * CMLogosPage は局ごとの CM ロゴと、自動学習で埋まらない局の枠を教える画面。
 *
 * **番組表の局ロゴとは別物である**（あちらはサービス一覧、こちらは CM 検出器が
 * 映像から探すロゴ）。
 */
export function CMLogosPage() {
  const query = useListCMLogos()
  const logos = unwrap(query.data) ?? []
  const cmDetectEnabled = useCMDetectEnabled()
  const search = useSearch({ from: '/cm-logos' })
  const [openKey, setOpenKey] = useState<string | null>(null)

  // ディープリンク（録画詳細の「CM 検出に失敗」）はその局を開いた状態で来る。
  const deepLinkKey =
    search.network !== undefined && search.service !== undefined
      ? `${search.network}-${search.service}`
      : null
  const deepLinkKeyExists = deepLinkKey !== null && logos.some((logo) => stationKey(logo) === deepLinkKey)
  const effectiveOpenKey = openKey ?? (deepLinkKeyExists ? deepLinkKey : null)

  return (
    <>
      <PageHeader title="CM 検出のロゴ" />
      <PageContent className="flex flex-col gap-4 px-4 py-4">
        <p className="text-sm text-muted-foreground">
          自動の学習は画面の隅に同じ縁が続くことを手がかりにするので、薄いロゴや動くロゴの局では
          見つけられません。そうした局だけ、映像のコマの上で枠を教えます。枠は局ごとに共有されます。
        </p>
        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()}>CM ロゴの取得に失敗しました</ErrorState>
        ) : query.isPending ? (
          <ListSkeleton />
        ) : logos.length === 0 ? (
          <EmptyState>録画局がありません</EmptyState>
        ) : (
          <ul className="flex flex-col gap-3">
            {logos.map((logo) => {
              const key = stationKey(logo)
              return (
                <LogoRow
                  key={key}
                  logo={logo}
                  open={cmDetectEnabled && effectiveOpenKey === key}
                  recordingId={
                    search.recording !== undefined && key === deepLinkKey ? search.recording : 0
                  }
                  onToggle={() => setOpenKey(effectiveOpenKey === key ? null : key)}
                />
              )
            })}
          </ul>
        )}
        {!cmDetectEnabled && (
          <p className="text-sm text-muted-foreground">
            このデプロイでは CM 検出が無効なので、枠を教える面は出ません。
          </p>
        )}
      </PageContent>
    </>
  )
}
