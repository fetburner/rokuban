import { describe, expect, it } from 'vitest'

import type { ProgramListItem, Reservation } from '@/api/generated'
import { buildReservationOverlapIndex, deriveProgramOverlaps } from '@/lib/program-overlaps'
import type { SiteProgram } from '@/lib/all-sites-services'

const targetStart = new Date('2026-08-14T10:00:00+09:00').getTime()

function program(overrides: Partial<ProgramListItem> = {}): SiteProgram {
  return {
    site: 'default',
    programId: 1,
    networkId: 32736,
    serviceId: 1024,
    eventId: 1,
    startAt: new Date(targetStart).toISOString(),
    endAt: new Date(targetStart + 3_600_000).toISOString(),
    durationMs: 3_600_000,
    name: '対象番組',
    description: '',
    genres: [],
    isFree: true,
    ...overrides,
  }
}

function reservation(
  programId: number,
  startAt: number,
  overrides: Partial<Reservation> = {},
): Reservation {
  return {
    site: 'default',
    programId,
    source: 'manual',
    state: 'active',
    title: `予約 ${programId}`,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt: new Date(startAt).toISOString(),
    durationMs: 1_800_000,
    createdAt: new Date(targetStart).toISOString(),
    updatedAt: new Date(targetStart).toISOString(),
    skip: false,
    ...overrides,
  }
}

function index(reservations: readonly Reservation[]) {
  return buildReservationOverlapIndex(reservations)
}

describe('deriveProgramOverlaps', () => {
  it('同一 site・別 programId・半開区間で重なる予約だけを件数と内訳に含める', () => {
    const target = program()
    const included = reservation(2, targetStart + 1_800_000, { title: '含める予約' })
    const crossingStart = reservation(3, targetStart - 1_800_000, {
      title: '前から重なる予約',
      durationMs: 3_600_000,
    })
    const adjacentBefore = reservation(4, targetStart - 1_800_000, {
      title: '直前の予約',
      durationMs: 1_800_000,
    })
    const adjacentAfter = reservation(5, targetStart + 3_600_000, {
      title: '直後の予約',
    })

    const overlaps = deriveProgramOverlaps(
      target,
      index([
        included,
        crossingStart,
        adjacentBefore,
        adjacentAfter,
        reservation(target.programId, targetStart + 1_800_000, { title: '自分自身' }),
        reservation(6, targetStart + 1_800_000, { site: 'other', title: '別 site' }),
        reservation(7, targetStart + 1_800_000, { state: 'orphaned', title: 'orphaned' }),
        reservation(8, targetStart + 1_800_000, { skip: true, title: 'skip' }),
      ]),
    )

    expect(overlaps).toEqual({
      count: 2,
      reservations: [
        {
          programId: 2,
          title: '含める予約',
          startAt: included.startAt,
          durationMs: included.durationMs,
        },
        {
          programId: 3,
          title: '前から重なる予約',
          startAt: crossingStart.startAt,
          durationMs: crossingStart.durationMs,
        },
      ],
    })
  })

  it('orphaned 以外の state は重なりに含める', () => {
    const target = program()
    const detached = reservation(2, targetStart + 1_800_000, { state: 'detached' })

    expect(deriveProgramOverlaps(target, index([detached]))).toEqual({
      count: 1,
      reservations: [
        {
          programId: detached.programId,
          title: detached.title,
          startAt: detached.startAt,
          durationMs: detached.durationMs,
        },
      ],
    })
  })
})
