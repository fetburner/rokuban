import { Link } from '@tanstack/react-router'

import type { Reservation } from '@/api/generated'
import { stateLabels } from '@/lib/reservation-labels'
import { cn } from '@/lib/utils'

/** ReservationOrigin は、ルールの編集先リンクを行全面リンクより手前に置く。 */
export function ReservationOrigin({
  reservation,
  ruleLabel,
}: {
  reservation: Reservation
  ruleLabel: (ruleId: number) => string
}) {
  if (reservation.source === 'manual') return <span className="shrink-0">手動</span>
  // source は ruleId と独立した出自。ルールが現在の予約に base を供給して
  // いない場合は ruleId が無いこともあるので、手動と誤表示しない。
  if (reservation.ruleId === undefined) return <span className="shrink-0">ルール</span>
  return (
    <Link
      to="/search"
      search={{ ruleId: reservation.ruleId }}
      className="relative z-10 inline-flex min-h-6 items-center px-1 text-foreground underline underline-offset-2"
      aria-label={`ルール「${ruleLabel(reservation.ruleId)}」`}
    >
      ルール「{ruleLabel(reservation.ruleId)}」
    </Link>
  )
}

/**
 * StateBadge の `detached` の文字色は `text-foreground`（bg-muted 小バッジの
 * 合成後コントラスト対策。docs/frontend/design.md「コントラストは毎回測る」）。
 */
export function StateBadge({ state }: { state: Reservation['state'] }) {
  if (state === 'active') return null
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        state === 'orphaned' ? 'bg-destructive/10 text-destructive' : 'bg-muted text-foreground',
      )}
    >
      {stateLabels[state]}
    </span>
  )
}
