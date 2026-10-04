import type { CapacityOverage, Reservation } from '@/api/generated'
import { intersectingOverages, worstOverage } from '@/lib/capacity'

/**
 * reservationVerdict is the single client-side conclusion about whether a reservation will record.
 *
 * An orphaned reservation has no scheduled recording, regardless of skip. Otherwise skip takes
 * precedence over capacity because skipped reservations are not tuner demand. Capacity data is
 * a lower-bound claim: when it is unavailable, a scheduled reservation stays scheduled without
 * a shortfall modifier.
 */
export type ReservationVerdict =
  | { kind: 'not_recorded' }
  | { kind: 'skipped'; reason: 'duplicate' | 'excluded' }
  | { kind: 'scheduled'; shortfall?: CapacityOverage }

export function reservationVerdict(
  reservation: Reservation,
  overages: readonly CapacityOverage[] | undefined,
): ReservationVerdict {
  if (reservation.state === 'orphaned') return { kind: 'not_recorded' }

  if (reservation.skip) {
    return {
      kind: 'skipped',
      reason:
        reservation.dedupMatchRecordingId === undefined ? 'excluded' : 'duplicate',
    }
  }

  if (overages === undefined) return { kind: 'scheduled' }

  const overlapping = intersectingOverages(
    overages,
    reservation.site,
    new Date(reservation.startAt).getTime(),
    new Date(reservation.startAt).getTime() + reservation.durationMs,
  )
  const shortfall = worstOverage(overlapping) ?? undefined
  return shortfall === undefined
    ? { kind: 'scheduled' }
    : { kind: 'scheduled', shortfall }
}
