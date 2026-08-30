import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import type { ProgramListItem, Service } from '@/api/generated'
import {
  axisHeightPx,
  epgColumnWidthPx,
  groupProgramsByService,
  hourTicks,
  spanToPx,
  timeToPx,
  visibleColumnRange,
  visibleTimeWindow,
  type PlacedProgram,
  type TimeAxis,
} from '@/lib/epg-grid'
import { formatTime } from '@/lib/format'
import { genreLabel, genreTint } from '@/lib/genre'
import { composeServiceId } from '@/lib/service-id'
import { cn } from '@/lib/utils'

/** 時間軸（左端の目盛り列）の幅。 */
const gutterWidthPx = 56

/** サービス名を出すヘッダ行の高さ。 */
const headerHeightPx = 36

/**
 * 画面外にも描いておく高さ。スクロール中に空白が見えないための余白で、
 * 大きくすると DOM 数が増える。
 */
const overscanPx = 400

/** 画面外にも描いておく列数（左右それぞれ）。 */
const overscanColumns = 1

/** 現在時刻インジケータを動かす間隔。1 分未満のずれは 2px 未満なので 30 秒で足りる。 */
const clockIntervalMs = 30_000

type Viewport = {
  top: number
  left: number
  width: number
  height: number
}

/** 未計測の viewport。幅・高さが 0 のとき仮想化は「全部描く」に倒れる（lib/epg-grid.ts）。 */
const unmeasuredViewport: Viewport = { top: 0, left: 0, width: 0, height: 0 }

/**
 * ProgramGrid はサービス x 時間の 2 次元番組表。
 *
 * このコンポーネントの責務は座標の写像（lib/epg-grid.ts）を DOM に置くことだけで、
 * どの番組・どのサービスを渡すかは呼び出し側が決める。
 *
 * ## 仮想化
 *
 * ライブラリを入れずに 2 軸とも自前で間引く。理由は縦軸がリストではないこと:
 * 時間軸は連続量で、番組セルは複数の目盛りをまたぐ「区間」なので、行の並びを
 * 前提とする仮想化ライブラリの模型に乗らない。可視判定は
 * `visibleTimeWindow` / `visibleColumnRange` という純関数 2 つに落ちるので、
 * ライブラリを入れるより検証しやすい。
 *
 * ## ヘッダの固定
 *
 * スクロール容器は 1 つだけにして、サービス名の行と時間軸の列を `position: sticky`
 * で固定する。スクロール位置を JS で同期させる実装（容器を 3 つに分ける方式）に
 * すると、慣性スクロール中にヘッダが遅れる経路を自前で持つことになる。
 *
 * ## 帯を重ねる余地
 *
 * M2-10 の容量超過は「番組ではなく区間」に描く（docs/frontend.md）。全チャンネルを
 * 縦断する帯を置くための層を `overlay` として開けてあり、そこに渡される軸で
 * `spanToPx` を呼べば番組セルと同じ座標系に乗る。現在時刻インジケータが
 * その最初の利用者。
 *
 * 帯の**見えるラベル**だけは別の層 `gutterOverlay` に置く。時間軸列（左端の
 * `21:00` 等が並ぶ列）は局の列とは別の DOM 部分木なので、帯の見えるラベルを
 * ここに出せば番組セルの時刻文字と重なりようがない（issue #460。局の列の中に
 * 出すと帯の上端が番組セルの上端と重なったときに衝突する）。
 */
export function ProgramGrid({
  services,
  programs,
  axis,
  reservationByProgramId,
  selectedProgramId,
  onSelect,
  now,
  scrollToMs,
  overlay,
  gutterOverlay,
}: {
  /** 列。渡された順に左から並べる（並び順は lib/epg-grid.ts の orderServices）。 */
  services: Service[]
  programs: ProgramListItem[]
  axis: TimeAxis
  reservationByProgramId: Set<number>
  selectedProgramId: number | null
  onSelect: (program: ProgramListItem) => void
  /** 現在時刻。省略すると内部の時計を使う（テストから固定するための口）。 */
  now?: number
  /**
   * 軸が変わったときの初期スクロール先（epoch ms）。省略すると「今」に
   * スクロールする既定の挙動（下記 useLayoutEffect）のまま。容量不足バッジ
   * （`components/capacity-shortfall-badge.tsx`）から「この時間帯」への導線
   * （issue #233 M6-5）が、軸の外の時刻を渡すこともある --- その場合は「今」と
   * 同じフォールバック（先頭。`inAxis` が false になる分岐）に落ちる。
   */
  scrollToMs?: number
  /** 全チャンネル縦断の帯を重ねる層。軸を受け取って絶対配置の要素を返す。 */
  overlay?: (axis: TimeAxis) => React.ReactNode
  /**
   * 時間軸列（gutter）に重ねる層。帯の見えるラベルなど、局の列の番組セルと
   * 衝突させたくない要素をここに置く（issue #460）。軸を受け取って絶対配置の
   * 要素を返す点は `overlay` と同じ --- 座標系（`spanToPx` の top）も共有する。
   */
  gutterOverlay?: (axis: TimeAxis) => React.ReactNode
}) {
  const scrollerRef = useRef<HTMLDivElement>(null)
  const [viewport, setViewport] = useState<Viewport>(unmeasuredViewport)
  const clock = useNow(clockIntervalMs)
  const currentMs = now ?? clock

  const measure = useCallback(() => {
    const el = scrollerRef.current
    if (!el) return
    setViewport((prev) =>
      prev.top === el.scrollTop &&
      prev.left === el.scrollLeft &&
      prev.width === el.clientWidth &&
      prev.height === el.clientHeight
        ? prev
        : {
            top: el.scrollTop,
            left: el.scrollLeft,
            width: el.clientWidth,
            height: el.clientHeight,
          },
    )
  }, [])

  useLayoutEffect(measure, [measure])

  useEffect(() => {
    const el = scrollerRef.current
    if (!el) return
    const observer = new ResizeObserver(measure)
    observer.observe(el)
    return () => observer.disconnect()
  }, [measure])

  // 開いた直後は「今」（または `scrollToMs` が指す時刻）が見えている方が
  // 有用なので、そこまでスクロールしておく。軸が変わったとき（日付を変えた
  // とき）だけやり直す — 時計の更新で毎分スクロール位置が戻ると操作できない。
  const scrolledForAxisRef = useRef<number | null>(null)
  useLayoutEffect(() => {
    const el = scrollerRef.current
    if (!el || scrolledForAxisRef.current === axis.startMs) return
    scrolledForAxisRef.current = axis.startMs
    const targetMs = scrollToMs ?? currentMs
    const inAxis = targetMs >= axis.startMs && targetMs < axis.endMs
    // 少し上に余白を残す（対象が画面の最上端に張り付くと直前の番組が見えない）
    el.scrollTop = inAxis ? Math.max(0, timeToPx(axis, targetMs) - axis.pxPerHour / 2) : 0
    measure()
  }, [axis, currentMs, scrollToMs, measure])

  const placedByService = useMemo(() => groupProgramsByService(programs), [programs])
  const ticks = useMemo(() => hourTicks(axis), [axis])

  const totalHeightPx = axisHeightPx(axis)
  const columnsWidthPx = services.length * epgColumnWidthPx
  const columnRange = visibleColumnRange(
    services.length,
    epgColumnWidthPx,
    viewport.left,
    viewport.width,
    overscanColumns,
  )
  const timeWindow = visibleTimeWindow(axis, viewport.top, viewport.height, overscanPx)
  const visibleServices = services.slice(columnRange.start, columnRange.end)
  const nowTopPx =
    currentMs >= axis.startMs && currentMs < axis.endMs ? timeToPx(axis, currentMs) : null

  return (
    <div
      ref={scrollerRef}
      onScroll={measure}
      data-testid="program-grid"
      // キーボードだけでもスクロールできるように領域として focus 可能にする。
      // 絶対配置のセルに role="grid"/"gridcell" を被せると行の構造を偽ることに
      // なるので、名前付きの領域に留める。
      tabIndex={0}
      role="region"
      aria-label="番組表"
      // 高さは親から与える（h-full）。内側だけがスクロールする箱にしないと
      // ページ全体が 2880px 伸びて sticky なヘッダが効かない。高さの予算を
      // 持つのは呼び出し側（選択中の番組を上に挟むかどうかを知っているのはそちら）。
      className="relative h-full overflow-auto overscroll-contain border-t border-border"
    >
      <div className="w-max">
        <div
          className="sticky top-0 z-30 flex bg-background/95 backdrop-blur"
          style={{ height: headerHeightPx }}
        >
          {/* 左上の角。縦横どちらにも固定する */}
          <div
            className="sticky left-0 z-10 shrink-0 border-r border-b border-border bg-background/95"
            style={{ width: gutterWidthPx }}
          />
          <div
            className="relative shrink-0 border-b border-border"
            style={{ width: columnsWidthPx, height: headerHeightPx }}
          >
            {visibleServices.map((service, index) => (
              <div
                key={`${service.networkId}-${service.serviceId}`}
                className="absolute top-0 flex h-full items-center gap-1.5 overflow-hidden border-r border-border px-2"
                style={{
                  left: (columnRange.start + index) * epgColumnWidthPx,
                  width: epgColumnWidthPx,
                }}
              >
                {service.channelType === 'GR' && service.remoteControlKeyId > 0 && (
                  /* 文字色は text-foreground（bg-muted 小バッジの合成後コントラスト
                     対策。docs/frontend/design.md「コントラストは毎回測る」）。 */
                  <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-foreground">
                    {service.remoteControlKeyId}
                  </span>
                )}
                <span className="truncate text-xs font-medium">{service.name}</span>
              </div>
            ))}
          </div>
        </div>

        <div className="flex">
          <div
            className="sticky left-0 z-20 shrink-0 border-r border-border bg-background/95"
            style={{ width: gutterWidthPx, height: totalHeightPx }}
          >
            <div className="relative h-full">
              {ticks.map((tick) => (
                <div
                  key={tick.ms}
                  // 目盛り線の下に置く（19:00 のラベルは 19 時台の帯を指す）。
                  // 線の中央に載せると、先頭の目盛りが sticky なヘッダに隠れる
                  className="absolute inset-x-0 pt-0.5 pr-1.5 text-right text-[11px] text-muted-foreground"
                  style={{ top: tick.topPx }}
                >
                  {formatTime(new Date(tick.ms).toISOString())}
                </div>
              ))}
              {nowTopPx !== null && (
                <div
                  data-testid="program-grid-now-label"
                  className="absolute inset-x-0 -translate-y-1/2 pr-1.5 text-right"
                  style={{ top: nowTopPx }}
                >
                  {/* 「いま」はタリーレッドだが、**塗りにする**（文字色にしない）。
                      11px の赤い文字はダークの地に対して 4.5 に届かない ---
                      タリーを塗りに限る規律の実体
                      （docs/frontend/design.md「タリーは塗り、destructive は文字」） */}
                  <span className="rounded-sm bg-tally px-1 py-px text-[11px] font-medium text-tally-foreground">
                    {formatTime(new Date(currentMs).toISOString())}
                  </span>
                </div>
              )}
              {/* 帯の見えるラベル層（issue #460）。局の列の overlay と対になる
                  時間軸列側の層 --- 番組セルが無い列なので、帯の上端が目盛りの
                  行と重なっても番組の文字と衝突しない。不透明な地を持たせて
                  いるので、目盛り（21:00 等）と縦位置が重なってもラベルが上に
                  乗って隠す（「現在時刻の札」が目盛りに乗るのと同じ扱い）。 */}
              {gutterOverlay?.(axis)}
            </div>
          </div>

          <div
            className="relative shrink-0"
            style={{ width: columnsWidthPx, height: totalHeightPx }}
          >
            {ticks.map((tick) => (
              <div
                key={tick.ms}
                className="pointer-events-none absolute inset-x-0 border-t border-border/60"
                style={{ top: tick.topPx }}
              />
            ))}

            {visibleServices.map((service, index) => (
              <ServiceColumn
                key={`${service.networkId}-${service.serviceId}`}
                service={service}
                leftPx={(columnRange.start + index) * epgColumnWidthPx}
                placed={placedByService.get(composeServiceId(service.networkId, service.serviceId)) ?? []}
                axis={axis}
                timeWindow={timeWindow}
                reservationByProgramId={reservationByProgramId}
                selectedProgramId={selectedProgramId}
                onSelect={onSelect}
              />
            ))}

            {/* 全チャンネル縦断の層。区間（帯・現在時刻）はここに置く。
                番組セルより上、ヘッダより下（docs/frontend.md「容量超過は番組ではなく区間に描く」） */}
            <div className="pointer-events-none absolute inset-0 z-[15]">
              {nowTopPx !== null && (
                <div
                  data-testid="program-grid-now-line"
                  // 現在時刻の線は ON AIR の指標なのでタリーレッド。地が無彩なので
                  // 罫線と番組セルの境界に紛れない（docs/frontend/design.md）
                  className="absolute inset-x-0 border-t-2 border-tally"
                  style={{ top: nowTopPx }}
                />
              )}
              {overlay?.(axis)}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function ServiceColumn({
  service,
  leftPx,
  placed,
  axis,
  timeWindow,
  reservationByProgramId,
  selectedProgramId,
  onSelect,
}: {
  service: Service
  leftPx: number
  placed: PlacedProgram<ProgramListItem>[]
  axis: TimeAxis
  timeWindow: { startMs: number; endMs: number }
  reservationByProgramId: Set<number>
  selectedProgramId: number | null
  onSelect: (program: ProgramListItem) => void
}) {
  return (
    <div
      data-testid="program-grid-column"
      data-service-id={service.serviceId}
      className="absolute top-0 border-r border-border"
      style={{ left: leftPx, width: epgColumnWidthPx, height: '100%' }}
    >
      {placed
        .filter((p) => p.endMs > timeWindow.startMs && p.startMs < timeWindow.endMs)
        .map((p) => (
          <ProgramCell
            key={p.program.programId}
            placed={p}
            axis={axis}
            reserved={reservationByProgramId.has(p.program.programId)}
            selected={selectedProgramId === p.program.programId}
            onSelect={onSelect}
          />
        ))}
    </div>
  )
}

function ProgramCell({
  placed,
  axis,
  reserved,
  selected,
  onSelect,
}: {
  placed: PlacedProgram<ProgramListItem>
  axis: TimeAxis
  reserved: boolean
  selected: boolean
  onSelect: (program: ProgramListItem) => void
}) {
  const rect = spanToPx(axis, placed.startMs, placed.endMs)
  if (!rect) return null

  const { program } = placed
  const genre = genreLabel(program.genres[0])
  // 予約済みであることは色ではなく名前でも伝える（色だけの情報にしない）。
  // 読み上げ用だけでなく、セルの中にも見える「予約」を置く。
  const label = [
    formatTime(program.startAt),
    program.name,
    genre,
    reserved ? '予約済み' : undefined,
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <button
      type="button"
      data-testid="program-grid-cell"
      data-program-id={program.programId}
      data-reserved={reserved ? 'true' : undefined}
      aria-pressed={selected}
      aria-label={label}
      onClick={() => onSelect(program)}
      // 高さは放送時間そのものなので、内容がはみ出す側を切る
      className={cn(
        'absolute inset-x-0 overflow-hidden border-b border-l-2 border-b-border px-1.5 py-0.5 text-left hover:brightness-95 dark:hover:brightness-125',
        genreTint(program.genres[0]),
        // 輪は選択中だけ。予約済みに同じ ring-primary を足すと、差が太さだけに
        // なりジャンルの左罫と選択中の両方に紛れる。
        selected && 'ring-2 ring-primary ring-inset',
      )}
      style={{ top: rect.topPx, height: rect.heightPx }}
    >
      {reserved && (
        <>
          {/* 5 分セル（10px）でも残る印。点は高さ 6px でグリッド全体ではほぼ消える */}
          <span
            aria-hidden
            className="pointer-events-none absolute inset-y-0 right-0 w-1 bg-foreground"
          />
          <span
            aria-hidden
            className="absolute top-0 right-1 z-[1] rounded-sm bg-foreground px-0.5 text-[10px] leading-none text-background"
          >
            予約
          </span>
        </>
      )}
      <span
        data-testid="program-grid-cell-time"
        className="block text-[10px] leading-tight text-muted-foreground"
      >
        {formatTime(program.startAt)}
      </span>
      <span className="block text-xs leading-tight">{program.name}</span>
    </button>
  )
}

/** useNow は一定間隔で更新される現在時刻を返す。現在時刻インジケータを動かすため。 */
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs)
    return () => window.clearInterval(id)
  }, [intervalMs])
  return now
}
