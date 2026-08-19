import { describe, expect, it } from 'vitest'

import { computeInitialTabClearanceScroll } from './tab-clearance'

describe('computeInitialTabClearanceScroll', () => {
  it('行が無ければ 0', () => {
    expect(computeInitialTabClearanceScroll([], 780)).toBe(0)
  })

  it('全行がタブより上にあれば 0', () => {
    const rows = [
      { top: 100, bottom: 157 },
      { top: 157, bottom: 214 },
    ]
    expect(computeInitialTabClearanceScroll(rows, 780)).toBe(0)
  })

  it('行が完全にタブの裏（top がタブ上端以上）なら対象にしない', () => {
    // 780 ちょうどから始まる行は「上端も隠れている」ので、ユーザーには
    // 最初から見えていない --- 切れて見える問題にはならない
    const rows = [{ top: 780, bottom: 837 }]
    expect(computeInitialTabClearanceScroll(rows, 780)).toBe(0)
  })

  it('上端は見えるが下端がタブに食い込む行があれば、その食い込み量を返す', () => {
    // issue #303 の再現値に近い形: 751-808 の行がタブ上端 779 に食い込む
    const rows = [{ top: 751, bottom: 808 }]
    expect(computeInitialTabClearanceScroll(rows, 779)).toBe(29)
  })

  it('複数行が該当する場合は最大の食い込み量を返す', () => {
    const rows = [
      { top: 751, bottom: 808 }, // 食い込み 29
      { top: 700, bottom: 790 }, // 食い込み 11
    ]
    expect(computeInitialTabClearanceScroll(rows, 779)).toBe(29)
  })

  it('ちょうどタブ上端に揃っている行（重なりゼロ）は対象にしない', () => {
    const rows = [{ top: 722, bottom: 779 }]
    expect(computeInitialTabClearanceScroll(rows, 779)).toBe(0)
  })
})
