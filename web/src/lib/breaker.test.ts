import { describe, expect, it } from 'vitest'

import { describeBreakerName, describeBreakerReason } from '@/lib/breaker'

describe('describeBreakerName', () => {
  it('ruler_deletes を日本語ラベルにする', () => {
    expect(describeBreakerName('ruler_deletes')).toBe('ルール評価による予約の削除')
  })

  it('reconcile_total_loss を日本語ラベルにする', () => {
    expect(describeBreakerName('reconcile_total_loss')).toBe('録画予定の削除')
  })

  it('未知の識別子はそのまま返す（将来の追加に備えたフォールバック）', () => {
    expect(describeBreakerName('some_future_breaker')).toBe('some_future_breaker')
  })
})

describe('describeBreakerReason', () => {
  // 利用者向けの理由文に「DB を直接確認」のような内部実装の語を出さない
  // （issue #454）。
  it('delete_reconcile の理由文は DB という語を含まない', () => {
    expect(describeBreakerReason('delete_reconcile')).not.toMatch(/DB/)
  })
})
