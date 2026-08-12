import { TriangleAlert } from 'lucide-react'

import type { CapacityOverage } from '@/api/generated'
import {
  intersectingOverages,
  shortageLabel,
  shortageMessage,
  worstOverage,
} from '@/lib/capacity'

/**
 * CapacityShortfallBadge は「その時間帯にチューナーが足りていない」ことを一覧の行に
 * 出すバッジ（M2-10, issue #24 / #21）。
 *
 * グリッドの帯は `lg` 以上でしか出ないので、**画面幅に依存しない伝達手段**として
 * 別に必要になる（docs/frontend.md「リスト・予約一覧・モバイル: 同じ文言のバッジ」）。
 * 文言は帯と共有する（lib/capacity.ts）。
 *
 * **主張するのは区間の性質だけ。**「この予約は競合しています」「録画できません」とは
 * 書かない --- 負ける側を決めるのは mirakc で、Rokuban から見えない消費者がいるので
 * 予測できない（docs/data.md §6.5）。行に出しているのは「この予約の時間帯が不足区間と
 * 交差している」という事実であって、この予約が録れないという予告ではない。
 *
 * 交差する区間が無ければ何も描かない（`ReservationSkipBadge` / `StateBadge` と同じ
 * 「余計な枠を出さない」流儀）。**「競合なし」「収まります」は出さない** ---
 * 判定の盲点はすべて「警告を見逃す」方向に偏っているので、沈黙を保証として
 * 読ませてはならない（docs/data.md §6.5）。
 */
export function CapacityShortfallBadge({
  overages,
  site,
  startMs,
  endMs,
}: {
  /** 窓ぶんの超過区間（`GET /api/capacity/overages` の結果）。 */
  overages: readonly CapacityOverage[]
  /** この行が属するサイト。判定はサイトごとに独立している（docs/data.md §6.5）。 */
  site: string
  startMs: number
  endMs: number
}) {
  const worst = worstOverage(intersectingOverages(overages, site, startMs, endMs))
  if (worst === null) return null

  return (
    <span className="flex shrink-0 items-center gap-1 rounded bg-warning/10 px-1.5 py-0.5 text-[0.65rem] text-warning">
      <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
      {/* 読み上げには文で渡す（バッジの短い表示だけだと主語が「時間帯」であることが
          伝わらず、「この予約が録れない」と読まれかねない） */}
      <span className="sr-only">{shortageMessage(worst)}</span>
      <span aria-hidden="true">{shortageLabel(worst)}</span>
    </span>
  )
}
