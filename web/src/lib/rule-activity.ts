import type { CapacityOverage, Reservation } from '@/api/generated'
import { reservationVerdict } from '@/lib/reservation-labels'

export type RuleActivitySummary = {
  scheduledCount: number
  /** Undefined means capacity overages are not available yet. */
  shortfallCount: number | undefined
}

/**
 * summarizeRuleActivity counts the reservations that the rule filter on /reservations can show.
 * It deliberately ignores source and uses the shared reservation conclusion for each row.
 */
export function summarizeRuleActivity(
  ruleId: number,
  reservations: readonly Reservation[],
  overages: readonly CapacityOverage[] | undefined,
): RuleActivitySummary {
  let scheduledCount = 0
  let shortfallCount = 0

  for (const reservation of reservations) {
    if (reservation.ruleId !== ruleId) continue

    const { kind } = reservationVerdict(reservation, overages)
    if (kind !== 'scheduled' && kind !== 'scheduled-shortfall') continue

    scheduledCount += 1
    if (kind === 'scheduled-shortfall') shortfallCount += 1
  }

  return {
    scheduledCount,
    shortfallCount: overages === undefined ? undefined : shortfallCount,
  }
}
