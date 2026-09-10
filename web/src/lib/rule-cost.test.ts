import { describe, expect, it } from 'vitest'

import { estimateRuleCost, type RuleCostInput } from '@/lib/rule-cost'

describe('estimateRuleCost', () => {
  it('0 件のときは件数・時間ともに 0 になる（未算出とは違う確定値）', () => {
    const input: RuleCostInput = { totalCount: 0, durationsMs: [] }
    const estimate = estimateRuleCost(input)

    expect(estimate.totalCount).toBe(0)
    expect(estimate.countPerWeek).toBe(0)
    // 「まだ計算できていない」（undefined）ではなく「計算した結果が 0」（0）。
    expect(estimate.durationMsPerWeek).toBe(0)
  })

  it('7 日換算の係数（windowDays=8 の既定値）を件数・時間の両方に適用する', () => {
    // 全 8 件、各 30 分（1_800_000ms）。8 日分の実測を 7 日分に正規化する。
    const durations = Array.from({ length: 8 }, () => 1_800_000)
    const input: RuleCostInput = { totalCount: 8, durationsMs: durations }
    const estimate = estimateRuleCost(input)

    expect(estimate.countPerWeek).toBeCloseTo(7) // 8 * 7/8 = 7
    expect(estimate.durationMsPerWeek).toBeCloseTo(8 * 1_800_000 * (7 / 8)) // = 12_600_000
  })

  it('全件の durationMs の合計を 7 日換算する', () => {
    const durations = [600_000, 1_200_000, 1_800_000]
    const input: RuleCostInput = { totalCount: 3, durationsMs: durations }
    const estimate = estimateRuleCost(input)

    const totalMs = durations.reduce((a, b) => a + b, 0)
    expect(estimate.durationMsPerWeek).toBeCloseTo(totalMs * (7 / 8))
  })
})
