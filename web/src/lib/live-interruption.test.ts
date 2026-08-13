import { describe, expect, it } from 'vitest'

import {
  interruptionLookaheadMs,
  interruptionWarningMessage,
  upcomingInterruptingReservation,
  type InterruptingReservationCandidate,
} from '@/lib/live-interruption'

/** 時刻はローカルの 0 時基準で組む（capacity.test.ts と同じ流儀）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)
const nowMs = dayStart.getTime()

/** at は 0 時からの分数を ISO 文字列に直す。 */
function at(minutes: number): string {
  return new Date(nowMs + minutes * 60_000).toISOString()
}

function reservation(
  overrides: Partial<InterruptingReservationCandidate> = {},
): InterruptingReservationCandidate {
  return {
    site: 'default',
    programId: 1,
    skip: false,
    startAt: at(30),
    ...overrides,
  }
}

const sameType = new Set([1])

describe('upcomingInterruptingReservation', () => {
  // --- 該当するとき返す方向 ---

  it('skip されておらず・同じチャンネル種別・窓内なら返す', () => {
    const r = reservation({ startAt: at(30) })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBe(r)
  })

  it('複数が一致するときは最も早く始まるものを返す（配列の並び順に依存しない。両方向）', () => {
    const early = reservation({ programId: 1, startAt: at(90) })
    const earlier = reservation({ programId: 1, startAt: at(30) })

    // 早い方が後ろにある並び
    expect(upcomingInterruptingReservation([early, earlier], 'default', sameType, nowMs)).toBe(
      earlier,
    )
    // 早い方が先頭にある並び --- 「最後に見た候補で上書きする」実装だとこちらで
    // `early` を返してしまう
    expect(upcomingInterruptingReservation([earlier, early], 'default', sameType, nowMs)).toBe(
      earlier,
    )
  })

  it('窓の開始境界（nowMs ちょうど）は含む', () => {
    const r = reservation({ startAt: new Date(nowMs).toISOString() })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBe(r)
  })

  it('窓の終了境界の直前（lookaheadMs - 1ms）は含む', () => {
    const r = reservation({ startAt: new Date(nowMs + interruptionLookaheadMs - 1).toISOString() })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBe(r)
  })

  // --- 該当しないとき返さない方向（両方向のテスト） ---

  it('skip の予約は除外する（サーバーの需要計算と同じ規則）', () => {
    const r = reservation({ skip: true, startAt: at(30) })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBeNull()
  })

  it('別チャンネル種別（sameTypeProgramIds に無い programId）は除外する', () => {
    const r = reservation({ programId: 999, startAt: at(30) })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBeNull()
  })

  it('窓の外（先読み時間を過ぎた予約）は除外する', () => {
    const r = reservation({ startAt: new Date(nowMs + interruptionLookaheadMs).toISOString() })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBeNull()
  })

  it('すでに始まっている予約（startAt < nowMs）は除外する', () => {
    const r = reservation({ startAt: at(-1) })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBeNull()
  })

  it('別サイトの同じ programId は取り違えない（programId は site スコープ）', () => {
    const r = reservation({ site: 'other-site', programId: 1, startAt: at(30) })
    expect(upcomingInterruptingReservation([r], 'default', sameType, nowMs)).toBeNull()
  })

  it('予約が無ければ null', () => {
    expect(upcomingInterruptingReservation([], 'default', sameType, nowMs)).toBeNull()
  })
})

describe('interruptionWarningMessage', () => {
  it('時刻を含み、断言せず条件付きの文言を返す', () => {
    const message = interruptionWarningMessage({ startAt: at(30) })
    expect(message).toContain('から録画予約があります')
    // 「中断されます」と断言しない --- 「不足すると」の条件が付くこと自体が
    // 主張の強さを制限する（issue #235 の「罠」）
    expect(message).toContain('チューナーが不足すると視聴は中断されます')
    expect(message).not.toMatch(/^視聴は中断されます/)
  })
})
