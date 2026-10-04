import { describe, expect, it } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { summarizeRuleActivity } from '@/lib/rule-activity'

function reservation(
  id: number,
  overrides: Partial<Reservation> = {},
): Reservation {
  return {
    site: 'default',
    programId: id,
    source: 'rule',
    ruleId: 10,
    state: 'active',
    title: `番組 ${id}`,
    serviceName: 'NHK総合',
    channelType: 'GR',
    startAt: '2026-09-01T20:00:00Z',
    durationMs: 60 * 60_000,
    createdAt: '2026-08-01T00:00:00Z',
    updatedAt: '2026-08-01T00:00:00Z',
    skip: false,
    series: null,
    ...overrides,
  }
}

const overage: CapacityOverage = {
  site: 'default',
  startAt: '2026-09-01T20:00:00Z',
  endAt: '2026-09-01T20:30:00Z',
  shortfall: 1,
  jammedTypes: ['GR'],
}

describe('summarizeRuleActivity', () => {
  it('counts every source for the same rule, while excluding skipped and orphaned reservations', () => {
    const result = summarizeRuleActivity(
      10,
      [
        reservation(1),
        reservation(2, { source: 'manual', startAt: '2026-09-01T22:00:00Z' }),
        reservation(3, { skip: true }),
        reservation(4, { state: 'orphaned' }),
        reservation(5, { ruleId: 11 }),
      ],
      [overage],
    )

    expect(result).toEqual({ scheduledCount: 2, shortfallCount: 1 })
  })

  it('does not claim zero shortfall while capacity data is unavailable', () => {
    expect(summarizeRuleActivity(10, [reservation(1)], undefined)).toEqual({
      scheduledCount: 1,
      shortfallCount: undefined,
    })
  })

  it('returns a known zero when there are no scheduled reservations', () => {
    expect(summarizeRuleActivity(10, [], [])).toEqual({
      scheduledCount: 0,
      shortfallCount: 0,
    })
  })
})
