import type { CapacityOverage, Reservation } from '@/api/generated'
import { reservationVerdict } from '@/lib/reservation-verdict'

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

    const verdict = reservationVerdict(reservation, overages)
    if (verdict.kind !== 'scheduled') continue

    scheduledCount += 1
    if (verdict.shortfall !== undefined) shortfallCount += 1
  }

  return {
    scheduledCount,
    shortfallCount: overages === undefined ? undefined : shortfallCount,
  }
}
