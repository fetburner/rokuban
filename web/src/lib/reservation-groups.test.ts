import { describe, expect, it } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import { groupReservations, reservationGroupCapacityAt } from '@/lib/reservation-groups'

const origin = new Date('2026-10-02T00:00:00+09:00').getTime()

function reservation(
  id: number,
  series: string | null,
  minute: number,
  overrides: Partial<Reservation> = {},
): Reservation {
  const startAt = new Date(origin + minute * 60_000).toISOString()
  return {
    site: 'default',
    programId: id,
    source: 'manual',
    state: 'active',
    title: `${series ?? '単独'} 第${id}話`,
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt,
    durationMs: 30 * 60_000,
    createdAt: startAt,
    updatedAt: startAt,
    skip: false,
    series,
    ...overrides,
  }
}

function overage(minute: number, shortfall = 1, site = 'default'): CapacityOverage {
  const startAt = new Date(origin + minute * 60_000).toISOString()
  return {
    site,
    startAt,
    endAt: new Date(origin + (minute + 30) * 60_000).toISOString(),
    shortfall,
    jammedTypes: ['BS'],
  }
}

describe('groupReservations', () => {
  it('同じ実効シリーズをまとめ、null は予約ごとに分ける', () => {
    const rows = groupReservations(
      [
        reservation(1, 'シリーズ A', 30),
        reservation(2, 'シリーズ A', 60),
        reservation(3, null, 90),
        reservation(4, null, 120),
        reservation(5, null, 90, { site: 'takamatsu' }),
      ],
      [],
    )

    expect(rows.map((row) => row.reservations.map((item) => item.programId))).toEqual([
      [1, 2],
      [3],
      [5],
      [4],
    ])
    expect(new Set(rows.map((row) => row.key)).size).toBe(4)
  })

  it('次回はスキップを飛ばし、全件スキップなら最初の予約に戻す', () => {
    const rows = groupReservations(
      [
        reservation(1, '番組 A', 0, { skip: true, dedupMatchRecordingId: 11 }),
        reservation(2, '番組 A', 60),
        reservation(3, '番組 B', 120, { skip: true }),
        reservation(4, '番組 B', 180, { skip: true, dedupMatchRecordingId: 12 }),
      ],
      [],
    )

    expect(rows[0].next.programId).toBe(2)
    expect(rows[1].next.programId).toBe(3)
  })

  it('開始時刻が同じ予約は site と programId で安定させ、シリーズは次回順に並べる', () => {
    const rows = groupReservations(
      [
        reservation(1, '遅いシリーズ', 180),
        reservation(2, '早いシリーズ', 30, { site: 'takamatsu' }),
        reservation(3, '早いシリーズ', 30),
      ],
      [],
    )

    expect(rows.map((row) => row.title)).toEqual(['早いシリーズ', '遅いシリーズ'])
    expect(rows[0].reservations.map((item) => item.site)).toEqual(['default', 'takamatsu'])
  })

  it('状態・容量不足・重複スキップのバッジ件数を予約ごとに数える', () => {
    const rows = groupReservations(
      [
        reservation(1, 'シリーズ', 0, {
          state: 'orphaned',
          skip: true,
          dedupMatchRecordingId: 31,
        }),
        reservation(2, 'シリーズ', 60, { state: 'orphaned' }),
        reservation(3, 'シリーズ', 120, { state: 'detached', skip: true }),
        // 同じ放送を別サイトにも予約した場合も 1 本として数える。
        reservation(3, 'シリーズ', 120, { site: 'takamatsu', state: 'active' }),
      ],
      [overage(60, 1), overage(120, 2, 'takamatsu')],
    )

    expect(rows).toHaveLength(1)
    expect(rows[0].reservations).toHaveLength(4)
    expect(rows[0].badges).toEqual({
      orphaned: 2,
      detached: 1,
      capacityShortfall: 2,
      duplicateSkipped: 1,
    })
    expect(reservationGroupCapacityAt(rows[0])).toBe(Date.parse(overage(120, 2, 'takamatsu').startAt))
  })
})
