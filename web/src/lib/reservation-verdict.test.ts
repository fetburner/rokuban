import { describe, expect, it } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { reservationVerdict } from '@/lib/reservation-verdict'

const reservation: Reservation = {
  site: 'default',
  programId: 1,
  source: 'rule',
  ruleId: 10,
  state: 'active',
  title: '番組',
  serviceName: 'NHK総合',
  channelType: 'GR',
  startAt: '2026-09-01T20:00:00Z',
  durationMs: 60 * 60_000,
  createdAt: '2026-08-01T00:00:00Z',
  updatedAt: '2026-08-01T00:00:00Z',
  skip: false,
  series: null,
}

const overage: CapacityOverage = {
  site: 'default',
  startAt: '2026-09-01T20:00:00Z',
  endAt: '2026-09-01T20:30:00Z',
  shortfall: 1,
  jammedTypes: ['GR'],
}

describe('reservationVerdict', () => {
  it('orphaned wins over skip and a capacity shortfall', () => {
    expect(
      reservationVerdict(
        { ...reservation, state: 'orphaned', skip: true, dedupMatchRecordingId: 8 },
        [overage],
      ),
    ).toEqual({ kind: 'not_recorded' })
  })

  it('a duplicate skip wins over a capacity shortfall', () => {
    expect(
      reservationVerdict(
        { ...reservation, skip: true, dedupMatchRecordingId: 8 },
        [overage],
      ),
    ).toEqual({ kind: 'skipped', reason: 'duplicate' })
  })

  it('a non-duplicate skip is excluded', () => {
    expect(reservationVerdict({ ...reservation, skip: true }, [overage])).toEqual({
      kind: 'skipped',
      reason: 'excluded',
    })
  })

  it('reports a shortfall only for an overlapping interval at the reservation site', () => {
    expect(reservationVerdict(reservation, [overage])).toEqual({
      kind: 'scheduled',
      shortfall: overage,
    })
    expect(reservationVerdict(reservation, [{ ...overage, site: 'other' }])).toEqual({
      kind: 'scheduled',
    })
  })

  it('keeps a scheduled verdict without a shortfall when capacity data is unknown', () => {
    expect(reservationVerdict(reservation, undefined)).toEqual({ kind: 'scheduled' })
  })
})
