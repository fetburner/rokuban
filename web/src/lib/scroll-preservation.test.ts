import { describe, expect, it } from 'vitest'

import {
  findAnchorProgramId,
  scrollAdjustmentToRestoreTop,
  shouldStopFollowing,
  type AnchorCandidate,
} from '@/lib/scroll-preservation'

describe('findAnchorProgramId', () => {
  it('先頭から見て最初に viewport 上端に見えている行（bottom > 0）の programId を返す', () => {
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: -80, bottom: -10 }, // 完全に隠れている（viewport より上）
      { programId: 2, top: -10, bottom: 30 }, // 下端だけ見えている
      { programId: 3, top: 30, bottom: 70 },
    ]
    expect(findAnchorProgramId(candidates)).toBe(2)
  })

  it('全行が viewport より上に隠れきっていれば null を返す', () => {
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: -200, bottom: -160 },
      { programId: 2, top: -160, bottom: -120 },
    ]
    expect(findAnchorProgramId(candidates)).toBeNull()
  })

  it('候補が空なら null を返す', () => {
    expect(findAnchorProgramId([])).toBeNull()
  })
})

describe('scrollAdjustmentToRestoreTop', () => {
  it('挿入でアンカーが下にずれたぶんを正の補正量として返す', () => {
    // 挿入前は top=100 にあった行が、挿入後は top=700 まで押し下げられた
    expect(scrollAdjustmentToRestoreTop(100, 700)).toBe(600)
  })

  it('位置が変わらなければ補正しない（0）', () => {
    expect(scrollAdjustmentToRestoreTop(100, 100)).toBe(0)
  })

  it('位置が上に動いても差分をそのまま返す（呼び出し側が正負の意味を負う）', () => {
    expect(scrollAdjustmentToRestoreTop(700, 100)).toBe(-600)
  })
})

describe('shouldStopFollowing', () => {
  const config = { maxCorrections: 5, maxElapsedMs: 1000 }

  it('補正回数・経過時間のどちらも上限未満なら追従を続ける（false）', () => {
    expect(shouldStopFollowing({ corrections: 1, elapsedMs: 10 }, config)).toBe(false)
  })

  it('補正回数が上限に達したら打ち切る（true）', () => {
    expect(shouldStopFollowing({ corrections: 5, elapsedMs: 10 }, config)).toBe(true)
  })

  it('経過時間が上限に達したら、補正回数が少なくても打ち切る（true）', () => {
    expect(shouldStopFollowing({ corrections: 1, elapsedMs: 1000 }, config)).toBe(true)
  })
})
