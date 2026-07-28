/**
 * 番組表グリッドの座標系。
 *
 * グリッドの存在理由は「見やすさ」ではなく**同時性を空間に符号化すること**
 * （docs/frontend.md）。したがって時刻 → px の写像がこのモジュールの本体であり、
 * React に依存しない純粋な関数として切り出してある。
 *
 * 切り出す理由はテスト可能性だけではない。M2-10 の容量超過の帯は
 * 「全チャンネル縦断の帯」として同じ縦軸の上に描かれる（番組ではなく区間に描く。
 * docs/frontend.md「容量超過は番組ではなく区間に描く」）。番組セルと帯が同じ
 * `spanToPx` を通れば、両者が同じ時刻で必ず同じ位置に来る。
 */

import type { Service } from '@/api/generated'

const msPerHour = 3_600_000

/** epgColumnWidthPx は 1 サービス（1 列）の幅。列の仮想化がこの固定幅に依存する。 */
export const epgColumnWidthPx = 176

/** TimeAxis はグリッドの縦軸。`startMs` から `endMs` までを 1 時間 `pxPerHour` で線形に写す。 */
export type TimeAxis = {
  /** 軸の開始時刻（epoch ms）。 */
  startMs: number
  /** 軸の終了時刻（epoch ms）。 */
  endMs: number
  /** 1 時間あたりの高さ（px）。 */
  pxPerHour: number
}

/** axisHeightPx は軸全体の高さ（px）を返す。 */
export function axisHeightPx(axis: TimeAxis): number {
  return ((axis.endMs - axis.startMs) / msPerHour) * axis.pxPerHour
}

/**
 * timeToPx は時刻を軸上の位置（px, 軸の先頭からの距離）に写す。
 *
 * 軸の外はクランプしない（クランプすると呼び出し側が「はみ出している」ことを
 * 判定できなくなる）。矩形を得たいときは spanToPx を使う。
 */
export function timeToPx(axis: TimeAxis, ms: number): number {
  return ((ms - axis.startMs) / msPerHour) * axis.pxPerHour
}

/** pxToTime は軸上の位置から時刻（epoch ms）を返す。timeToPx の逆写像。 */
export function pxToTime(axis: TimeAxis, px: number): number {
  return axis.startMs + (px / axis.pxPerHour) * msPerHour
}

/** SpanRect は軸上の矩形。 */
export type SpanRect = {
  topPx: number
  heightPx: number
}

/**
 * spanToPx は時間区間 [startMs, endMs) を軸上の矩形に写す。軸の外にはみ出す分は
 * 切り落とし、まったく交差しなければ null を返す。
 *
 * 高さに下限は設けない。5 分番組が 5 分ぶんの高さになることがグリッドの主張
 * そのものなので、見やすさのために下限を入れると同時性の符号化が崩れる。
 */
export function spanToPx(axis: TimeAxis, startMs: number, endMs: number): SpanRect | null {
  const from = Math.max(startMs, axis.startMs)
  const to = Math.min(endMs, axis.endMs)
  if (to <= from) return null
  const topPx = timeToPx(axis, from)
  return { topPx, heightPx: timeToPx(axis, to) - topPx }
}

/** HourTick は時間軸の目盛り。 */
export type HourTick = {
  ms: number
  topPx: number
}

/**
 * hourTicks は軸に含まれる毎時 0 分の目盛りを返す。
 *
 * 1 時間ぶんの ms を足すのではなく `setHours(+1)` で進めるのは、夏時間のある
 * ロケールで「毎時 0 分」がずれないようにするため（位置の計算は実経過時間の
 * ままなので、軸は常に実時間に対して線形である）。
 */
export function hourTicks(axis: TimeAxis): HourTick[] {
  const ticks: HourTick[] = []
  const cursor = new Date(axis.startMs)
  cursor.setMinutes(0, 0, 0)
  if (cursor.getTime() < axis.startMs) cursor.setHours(cursor.getHours() + 1)
  while (cursor.getTime() < axis.endMs) {
    ticks.push({ ms: cursor.getTime(), topPx: timeToPx(axis, cursor.getTime()) })
    cursor.setHours(cursor.getHours() + 1)
  }
  return ticks
}

/**
 * visibleTimeWindow は縦スクロール位置から、描画すべき時間帯を返す。
 *
 * `viewportPx` が 0 以下（= まだ計測できていない）のときは軸全体を返す。
 * 「計測できていないから何も描かない」にすると、レイアウトが確定するまで
 * 空のグリッドが出るうえ、DOM 計測を持たない環境（テスト）で
 * 「何も描かれていないのに通る」テストを許してしまう。
 */
export function visibleTimeWindow(
  axis: TimeAxis,
  scrollTopPx: number,
  viewportPx: number,
  overscanPx = 0,
): { startMs: number; endMs: number } {
  if (viewportPx <= 0) return { startMs: axis.startMs, endMs: axis.endMs }
  return {
    startMs: Math.max(axis.startMs, pxToTime(axis, scrollTopPx - overscanPx)),
    endMs: Math.min(axis.endMs, pxToTime(axis, scrollTopPx + viewportPx + overscanPx)),
  }
}

/**
 * visibleColumnRange は横スクロール位置から、描画すべき列の添字範囲 [start, end) を返す。
 *
 * 列は固定幅なので、可視範囲は除算 1 回で出る。`viewportPx` が 0 以下のときは
 * 全列を返す（visibleTimeWindow と同じ理由）。
 */
export function visibleColumnRange(
  columnCount: number,
  columnWidthPx: number,
  scrollLeftPx: number,
  viewportPx: number,
  overscanColumns = 0,
): { start: number; end: number } {
  if (viewportPx <= 0 || columnWidthPx <= 0) return { start: 0, end: columnCount }
  const first = Math.floor(scrollLeftPx / columnWidthPx) - overscanColumns
  const last = Math.ceil((scrollLeftPx + viewportPx) / columnWidthPx) + overscanColumns
  return {
    start: Math.min(Math.max(0, first), columnCount),
    end: Math.min(columnCount, Math.max(0, last)),
  }
}

/** PlacedProgram は開始・終了を epoch ms に解決した番組。時刻の parse を描画から追い出すための型。 */
export type PlacedProgram<P extends { startAt: string; endAt: string }> = {
  program: P
  startMs: number
  endMs: number
}

/**
 * groupProgramsByService はサービスごとに番組を開始時刻の昇順で並べて返す。
 *
 * 時刻の parse をここで一度だけ行う。描画のたびに `new Date(...)` を呼ぶと
 * 可視判定が番組数 x 再描画回数の parse になる。
 */
export function groupProgramsByService<
  P extends { serviceId: number; startAt: string; endAt: string },
>(programs: readonly P[]): Map<number, PlacedProgram<P>[]> {
  const byService = new Map<number, PlacedProgram<P>[]>()
  for (const program of programs) {
    const placed: PlacedProgram<P> = {
      program,
      startMs: new Date(program.startAt).getTime(),
      endMs: new Date(program.endAt).getTime(),
    }
    const list = byService.get(program.serviceId)
    if (list) list.push(placed)
    else byService.set(program.serviceId, [placed])
  }
  for (const list of byService.values()) list.sort((a, b) => a.startMs - b.startMs)
  return byService
}

/** チャンネル種別の並び順。リモコン番号の意味が種別ごとに違うので、まず種別で束ねる。 */
const channelTypeOrder: Record<Service['channelType'], number> = {
  GR: 0,
  BS: 1,
  CS: 2,
  SKY: 3,
}

/**
 * orderServices はグリッドの列順にサービスを並べる。
 *
 * 種別（GR → BS → CS → SKY）→ リモコン番号 → serviceId の全順序。API の返す順
 * （networkId, serviceId）に任せると地上波のリモコン番号順にならず、視聴者の
 * 知っている並びと食い違う。同値のない全順序なので、再描画で列が入れ替わらない。
 */
export function orderServices(services: readonly Service[]): Service[] {
  return [...services].sort(
    (a, b) =>
      channelTypeOrder[a.channelType] - channelTypeOrder[b.channelType] ||
      a.remoteControlKeyId - b.remoteControlKeyId ||
      a.serviceId - b.serviceId,
  )
}
