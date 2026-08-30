/**
 * 容量超過（チューナー不足）区間の扱いと文言。
 *
 * ここに置くのは判定ではなく**表示のための絞り込みと言い方**だけ。判定そのものは
 * サーバー側（docs/data.md §6.5）で、クライアントは返ってきた区間を交差で引くに留める。
 *
 * ## 主張は下界に限る
 *
 * 返ってきた区間が超過していることは確実だが、返らなかった区間が「収まる」ことは
 * 保証されない --- 見えない消費者（並走 EPGStation・ライブ視聴・EPG 収集）と mirakc の
 * `excluded_channels` により、既知の盲点はすべて「警告を見逃す」方向に偏っている
 * （docs/data.md §6.5「これは原理的に近似であり、誤りの向きが偏っている」）。
 *
 * したがってこのモジュールは
 *
 * - 「競合しています」「録画できません」のような**勝敗の主張を作らない**
 *   （どの予約が負けるかを決めるのは mirakc であって Rokuban ではない）
 * - 「収まります」「競合なし」のような**肯定的な文言を持たない**。区間が無いときに
 *   呼び出し側が何も描かないことが唯一の正しい振る舞いで、沈黙は保証ではない
 */

import type { CapacityOverage } from '@/api/generated'
import { formatTime } from '@/lib/format'


/** TimeWindow は epoch ms の半開区間 [startMs, endMs)。 */
export type TimeWindow = {
  startMs: number
  endMs: number
}

/** overageWindow は超過区間を epoch ms に直す。 */
export function overageWindow(overage: CapacityOverage): TimeWindow {
  return {
    startMs: new Date(overage.startAt).getTime(),
    endMs: new Date(overage.endAt).getTime(),
  }
}

/**
 * intersectingOverages は指定サイトの超過区間のうち、[startMs, endMs) と交差する
 * ものを返す。
 *
 * **区間はどちらも半開区間として扱う。** 端で接するだけ（超過区間が 19:00 に終わり
 * 予約が 19:00 に始まる）は交差しない --- 接触を交差に数えると、19:00 時点で
 * 不足していないのに不足していると言うことになり、主張が事実より広がる。
 *
 * site で絞るのは判定がサイトごとに独立しているため。別サイトのチューナー不足は
 * 自分の予約の録画可否に関係しない（docs/data.md §6.5）。
 */
export function intersectingOverages(
  overages: readonly CapacityOverage[],
  site: string,
  startMs: number,
  endMs: number,
): CapacityOverage[] {
  return overages.filter((overage) => {
    if (overage.site !== site) return false
    const span = overageWindow(overage)
    return span.endMs > startMs && span.startMs < endMs
  })
}

/**
 * worstOverage は最も不足の大きい区間を返す（空なら null）。
 *
 * 予約が複数の超過区間に跨るとき、区間をまとめて 1 つの主張にはしない ---
 * `shortfall` と `jammedTypes` は 1 つの区間についての整合した組であり、
 * 種別を合併すると**どの区間でも成り立っていない主張**が出来上がる。
 * 代わりに最も不足の大きい区間を選ぶ（下界の主張なので、最大が最も強く言える）。
 * 同じ不足なら先に並んでいる（= 早い）区間を採る。
 */
export function worstOverage(overages: readonly CapacityOverage[]): CapacityOverage | null {
  let worst: CapacityOverage | null = null
  for (const overage of overages) {
    if (worst === null || overage.shortfall > worst.shortfall) worst = overage
  }
  return worst
}

/**
 * shortfallDetail は不足の内訳（「BS が 1 本」）を組む。
 *
 * 件数だけでなく詰まった種別まで出す。`jammedTypes` は Hall の条件を破った種別の
 * 部分集合なので、そのまま並べれば「GR・BS が 2 本」のように読める。
 * 空の場合（起きない想定）は本数だけにする --- 種別が無いことを理由に不足そのものを
 * 隠すと、判定の副産物の欠落で警告が消える。
 */
export function shortfallDetail(overage: CapacityOverage): string {
  const types = overage.jammedTypes.join('・')
  return types === '' ? `${overage.shortfall} 本` : `${types} が ${overage.shortfall} 本`
}

/** shortageLabel は予約一覧のバッジに出す短い表示（「チューナー不足（BS が 1 本）」）。 */
export function shortageLabel(overage: CapacityOverage): string {
  return `チューナー不足（${shortfallDetail(overage)}）`
}

/**
 * shortageLabelCompact はグリッドの時間軸列（56px）に収まる形（「BS-1」）。
 *
 * `shortageLabel` の全文は幅が要る（実測で `scrollWidth` が `clientWidth` の
 * 2 倍を超え、省略記号で切ると種別も本数も読めなくなる）。時間軸列は番組セルの
 * 幅に比べて狭いので、ここだけ専用の短い形を持つ。詰まった種別が 2 つ以上ある
 * ときは種別の列挙自体が幅を食うので本数だけにする --- 全文は帯の `sr-only`
 * （`shortageRangeMessage`）が持つので、ここで削ってよい。
 */
export function shortageLabelCompact(overage: CapacityOverage): string {
  const [type, ...rest] = overage.jammedTypes
  return type !== undefined && rest.length === 0 ? `${type}-${overage.shortfall}` : `-${overage.shortfall}`
}

/**
 * shortageMessage は文としての説明（「この時間帯はチューナーが不足しています
 * （BS が 1 本不足）」）。
 *
 * 主語は時間帯であって予約ではない。「この予約は競合しています」と書かないのは、
 * 超過が区間の性質であって番組の性質ではないため（docs/data.md §6.5）。
 */
export function shortageMessage(overage: CapacityOverage): string {
  return `この時間帯はチューナーが不足しています（${shortfallDetail(overage)}不足）`
}

/** shortageRangeMessage は時刻を添えた説明。帯は自分がどの区間かを言う必要がある。 */
export function shortageRangeMessage(overage: CapacityOverage): string {
  const range = `${formatTime(overage.startAt)}〜${formatTime(overage.endAt)}`
  return `${range} はチューナーが不足しています（${shortfallDetail(overage)}不足）`
}

/**
 * coveringWindow は与えられた予約すべてを覆う最小の時間窓を返す。空なら null。
 *
 * `GET /api/capacity/overages` は時間窓を必須で取るので、一覧のバッジのために
 * 「一覧に出ている予約が入る窓」を作る必要がある。固定幅（例: 今から 8 日）に
 * しないのは、窓の外に出た予約のバッジが黙って消えるため。null は問い合わせ自体を
 * 止める合図（予約が無ければ超過を訊く意味がない）。
 */
export function coveringWindow(
  reservations: readonly { startAt: string; durationMs: number }[],
): TimeWindow | null {
  let startMs = Number.POSITIVE_INFINITY
  let endMs = Number.NEGATIVE_INFINITY
  for (const reservation of reservations) {
    const from = new Date(reservation.startAt).getTime()
    startMs = Math.min(startMs, from)
    endMs = Math.max(endMs, from + reservation.durationMs)
  }
  if (startMs > endMs) return null
  // 端を広げる必要はない。サーバーの窓の絞り込みも半開区間の交差
  // （`StartAt.Before(end) && EndAt.After(start)`）なので、この窓で落ちる区間は
  // どの予約とも交差しない区間だけ。
  return { startMs, endMs }
}
