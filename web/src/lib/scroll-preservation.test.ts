import { describe, expect, it } from 'vitest'

import { scrollAdjustmentForPrepend } from '@/lib/scroll-preservation'

describe('scrollAdjustmentForPrepend', () => {
  it('挿入で高さが増えたぶんを正の補正量として返す', () => {
    expect(scrollAdjustmentForPrepend(1000, 1600)).toBe(600)
  })

  it('高さが変わらなければ補正しない（0）', () => {
    expect(scrollAdjustmentForPrepend(1000, 1000)).toBe(0)
  })

  it('高さが減っても差分をそのまま返す（呼び出し側が正負の意味を負う）', () => {
    expect(scrollAdjustmentForPrepend(1000, 400)).toBe(-600)
  })
})
