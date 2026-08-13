import { describe, expect, it } from 'vitest'

import { estimateRuleCost, type RuleCostSample } from '@/lib/rule-cost'

describe('estimateRuleCost', () => {
  it('0 件のときは件数・時間ともに 0 になる（未算出とは違う確定値）', () => {
    const sample: RuleCostSample = { totalCount: 0, loadedDurationsMs: [] }
    const estimate = estimateRuleCost(sample)

    expect(estimate.totalCount).toBe(0)
    expect(estimate.countPerWeek).toBe(0)
    // 「まだ計算できていない」（undefined）ではなく「計算した結果が 0」（0）。
    expect(estimate.durationMsPerWeek).toBe(0)
    expect(estimate.sampleSize).toBe(0)
    expect(estimate.isSampled).toBe(false)
  })

  it('結果があり、サンプルが 1 件も読み込めていないときは時間は未算出（undefined）', () => {
    // 件数は totalCount だけで確定するが、時間は durationMs のサンプルが無いと
    // 平均が取れない。ここで 0 を返すと「読み込み中」と「該当が無い」を混同する。
    const sample: RuleCostSample = { totalCount: 10, loadedDurationsMs: [] }
    const estimate = estimateRuleCost(sample)

    expect(estimate.countPerWeek).toBeCloseTo(10 * (7 / 8))
    expect(estimate.durationMsPerWeek).toBeUndefined()
    expect(estimate.isSampled).toBe(true)
  })

  it('7 日換算の係数（windowDays=8 の既定値）を件数・時間の両方に適用する', () => {
    // 全 8 件、各 30 分（1_800_000ms）。8 日分の実測を 7 日分に正規化する。
    const durations = Array.from({ length: 8 }, () => 1_800_000)
    const sample: RuleCostSample = { totalCount: 8, loadedDurationsMs: durations }
    const estimate = estimateRuleCost(sample)

    expect(estimate.countPerWeek).toBeCloseTo(7) // 8 * 7/8 = 7
    expect(estimate.durationMsPerWeek).toBeCloseTo(8 * 1_800_000 * (7 / 8)) // = 12_600_000
    expect(estimate.isSampled).toBe(false)
  })

  it('全件読み込み済みなら isSampled は false（境界値: サンプル件数 = 母数）', () => {
    const durations = [600_000, 1_200_000, 1_800_000]
    const sample: RuleCostSample = { totalCount: 3, loadedDurationsMs: durations }
    const estimate = estimateRuleCost(sample)

    expect(estimate.sampleSize).toBe(3)
    expect(estimate.isSampled).toBe(false)
    // 全件そろっているので平均 * 件数 = 合計そのもの（外挿の誤差が乗らない）
    const totalMs = durations.reduce((a, b) => a + b, 0)
    expect(estimate.durationMsPerWeek).toBeCloseTo(totalMs * (7 / 8))
  })

  it('部分サンプルからの外挿: 平均を母数（totalCount）にかけ、サンプル数にはかけない', () => {
    // 母数 100 件のうち 4 件だけ読み込めている。平均 30 分として 100 件分に外挿する。
    const sample: RuleCostSample = {
      totalCount: 100,
      loadedDurationsMs: [1_800_000, 1_800_000, 1_800_000, 1_800_000],
    }
    const estimate = estimateRuleCost(sample)

    expect(estimate.sampleSize).toBe(4)
    expect(estimate.isSampled).toBe(true)
    // 平均(1_800_000) * 100 件 * 7/8 = 157_500_000ms
    expect(estimate.durationMsPerWeek).toBeCloseTo(157_500_000)
  })
})
