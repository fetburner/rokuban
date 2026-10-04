import type { CapacityOverage, Reservation } from '@/api/generated'
import { intersectingOverages } from '@/lib/capacity'
import { formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { parseRuleId } from '@/lib/recording-search'
import { parseEnum } from '@/lib/url-search'

export type ReservationVerdict =
  | { kind: 'not-recorded' }
  | { kind: 'skip-duplicate' }
  | { kind: 'skip-excluded' }
  | { kind: 'scheduled-shortfall'; overages: CapacityOverage[] }
  | { kind: 'scheduled' }

/**
 * reservationVerdict は予約について画面が伝える結論を一つ決める。
 *
 * 優先順は「録画されなかった → skip の理由 → 自 site の容量不足区間 → 録画予定」。
 * `orphaned` は schedule が一度も観測されず録画試行も無かった意味なので、
 * skip が同時に立っていても「録画されず」が先になる。skip は需要にならないため、
 * skip と容量不足区間が重なっても容量不足の修飾を付けない。
 *
 * 容量をまだ取得できていない場合は `overages` を省略する。予定という結論だけを
 * 伝え、不足を主張しない（沈黙は録画可能の保証ではない）。
 */
export function reservationVerdict(
  reservation: Reservation,
  overages?: readonly CapacityOverage[],
): ReservationVerdict {
  if (reservation.state === 'orphaned') return { kind: 'not-recorded' }
  if (reservation.skip) {
    return reservation.dedupMatchRecordingId === undefined
      ? { kind: 'skip-excluded' }
      : { kind: 'skip-duplicate' }
  }

  if (overages === undefined) return { kind: 'scheduled' }
  const startMs = Date.parse(reservation.startAt)
  const intersecting = intersectingOverages(
    overages,
    reservation.site,
    startMs,
    startMs + reservation.durationMs,
  )
  return intersecting.length > 0
    ? { kind: 'scheduled-shortfall', overages: intersecting }
    : { kind: 'scheduled' }
}

/** reservationVerdictLabel は行の accessible name に含める結論の語。 */
export function reservationVerdictLabel(verdict: ReservationVerdict): string {
  switch (verdict.kind) {
    case 'not-recorded':
      return '録画されず'
    case 'skip-duplicate':
      return '録画しない（重複）'
    case 'skip-excluded':
      return '録画しない（除外）'
    case 'scheduled-shortfall':
    case 'scheduled':
      return '録画予定'
  }
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

/** reservationNeedsAttention は結論が「録画されず」または容量不足の予約か判定する。 */
export function reservationNeedsAttention(
  reservation: Reservation,
  overages: readonly CapacityOverage[],
): boolean {
  const verdict = reservationVerdict(reservation, overages)
  return verdict.kind === 'not-recorded' || verdict.kind === 'scheduled-shortfall'
}

/**
 * reservationRowLabel は予約行の本体リンクに付ける accessible name を組む。
 *
 * 行本体のリンクは子要素を持たない絶対配置なので、children から組めない
 * accessible name を明示する。採否は行を一意に識別できる情報（タイトル・局・
 * 日時・尺・結論）だけにする。毎日放送の番組は時刻だけでは同名の行が並ぶ。
 * 見た目の行は日付見出しの下にあるので日付を省くが、名前には日付を残す。
 * 出自や容量バッジの文言は混ぜない。
 */
export function reservationRowLabel(reservation: Reservation): string {
  return [
    programTitle(reservation.title),
    reservation.serviceName,
    formatDateTime(reservation.startAt),
    formatDuration(reservation.durationMs),
    reservationVerdictLabel(reservationVerdict(reservation)),
  ]
    // 空文字も落とす（`serviceName` は API required でも空文字を禁じていない）。
    .filter((part): part is string => part !== null && part !== '')
    .join(' ')
}

/** unwatchedLabel は棚の未視聴件数の文言。0 件は「すべて視聴済み」と言う。 */
export function unwatchedLabel(unwatchedCount: number): string {
  return unwatchedCount === 0 ? 'すべて視聴済み' : `未視聴 ${unwatchedCount}`
}
