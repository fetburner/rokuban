import { describe, expect, it } from 'vitest'

import { findAnchorProgramId, type AnchorCandidate } from '@/lib/scroll-preservation'

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
