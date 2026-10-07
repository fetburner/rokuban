import { Link } from '@tanstack/react-router'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { CapacityShortfallBadge } from '@/components/capacity-shortfall-badge'
import { reservationVerdict } from '@/lib/reservation-labels'
import { cn } from '@/lib/utils'

/** ReservationOrigin は、ルールの編集先リンクを行全面リンクより手前に置く。 */
export function ReservationOrigin({
  reservation,
  ruleLabel,
}: {
  reservation: Reservation
  ruleLabel: (ruleId: number) => string
}) {
  const outsideRule = reservation.state === 'detached'
  // detached は録画可否ではなく出自の情報なので、結論バッジには混ぜない。
  // ルール条件から外れた状態はここで読めるようにする。
  if (reservation.source === 'manual') {
    return <span className="shrink-0">{outsideRule ? '手動・ルール条件外' : '手動'}</span>
  }
  // source は ruleId と独立した出自。ルールが現在の予約に base を供給して
  // いない場合は ruleId が無いこともあるので、手動と誤表示しない。
  if (reservation.ruleId === undefined) {
    return <span className="shrink-0">{outsideRule ? 'ルール条件外' : 'ルール'}</span>
  }
  return (
    <span className="inline-flex min-h-6 shrink-0 items-center gap-1">
      <Link
        to="/search"
        search={{ ruleId: reservation.ruleId }}
        className="relative z-10 inline-flex min-h-6 pointer-coarse:min-h-11 items-center px-1 text-foreground underline underline-offset-2"
        aria-label={`ルール「${ruleLabel(reservation.ruleId)}」`}
      >
        ルール「{ruleLabel(reservation.ruleId)}」
      </Link>
      {outsideRule && <span>条件外</span>}
    </span>
  )
}

/** 一覧で結論を 1 つだけ伝える。予定は無印、不足は既存の区間バッジで補足する。 */
export function ReservationVerdictBadge({
  reservation,
  overages,
}: {
  reservation: Reservation
  overages?: readonly CapacityOverage[]
}) {
  const verdict = reservationVerdict(reservation, overages)
  if (verdict.kind === 'scheduled') return null
  if (verdict.kind === 'scheduled-shortfall') {
    const startMs = Date.parse(reservation.startAt)
    return (
      <CapacityShortfallBadge
        overages={verdict.overages}
        site={reservation.site}
        startMs={startMs}
        endMs={startMs + reservation.durationMs}
      />
    )
  }

  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        verdict.kind === 'not-recorded'
          ? 'bg-destructive/10 text-destructive'
          : 'bg-muted text-foreground',
      )}
    >
      {verdict.kind === 'not-recorded'
        ? '録画されず'
        : verdict.kind === 'skip-duplicate'
          ? '録画しない（重複）'
          : '録画しない（除外）'}
    </span>
  )
}
