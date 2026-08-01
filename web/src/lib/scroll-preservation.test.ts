import { describe, expect, it } from 'vitest'

import { findAnchorProgramId, type AnchorCandidate } from '@/lib/scroll-preservation'

describe('findAnchorProgramId', () => {
  it('sticky 要素が無い（stickyBottomPx 省略）場合、viewport 上端（y=0）より下に上端がある最初の行を返す', () => {
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: -80, bottom: -10 }, // 完全に隠れている（viewport より上）
      { programId: 2, top: -10, bottom: 30 }, // 上端はまだ viewport 上端より上
      { programId: 3, top: 30, bottom: 70 },
    ]
    expect(findAnchorProgramId(candidates)).toBe(3)
  })

  it('sticky の裏に上端が隠れている行は選ばない（下端だけ覗いていても不可）', () => {
    // sticky の下端が 149px の場合、programId 2 は下端（160）が 149 を超えて
    // 覗いているが、上端（100）はまだ裏に隠れている --- ユーザーには「次の行に
    // 覆いかぶさられた行」にしか見えないので、これをアンカーに選んではならない。
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: -50, bottom: 20 },
      { programId: 2, top: 100, bottom: 160 },
      { programId: 3, top: 160, bottom: 230 },
    ]
    expect(findAnchorProgramId(candidates, 149)).toBe(3)
  })

  it('sticky の下端より下に上端がある行はそのまま選ぶ', () => {
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: 100, bottom: 160 }, // 上端が sticky の裏（149 より上）
      { programId: 2, top: 149, bottom: 220 }, // 上端がちょうど sticky の下端
      { programId: 3, top: 220, bottom: 300 },
    ]
    expect(findAnchorProgramId(candidates, 149)).toBe(2)
  })

  it('全行が sticky の裏に隠れきっていれば null を返す', () => {
    const candidates: AnchorCandidate[] = [
      { programId: 1, top: -200, bottom: -160 },
      { programId: 2, top: 50, bottom: 120 },
    ]
    expect(findAnchorProgramId(candidates, 149)).toBeNull()
  })

  it('候補が空なら null を返す', () => {
    expect(findAnchorProgramId([], 149)).toBeNull()
  })
})
