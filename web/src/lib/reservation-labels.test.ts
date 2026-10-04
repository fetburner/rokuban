import { describe, expect, it } from 'vitest'

import type { CapacityOverage, Reservation } from '@/api/generated'
import {
  reservationNeedsAttention,
  reservationRowLabel,
  reservationVerdict,
} from '@/lib/reservation-labels'

const startAt = '2026-10-04T19:00:00+09:00'

function reservation(overrides: Partial<Reservation> = {}): Reservation {
  return {
    site: 'default',
    programId: 12,
    source: 'manual',
    state: 'active',
    title: 'テスト番組',
    serviceName: 'テスト局',
    channelType: 'GR',
    startAt,
    durationMs: 60 * 60_000,
    createdAt: startAt,
    updatedAt: startAt,
    skip: false,
    series: null,
    ...overrides,
  }
}

function overage(overrides: Partial<CapacityOverage> = {}): CapacityOverage {
  return {
    site: 'default',
    startAt: '2026-10-04T19:30:00+09:00',
    endAt: '2026-10-04T20:30:00+09:00',
    shortfall: 1,
    jammedTypes: ['GR'],
    ...overrides,
  }
}

describe('reservationVerdict', () => {
  it('orphaned を skip・容量不足より先に「録画されず」とする', () => {
    expect(
      reservationVerdict(
        reservation({ state: 'orphaned', skip: true, dedupMatchRecordingId: 77 }),
        [overage()],
      ),
    ).toEqual({ kind: 'not-recorded' })
  })

  it('重複 skip を容量不足より先に結論にする', () => {
    expect(
      reservationVerdict(
        reservation({ skip: true, dedupMatchRecordingId: 77 }),
        [overage()],
      ),
    ).toEqual({ kind: 'skip-duplicate' })
  })

  it('根拠のない skip は除外として結論にする', () => {
    expect(reservationVerdict(reservation({ skip: true }), [overage()])).toEqual({
      kind: 'skip-excluded',
    })
  })

  it('自 site の不足区間と交差した予約を録画予定・不足時間帯にする', () => {
    expect(reservationVerdict(reservation(), [overage()])).toEqual({
      kind: 'scheduled-shortfall',
      overages: [overage()],
    })
  })

  it('他 site の不足区間は自分の予約の結論に影響しない', () => {
    expect(reservationVerdict(reservation(), [overage({ site: 'takamatsu' })])).toEqual({
      kind: 'scheduled',
    })
  })

  it('overages が未取得なら不足を主張せず録画予定にする', () => {
    expect(reservationVerdict(reservation())).toEqual({ kind: 'scheduled' })
  })
})

describe('reservation attention and accessible labels', () => {
  it('要確認は orphaned と不足交差を含み、detached 単独と skip を除く', () => {
    expect(reservationNeedsAttention(reservation({ state: 'orphaned' }), [])).toBe(true)
    expect(reservationNeedsAttention(reservation({ state: 'detached' }), [])).toBe(false)
    expect(reservationNeedsAttention(reservation(), [overage()])).toBe(true)
    expect(reservationNeedsAttention(reservation({ skip: true }), [overage()])).toBe(false)
  })

  it('行の accessible name は内部 state ではなく録画の結論を使う', () => {
    const label = reservationRowLabel(reservation())
    expect(label).toContain('録画予定')
    expect(label).not.toContain('active')
    expect(label).not.toContain('有効')
  })
})
