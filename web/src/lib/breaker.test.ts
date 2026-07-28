import { describe, expect, it } from 'vitest'

import { describeBreakerName } from '@/lib/breaker'

describe('describeBreakerName', () => {
  it('ruler_deletes を日本語ラベルにする', () => {
    expect(describeBreakerName('ruler_deletes')).toBe('ルール評価による予約の削除')
  })

  it('reconcile_total_loss を日本語ラベルにする', () => {
    expect(describeBreakerName('reconcile_total_loss')).toBe('mirakc の録画予定の削除')
  })

  it('未知の識別子はそのまま返す（将来の追加に備えたフォールバック）', () => {
    expect(describeBreakerName('some_future_breaker')).toBe('some_future_breaker')
  })
})
