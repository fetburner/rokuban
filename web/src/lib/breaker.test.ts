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
  // （issue #454）。否定だけの assertion だと理由文が空文字に壊れても
  // 検出できない（レビュー指摘: 実際に空文字へ差し替えて緑のままだった）ので、
  // 新しい理由文をリテラルで固定する。
  it('delete_reconcile の理由文', () => {
    expect(describeBreakerReason('delete_reconcile')).toBe(
      '1 回のパスで物理削除される件数が多すぎます。大量削除の意図が正しいか確認してください（対象の内訳はこの画面では確認できません）',
    )
  })
})
