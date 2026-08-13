import { describe, expect, it } from 'vitest'

import type { Recording, StorageRoot } from '@/api/generated'
import {
  estimateAverageBitrate,
  estimateStorageForecast,
  findMediaRoot,
  isObservationStale,
  recentBitrateSamples,
  upcomingReservationSchedule,
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

/** reservation は `upcomingReservationSchedule` のテスト用の最小限の予約。 */
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
      // durationMs が 0: 0 除算を避けるため除外（未文書だったガードをここで固定）
      recording({ id: 6, status: 'finished', sizeBytes: 999, durationMs: 0 }),
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

describe('upcomingReservationSchedule', () => {
  const windowStart = new Date('2026-08-13T00:00:00Z').getTime()
  const windowEnd = new Date('2026-08-20T00:00:00Z').getTime()

  it('skip=true の予約は含めない（reconciler が mirakc に同期しない予約）', () => {
    const reservations = [
      reservation({ durationMs: 1_000_000, skip: false }),
      reservation({ durationMs: 2_000_000, skip: true }),
    ]

    const schedule = upcomingReservationSchedule(reservations, windowStart, windowEnd)

    expect(schedule).toHaveLength(1)
    expect(schedule[0].durationMs).toBe(1_000_000)
  })

  it('窓の外の予約は含めない（半開区間: 開始側は含む、終了側は含まない）', () => {
    const reservations = [
      reservation({ startAt: new Date(windowStart).toISOString(), durationMs: 111 }), // 窓の開始そのもの: 含む
      reservation({ startAt: new Date(windowEnd).toISOString(), durationMs: 222 }), // 窓の終了そのもの: 含まない
      reservation({ startAt: new Date(windowStart - 1).toISOString(), durationMs: 333 }), // 窓の直前: 含まない
      reservation({ startAt: new Date(windowEnd - 1).toISOString(), durationMs: 444 }), // 窓の直前(終了側): 含む
    ]

    const schedule = upcomingReservationSchedule(reservations, windowStart, windowEnd)

    expect(schedule.map((e) => e.durationMs).sort((a, b) => a - b)).toEqual([111, 444])
  })

  it('予約が 0 件なら空配列', () => {
    expect(upcomingReservationSchedule([], windowStart, windowEnd)).toEqual([])
  })

  it('startMs 昇順に整列する（入力の順序に依存しない）', () => {
    const reservations = [
      reservation({ startAt: new Date(windowStart + 5000).toISOString(), durationMs: 5 }),
      reservation({ startAt: new Date(windowStart + 1000).toISOString(), durationMs: 1 }),
      reservation({ startAt: new Date(windowStart + 3000).toISOString(), durationMs: 3 }),
    ]

    const schedule = upcomingReservationSchedule(reservations, windowStart, windowEnd)

    expect(schedule.map((e) => e.durationMs)).toEqual([1, 3, 5])
    expect(schedule.map((e) => e.startMs)).toEqual([
      windowStart + 1000,
      windowStart + 3000,
      windowStart + 5000,
    ])
  })
})

describe('isObservationStale', () => {
  const observedAt = '2026-08-13T00:00:00Z'
  const observedAtMs = new Date(observedAt).getTime()

  it('しきい値以内なら古くない', () => {
    expect(isObservationStale(observedAt, observedAtMs + 1000, 60_000)).toBe(false)
  })

  it('しきい値ちょうどは古いとしない（境界値。> であって >= ではない）', () => {
    expect(isObservationStale(observedAt, observedAtMs + 60_000, 60_000)).toBe(false)
  })

  it('しきい値を 1ms でも超えたら古い', () => {
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
      upcomingSchedule: [{ startMs: nowMs, durationMs: 1_000_000 }],
      nowMs,
    })

    expect(forecast.hasEstimate).toBe(false)
    expect(forecast.sampleSize).toBe(0)
    expect(forecast.projectedConsumptionBytes).toBeUndefined()
    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  // 指摘 1: 予約取得が失敗/未解決（upcomingSchedule が undefined）のときに
  // [] へフォールバックすると「+0 B の見込み」という捏造された肯定を描いてしまう
  // （実際に実ブラウザで再現した）。undefined は [] と区別して hasEstimate: false
  // に落ちることを固定する。
  it('予約の取得が未解決/失敗（upcomingSchedule が undefined）のときは見込みを出さない', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: { bytesPerMs: 1, sampleSize: 5 },
      upcomingSchedule: undefined,
      nowMs,
    })

    expect(forecast.hasEstimate).toBe(false)
    expect(forecast.sampleSize).toBe(0)
    expect(forecast.projectedConsumptionBytes).toBeUndefined()
    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  // 指摘 2: 予約が正当に 0 件（upcomingSchedule が空配列。取得は成功したが窓の中に
  // 予約が無い）のときは「算出不能」ではなく「算出した結果が 0」。
  // 上のテストと区別できることをここで固定する（区別できないと指摘 1 の直し方
  // ---undefined を通す--- がここでも [] にフォールバックしてしまい退行する）。
  it('予約が正当に 0 件のときは見込み消費 0 として算出する（算出不能とは区別する）', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: { bytesPerMs: 1, sampleSize: 5 },
      upcomingSchedule: [],
      nowMs,
    })

    expect(forecast.hasEstimate).toBe(true)
    expect(forecast.projectedConsumptionBytes).toBe(0)
    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  it('見込み消費が残量に収まるときは満杯見込み日を出さない（「足りる」は主張しない）', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 1_000,
      averageBitrate: { bytesPerMs: 1, sampleSize: 5 }, // 1 byte/ms
      upcomingSchedule: [{ startMs: nowMs, durationMs: 500 }], // 500 bytes 消費見込み < 1000
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
      upcomingSchedule: [{ startMs: nowMs, durationMs: 500 }], // 500 bytes 消費見込み === 500 残量
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(false)
    expect(forecast.fullAtMs).toBeUndefined()
  })

  it('複数予約に累積し、閾値を超えた予約の途中で交差する', () => {
    // event1: 100 bytes（cumulative 0→100、まだ超えない）
    // event2: 100 bytes（cumulative 100→200、150 で交差 = event2 の 50% 地点）
    const event1Start = nowMs + 1_000
    const event2Start = nowMs + 2_000
    const forecast = estimateStorageForecast({
      availableBytes: 150,
      averageBitrate: { bytesPerMs: 1, sampleSize: 2 },
      upcomingSchedule: [
        { startMs: event1Start, durationMs: 100 },
        { startMs: event2Start, durationMs: 100 },
      ],
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(true)
    expect(forecast.projectedConsumptionBytes).toBe(200)
    expect(forecast.fullAtMs).toBe(event2Start + 50)
  })

  // 指摘 4: 一様分布の仮定を置くと、予約が窓の後半に集中しているケースで
  // 実際より早い満杯見込み日を出してしまう（過大警告）。実際の開始時刻を辿れば、
  // 消費が始まるまでは残量が減らないので満杯見込みはその予約の中に来る。
  it('予約が窓の終盤 1 件に集中しているとき、満杯見込みはその予約の中（終盤）になる', () => {
    const sevenDaysMs = 7 * 24 * 60 * 60 * 1000
    const lateStart = nowMs + sevenDaysMs - 1_000 // 窓の終盤（残り1秒の時点）で開始
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: { bytesPerMs: 2, sampleSize: 1 }, // 2 bytes/ms
      upcomingSchedule: [{ startMs: lateStart, durationMs: 1_000 }], // 2_000 bytes > 100
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(true)
    // 一様分布なら (100/2000)*7日 ≈ 0.35日後（今日〜明日）と出てしまうところ、
    // 実際の開始時刻に沿えば「窓の終盤」に来る。
    expect(forecast.fullAtMs).toBeDefined()
    expect(forecast.fullAtMs! - nowMs).toBeGreaterThan(sevenDaysMs - 2_000)
    expect(forecast.fullAtMs! - lateStart).toBeCloseTo(50) // 100/2000 = 5% × 1000ms = 50ms
  })

  // 指摘 4 の逆方向（過小警告、危険な方向）: 予約が窓の冒頭 1 件に集中している
  // とき、一様分布は「まだ何日か先」と出してしまうが、実際は今日中に満杯になる。
  it('予約が窓の冒頭 1 件に集中しているとき、満杯見込みは今日（冒頭）になる', () => {
    const forecast = estimateStorageForecast({
      availableBytes: 100,
      averageBitrate: { bytesPerMs: 2, sampleSize: 1 }, // 2 bytes/ms
      upcomingSchedule: [{ startMs: nowMs, durationMs: 1_000 }], // 2_000 bytes > 100
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(true)
    // 一様分布なら (100/2000)*7日 ≈ 6 時間ではなく数日先と出てしまうところ、
    // 実際は開始直後（今日）に交差する。
    expect(forecast.fullAtMs! - nowMs).toBeCloseTo(50) // 100/2000 = 5% × 1000ms = 50ms
    expect(forecast.fullAtMs! - nowMs).toBeLessThan(60 * 60 * 1000) // 1 時間以内
  })

  it('availableBytes が既に負で schedule が空でも例外にせず nowMs にフォールバックする', () => {
    const forecast = estimateStorageForecast({
      availableBytes: -10,
      averageBitrate: { bytesPerMs: 1, sampleSize: 1 },
      upcomingSchedule: [],
      nowMs,
    })

    expect(forecast.exceedsAvailable).toBe(true)
    expect(forecast.fullAtMs).toBe(nowMs)
  })

  // 既知の残存近似（レビューで発見。実装は変えず、doc に近似として明記した
  // --- モジュール doc の近似 3「重なる予約を直列として扱う」参照）。
  // `projectedFullAtMs` は予約を startMs 昇順の直列として消費を積み上げるため、
  // 実際に並行して録画される予約（複数チューナー構成では普通）どうしの重なりを
  // 見ない。ここでは実装を固定するのではなく、**この近似がどちらの方向にも
  // 振れることそのもの**を固定する（一方向の保証を書かないための裏付け）。
  it('既知の近似: 同時開始の複数予約は直列消費として扱われ、実際より遅い満杯見込みを出す（過小警告・危険な方向）', () => {
    const sixHours = 6 * 60 * 60 * 1000
    const forecast = estimateStorageForecast({
      availableBytes: 10_000_000,
      averageBitrate: { bytesPerMs: 1, sampleSize: 1 },
      // 同時開始の 6 時間予約 2 本。実際は並行録画で 2 bytes/ms の合成レートに
      // なるが、直列近似は 1 本目の 1 bytes/ms のレートのまま計算する。
      upcomingSchedule: [
        { startMs: nowMs, durationMs: sixHours },
        { startMs: nowMs, durationMs: sixHours },
      ],
      nowMs,
    })

    // 直列近似の報告値（実測: 2.7778 時間）。
    expect(forecast.fullAtMs! - nowMs).toBeCloseTo(10_000_000)
    // 真値（合成レート 2 bytes/ms で 10_000_000 / 2 = 5_000_000ms ≈ 1.3889 時間）
    // より確実に遅い --- 直列近似は実際より「まだ時間がある」と見せてしまう。
    const trueMs = 10_000_000 / 2
    expect(forecast.fullAtMs! - nowMs).toBeGreaterThan(trueMs)
  })

  it('既知の近似: 長い予約に短い予約が重なると、実際より早い満杯見込みを出す（過大警告・安全側だが不正確）', () => {
    const twentyFourHours = 24 * 60 * 60 * 1000
    const thirtyMin = 30 * 60 * 1000
    const forecast = estimateStorageForecast({
      availableBytes: 87_000_000,
      averageBitrate: { bytesPerMs: 1, sampleSize: 1 },
      // 24 時間予約の冒頭に 30 分予約が重なる。直列近似は 24 時間予約の
      // バイト数を積んだ「後」に 30 分予約のバイト数を積むため、実際の
      // 重なり（先頭 30 分だけ合成レート 2 bytes/ms）とは別の場所で交差する。
      upcomingSchedule: [
        { startMs: nowMs, durationMs: twentyFourHours },
        { startMs: nowMs, durationMs: thirtyMin },
      ],
      nowMs,
    })

    // 直列近似の報告値（実測: 0.1667 時間 = 10 分）。
    expect(forecast.fullAtMs! - nowMs).toBeCloseTo(600_000)
    // 真値: 先頭 30 分は合成レート 2 bytes/ms、以降は 24 時間予約のみの
    // 1 bytes/ms。87_000_000 バイトに達するのは
    // 1_800_000（重複区間で消費）+ 83_400_000（単独区間、レート 1）=
    // 85_200_000ms ≈ 23.6667 時間後。
    const trueMs = 1_800_000 + 83_400_000
    expect(forecast.fullAtMs! - nowMs).toBeLessThan(trueMs)
  })
})
