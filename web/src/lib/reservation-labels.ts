import type { CapacityOverage, Reservation } from '@/api/generated'
import { intersectingOverages } from '@/lib/capacity'
import { formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { parseRuleId } from '@/lib/recording-search'
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
  /** ルールに関連付いた予約に絞る。削除済みルール ID は 0 件として扱う。 */
  ruleId?: number
}

/** parseReservationsSearch は不正な `only` / `ruleId` を既定の絞り込みなしへ落とす。 */
export function parseReservationsSearch(
  search: Record<string, unknown>,
): ReservationsPageSearch {
  // TanStack Router の非 strict モードでは、生の search に戻り値を重ねるため
  // 無効な値を消すキーも明示的に返す（docs/frontend/recordings.md §validateSearch）。
  return {
    only: parseEnum(search.only, ['attention'] as const),
    ruleId: parseRuleId(search.ruleId),
  }
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

/**
 * reservationRowLabel は予約行の本体リンクに付ける accessible name を組む。
 *
 * 行本体のリンクは子要素を持たない絶対配置なので、children から組めない
 * accessible name を明示する。採否は行を一意に識別できる情報（タイトル・局・
 * 日時・尺・state）だけにする。毎日放送の番組は時刻だけでは同名の行が並ぶ。
 * 見た目の行は日付見出しの下にあるので日付を省くが、名前には日付を残す。
 * 出自や容量バッジの文言は混ぜない。
 */
export function reservationRowLabel(reservation: Reservation): string {
  return [
    programTitle(reservation.title),
    reservation.serviceName,
    formatDateTime(reservation.startAt),
    formatDuration(reservation.durationMs),
    reservation.state === 'active' ? null : stateLabels[reservation.state],
  ]
    // 空文字も落とす（`serviceName` は API required でも空文字を禁じていない）。
    .filter((part): part is string => part !== null && part !== '')
    .join(' ')
}

/** unwatchedLabel は棚の未視聴件数の文言。0 件は「すべて視聴済み」と言う。 */
export function unwatchedLabel(unwatchedCount: number): string {
  return unwatchedCount === 0 ? 'すべて視聴済み' : `未視聴 ${unwatchedCount}`
}
