import { Link } from '@tanstack/react-router'
import { CopyCheck, MinusCircle } from 'lucide-react'

import type { Reservation } from '@/api/generated'
import { cn } from '@/lib/utils'

/**
 * skipReason は予約がなぜ録られないのかを判別する。
 *
 * `skip` は `reservations` の列ではなく effective（base + overrides +
 * `program_intents.action`）の結果で、立つ経路が 2 つある。根拠 2 列
 * （`dedupMatchRecordingId` / `dedupSimilarity`）があれば重複排除（M2-6）由来、
 * 無ければユーザーの「録るな」または凍結された base 由来。**根拠の有無で
 * 区別する**のは、ruler が判定のたびに 2 列を作り直す（マッチが消えれば NULL に
 * 戻る）ため、これが「いま重複と判定されている」ことの唯一の表現だから。
 */
function skipReason(reservation: Reservation): 'dedupe' | 'excluded' | null {
  if (!reservation.skip) return null
  return reservation.dedupMatchRecordingId === undefined ? 'excluded' : 'dedupe'
}

/**
 * ReservationSkipBadge は一覧で「録られない予約」に付けるマーカー。
 *
 * 予約行が残っているのに録画されない状態は、それ自体が説明を要する
 * （docs/recording.md §3.1「なぜスキップされたかを説明可能にする」）。
 * skip でなければ何も描画しない（`StateBadge` と同じ「余計な枠を出さない」流儀）。
 *
 * 文字色は `text-foreground`（issue #308）。`text-muted-foreground` だと
 * `bg-muted` との合成後コントラストがライトで 4.5 を割る（他の bg-muted
 * 小バッジと同じ形）。
 */
export function ReservationSkipBadge({ reservation }: { reservation: Reservation }) {
  const reason = skipReason(reservation)
  if (reason === null) return null

  return (
    <span
      className={cn(
        'flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-[0.65rem]',
        'bg-muted text-foreground',
      )}
    >
      {reason === 'dedupe' ? (
        <CopyCheck className="size-3" aria-hidden="true" />
      ) : (
        <MinusCircle className="size-3" aria-hidden="true" />
      )}
      {reason === 'dedupe' ? '重複' : '除外'}
    </span>
  )
}

/**
 * ReservationSkipReason は詳細画面に出す 1 行の説明文。
 *
 * 重複排除の場合は根拠（マッチした録画と類似度）まで出す --- 「なぜスキップ
 * されたか」を説明可能にするのがこの 2 列を持つ目的なので、件数や真偽値だけでは
 * 足りない。類似度は pg_trgm の similarity() で 0.0〜1.0。
 *
 * **「録画 #id」は録画単体ページ（`/recordings/$id`）へのリンクにする**
 * （issue #233 M6-5。「固有名詞はリンク」の原則）。この文はどこにも別の `Link` の
 * 中に置かれていない（呼び出し元は `pages/reservation-detail.tsx` の詳細フィールド）
 * ので、`<a>` の入れ子の心配は無い --- 同じ「参照をリンクにする」変更でも
 * `CapacityShortfallBadge`（予約一覧の行の `Link` の中）とは事情が違う。
 */
export function ReservationSkipReason({ reservation }: { reservation: Reservation }) {
  const reason = skipReason(reservation)
  if (reason === null) return null

  if (reason === 'excluded') {
    return <span>録画しない（除外）</span>
  }
  const similarity = reservation.dedupSimilarity
  return (
    <span>
      重複（
      <Link
        to="/recordings/$id"
        params={{ id: String(reservation.dedupMatchRecordingId) }}
        className="text-primary underline-offset-2 hover:underline"
      >
        録画 #{reservation.dedupMatchRecordingId}
      </Link>
      {similarity === undefined ? '' : `・類似度 ${similarity.toFixed(2)}`}）
    </span>
  )
}
