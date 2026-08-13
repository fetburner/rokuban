import { describe, expect, it } from 'vitest'

import {
  interruptionQueryWindow,
  interruptionWarningMessage,
  upcomingInterruptingReservation,
  type InterruptingReservationCandidate,
} from '@/lib/live-interruption'

/**
 * testLookaheadMs は境界テストが使う先読み窓。**実装の定数
 * （`interruptionLookaheadMs`）を読まずリテラルで固定する** --- 定数と比較する
 * テストは定数を変えても通ってしまい何も主張しない（CLAUDE.md「実装の定数と
 * 比較するテストは何も主張していない」）。`upcomingInterruptingReservation` は
 * `lookaheadMs` を引数に取れるので、この値を明示的に渡して境界を固定する。
 */
const testLookaheadMs = 2 * 60 * 60 * 1000

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
    const r = reservation({ startAt: new Date(nowMs + testLookaheadMs - 1).toISOString() })
    expect(
      upcomingInterruptingReservation([r], 'default', sameType, nowMs, testLookaheadMs),
    ).toBe(r)
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
    const r = reservation({ startAt: new Date(nowMs + testLookaheadMs).toISOString() })
    expect(
      upcomingInterruptingReservation([r], 'default', sameType, nowMs, testLookaheadMs),
    ).toBeNull()
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

describe('interruptionQueryWindow', () => {
  const gridMs = 10 * 60_000
  const lookaheadMs = 2 * 60 * 60_000

  it('グリッド内で 30 秒（nowPlayingRefetchMs の tick 間隔）進めても窓の値が変わらない', () => {
    // グリッドの先頭ちょうどだと「たまたま境界に乗っていた」だけで通ってしまう
    // ことがあるため、先頭から 2 分進めた位置を基準にする
    const base = nowMs + 2 * 60_000
    const a = interruptionQueryWindow(base, lookaheadMs, gridMs)
    const b = interruptionQueryWindow(base + 30_000, lookaheadMs, gridMs)
    // react-query のクエリキーは値のハッシュで比較される（オブジェクト参照では
    // ない）ため、文字列として一致すれば同じキャッシュエントリに解決する
    expect(b).toEqual(a)
  })

  it('グリッドを跨ぐと窓は変わる（無限に固定されるわけではない）', () => {
    const base = Math.floor(nowMs / gridMs) * gridMs
    const beforeBoundary = interruptionQueryWindow(base + gridMs - 1, lookaheadMs, gridMs)
    const afterBoundary = interruptionQueryWindow(base + gridMs, lookaheadMs, gridMs)
    expect(afterBoundary).not.toEqual(beforeBoundary)
  })

  it('丸めた窓は常に実際の判定窓 [nowMs, nowMs + lookaheadMs) を包含する（境界含む）', () => {
    // グリッド内の複数点で確認する（切り捨ての余りが最大の位置＝境界直前を含む）
    for (const offsetMinutes of [0, 1, 5, 9, 9.9]) {
      const t = nowMs + offsetMinutes * 60_000
      const window = interruptionQueryWindow(t, lookaheadMs, gridMs)
      const startMs = new Date(window.start).getTime()
      const endMs = new Date(window.end).getTime()
      expect(startMs).toBeLessThanOrEqual(t)
      expect(endMs).toBeGreaterThanOrEqual(t + lookaheadMs)
    }
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
