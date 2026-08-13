import { describe, expect, it } from 'vitest'

import type { Recording, StorageRoot } from '@/api/generated'
import {
  estimateAverageBitrate,
  estimateStorageForecast,
  findMediaRoot,
  isObservationStale,
  recentBitrateSamples,
  upcomingReservationDurationMs,
  type UpcomingReservation,
} from '@/lib/storage-forecast'

/** recording は `recentBitrateSamples` のテスト用に最小限の Recording を組む。 */
function recording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 1,
    site: 'default',
    source: 'manual',
    serviceName: 'test',
    channelType: 'GR',
    channel: '1',
    networkId: 1,
    serviceId: 1,
    eventId: 1,
    title: 'test',
    startAt: '2026-08-01T00:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-08-01T00:00:00Z',
    sizeBytes: 900_000_000,
    ...overrides,
  }
}

/** reservation は `upcomingReservationDurationMs` のテスト用の最小限の予約。 */
function reservation(overrides: Partial<UpcomingReservation> = {}): UpcomingReservation {
  return {
    startAt: '2026-08-13T00:00:00Z',
    durationMs: 1_800_000,
    skip: false,
    ...overrides,
  }
}

describe('recentBitrateSamples', () => {
  it('finished かつ sizeBytes ありの録画だけを標本にする', () => {
    const recordings = [
      recording({ id: 1, status: 'finished', sizeBytes: 100 }),
      // 録画中: 途中経過の sizeBytes を含めると平均が偏るので除外
      recording({ id: 2, status: 'recording', sizeBytes: 50 }),
      // 失敗: durationMs（全尺）に対して sizeBytes が途中までしか無い
      recording({ id: 3, status: 'failed', sizeBytes: 20 }),
      // キャンセル: sizeBytes が無い
      recording({ id: 4, status: 'canceled', sizeBytes: undefined }),
      // 完了だが原本削除済み: sizeBytes が無い
      recording({ id: 5, status: 'finished', sizeBytes: undefined }),
    ]

    const samples = recentBitrateSamples(recordings)

    expect(samples).toHaveLength(1)
    expect(samples[0]).toEqual({ sizeBytes: 100, durationMs: 1_800_000 })
  })

  it('録画が 0 件なら標本も 0 件', () => {
    expect(recentBitrateSamples([])).toEqual([])
  })
})

describe('estimateAverageBitrate', () => {
  it('標本が 0 件のときは undefined（でっち上げの既定値を置かない）', () => {
    expect(estimateAverageBitrate([])).toBeUndefined()
  })

  it('Σ sizeBytes / Σ durationMs を返す', () => {
    // 900MB/30分 + 300MB/10分 = 1200MB / 40分(2_400_000ms) = 500 bytes/ms
    const estimate = estimateAverageBitrate([
      { sizeBytes: 900_000_000, durationMs: 1_800_000 },
      { sizeBytes: 300_000_000, durationMs: 600_000 },
    ])

    expect(estimate).toBeDefined()
    expect(estimate?.bytesPerMs).toBeCloseTo(500)
    expect(estimate?.sampleSize).toBe(2)
  })
})

describe('upcomingReservationDurationMs', () => {
  const windowStart = new Date('2026-08-13T00:00:00Z').getTime()
  const windowEnd = new Date('2026-08-20T00:00:00Z').getTime()

  it('skip=true の予約は合算しない（reconciler が mirakc に同期しない予約）', () => {
    const reservations = [
      reservation({ durationMs: 1_000_000, skip: false }),
      reservation({ durationMs: 2_000_000, skip: true }),
    ]

    expect(upcomingReservationDurationMs(reservations, windowStart, windowEnd)).toBe(1_000_000)
  })

  it('窓の外の予約は合算しない（半開区間: 開始側は含む、終了側は含まない）', () => {
    const reservations = [
      reservation({ startAt: new Date(windowStart).toISOString(), durationMs: 111 }), // 窓の開始そのもの: 含む
      reservation({ startAt: new Date(windowEnd).toISOString(), durationMs: 222 }), // 窓の終了そのもの: 含まない
      reservation({ startAt: new Date(windowStart - 1).toISOString(), durationMs: 333 }), // 窓の直前: 含まない
      reservation({ startAt: new Date(windowEnd - 1).toISOString(), durationMs: 444 }), // 窓の直前(終了側): 含む
    ]

    expect(upcomingReservationDurationMs(reservations, windowStart, windowEnd)).toBe(111 + 444)
  })

  it('予約が 0 件なら 0', () => {
    expect(upcomingReservationDurationMs([], windowStart, windowEnd)).toBe(0)
  })
})

describe('isObservationStale', () => {
  const observedAt = '2026-08-13T00:00:00Z'
  const observedAtMs = new Date(observedAt).getTime()

  it('しきい値以内なら古くない', () => {
    expect(isObservationStale(observedAt, observedAtMs + 1000, 60_000)).toBe(false)
  })

  it('しきい値を超えたら古い', () => {
    expect(isObservationStale(observedAt, observedAtMs + 60_001, 60_000)).toBe(true)
  })
})

describe('findMediaRoot', () => {
  const scratch: StorageRoot = {
    root: 'scratch',
    path: '/scratch',
    totalBytes: 1,
    usedBytes: 1,
    availableBytes: 1,
    observedAt: '2026-08-13T00:00:00Z',
  }
  const media: StorageRoot = {
    root: 'media',
    path: '/media',
    totalBytes: 2,
    usedBytes: 2,
    availableBytes: 2,
    observedAt: '2026-08-13T00:00:00Z',
  }

  it('media root を見つける', () => {
    expect(findMediaRoot([scratch, media])).toEqual(media)
  })

  it('media root が無ければ undefined（観測なしで黙る）', () => {
    expect(findMediaRoot([scratch])).toBeUndefined()
    expect(findMediaRoot([])).toBeUndefined()
  })
})

describe('estimateStorageForecast', () => {
  const nowMs = new Date('2026-08-13T00:00:00Z').getTime()

  it('録画実績が 0 件（averageBitrate が undefined）のときは見込みを出さない', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: undefined,
      upcomingDurationMs: 1_000_000,
      nowMs,
    })

    expect(forecast.hasEstimate).toBe(false)
    expect(forecast.sampleSize).toBe(0)
    expect(forecast.projectedConsumptionBytes).toBeUndefined()
    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  it('見込み消費が残量に収まるときは満杯見込み日を出さない（「足りる」は主張しない）', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 1_000,
      averageBitrate: { bytesPerMs: 1, sampleSize: 5 }, // 1 byte/ms
      upcomingDurationMs: 500, // 500 bytes 消費見込み < 1000
      nowMs,
    })

    expect(forecast.hasEstimate).toBe(true)
    expect(forecast.sampleSize).toBe(5)
    expect(forecast.projectedConsumptionBytes).toBe(500)
    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  it('見込み消費がちょうど残量と等しいときも超過とはしない（境界値）', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 500,
      averageBitrate: { bytesPerMs: 1, sampleSize: 5 },
      upcomingDurationMs: 500, // 500 bytes 消費見込み === 500 残量
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  it('見込み消費が残量を超えるときは満杯見込み日を線形外挿で出す', () => {
    const windowDays = 7
    const windowMs = windowDays * 24 * 60 * 60 * 1000
    // 7 日で 1_400 bytes 消費見込み（200 bytes/日）、残量 100 bytes
    // → 100 / 200 = 0.5 日 = windowMs / 14 ms で満杯
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: { bytesPerMs: 1, sampleSize: 3 },
      upcomingDurationMs: 1_400,
      nowMs,
      windowDays,
    })

    expect(forecast.exceedsAvailable).toBe(true)
    expect(forecast.projectedConsumptionBytes).toBe(1_400)
    expect(forecast.fullAtMs).toBeCloseTo(nowMs + windowMs / 14)
  })
})
