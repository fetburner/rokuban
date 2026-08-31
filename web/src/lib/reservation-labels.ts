import type { CapacityOverage, Reservation } from '@/api/generated'
import { intersectingOverages } from '@/lib/capacity'
import { parseEnum } from '@/lib/url-search'

/**
 * stateLabels は reservations.state の表示名（docs/schema.md §3）。
 *
 * 一覧（`pages/reservations.tsx`）と詳細（`pages/reservation-detail.tsx`）の
 * 両方が使う --- 同じ状態が画面によって違う表記（生の enum 値など）で出ると
 * 利用者が混乱するので、ここに定義を集約する（issue #300）。
 *
 * `pages/*.tsx` に置かず独立したファイルにするのは、ページコンポーネントの
 * ファイルが値と（React Fast Refresh が要求する）コンポーネントのみの export
 * を混在させないため（`lib/recording-search.ts` の `statusLabels` /
 * `sourceLabels` と同じ手）。
 */
export const stateLabels: Record<Reservation['state'], string> = {
  active: '有効',
  detached: 'ルール外',
  orphaned: 'EPG から消失',
}

/** ReservationsPageSearch は `/reservations` の URL クエリパラメータ。 */
export type ReservationsPageSearch = {
  /** 問題のある予約だけに絞る。既定の全件表示は URL に書かない。 */
  only?: 'attention'
}

/** parseReservationsSearch は不正な `only` を既定の全件表示へ落とす。 */
export function parseReservationsSearch(
  search: Record<string, unknown>,
): ReservationsPageSearch {
  return { only: parseEnum(search.only, ['attention'] as const) }
}

/** reservationNeedsAttention は予約が非 active または容量不足区間と交差するか判定する。 */
export function reservationNeedsAttention(
  reservation: Reservation,
  overages: readonly CapacityOverage[],
): boolean {
  const startMs = new Date(reservation.startAt).getTime()
  return (
    reservation.state !== 'active' ||
    intersectingOverages(
      overages,
      reservation.site,
      startMs,
      startMs + reservation.durationMs,
    ).length > 0
  )
}
