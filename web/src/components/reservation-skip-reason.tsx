import { Link } from '@tanstack/react-router'

import type { Reservation } from '@/api/generated'

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
 * ReservationSkipReason は予約詳細の結論に出す 1 行の説明文。
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
    return <span>録画しません（除外）</span>
  }
  const similarity = reservation.dedupSimilarity
  return (
    <span>
      録画しません（重複: {' '}
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
