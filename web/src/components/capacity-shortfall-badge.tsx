import { Link } from '@tanstack/react-router'
import { TriangleAlert } from 'lucide-react'

import type { CapacityOverage } from '@/api/generated'
import {
  intersectingOverages,
  overageWindow,
  shortageLabel,
  shortageMessage,
  worstOverage,
} from '@/lib/capacity'
import { cn } from '@/lib/utils'

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
 *
 * ## 番組表への導線（issue #233 M6-5、`view` の URL 化は issue #437）
 *
 * バッジは「この時間帯」と言うだけで、その時間帯を見る手段が無かった。番組表
 * ルート（`/programs`。ホーム新設（M8-3）前は `/` だった）の `at`（epoch ms。
 * `lib/programs-search.ts`）に不足区間の開始時刻を積み、`view: 'grid'` も
 * 明示して `Link` にする --- `lg` 以上ではこの `view` がそのままグリッド表示に
 * なり `at` を初期スクロール位置に使う。それ以外（`lg` 未満）ではグリッドが
 * 出ないので `at` は日付ジャンプのフォールバックに使う（`pages/programs.tsx`
 * 参照）。
 *
 * **読み上げの規律（見える側 `aria-hidden`・読み上げ文は `sr-only`）は変えていない。**
 * `<a>` の accessible name はアクセシビリティツリーから除外された記述子
 * （`aria-hidden`）を無視し、`sr-only`（CSS で視覚的に隠すだけで除外されない）は
 * 含めて計算されるため、外側を `span` から `Link` に替えても計算される名前は
 * 変わらず `shortageMessage`（時刻を主語にした文）のまま --- リンク化のために
 * 読み上げ文を書き直す必要が無い。
 *
 * **呼び出し元（`pages/reservations.tsx`）ではこのリンクを別の `Link`（行の
 * 詳細への導線）の中に置かない。** `<a>` の中に `<a>`（が実質的な `<button>` 等の
 * 対話コンテンツも同様）はコンテンツモデル上不正で、クリックの宛先が不定になる
 * --- 行の badge 群は詳細への `Link` の外に出し、同じ `<li>` の中の兄弟要素にする。
 */
export function CapacityShortfallBadge({
  overages,
  site,
  startMs,
  endMs,
  className,
}: {
  /** 窓ぶんの超過区間（`GET /api/capacity/overages` の結果）。 */
  overages: readonly CapacityOverage[]
  /** この行が属するサイト。判定はサイトごとに独立している（docs/data.md §6.5）。 */
  site: string
  startMs: number
  endMs: number
  /**
   * 呼び出し元でのレイアウト調整用（例: 行の詳細への `Link` の外に置くときの
   * 余白）。バッジ自身の意味（警告色・aria）には関わらない。
   */
  className?: string
}) {
  const worst = worstOverage(intersectingOverages(overages, site, startMs, endMs))
  if (worst === null) return null

  return (
    <Link
      to="/programs"
      // 絞り込み中のチャンネル（serviceId）はこの導線の文脈に無いので指定しない
      // （「すべて」のまま開く。呼び出し元は予約一覧で、予約ごとにチャンネルが
      // 違うため、単一の serviceId に絞ると他の予約の不足が見えなくなる）。
      // `view: 'grid'` を明示することで、`at` の有無や画面幅から「グリッドに
      // したいか」を推論する必要が無くなる（`view` が最初から確定している）。
      // ただしグリッドが実際にマウントされるかは `pages/programs.tsx` の
      // `showGrid`（`wideScreen` を待つ）が決めるので、初回レンダーでの
      // マウントを保証するわけではない（`docs/frontend/programs.md`
      // 「番組表への `at` 導線」参照）。`lg` 未満では `showGrid` が
      // `wideScreen` で落とすので無害。
      search={{ view: 'grid', at: overageWindow(worst).startMs }}
      className={cn(
        'flex shrink-0 items-center gap-1 rounded bg-warning/10 px-1.5 py-0.5 text-xs text-warning hover:bg-warning/20 focus-visible:outline-2 focus-visible:outline-warning',
        className,
      )}
    >
      <TriangleAlert className="size-3 shrink-0" aria-hidden="true" />
      {/* 読み上げには文で渡す（バッジの短い表示だけだと主語が「時間帯」であることが
          伝わらず、「この予約が録れない」と読まれかねない） */}
      <span className="sr-only">{shortageMessage(worst)}</span>
      <span aria-hidden="true">{shortageLabel(worst)}</span>
    </Link>
  )
}
