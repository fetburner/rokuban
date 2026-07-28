import { CircuitBreakerName } from '@/api/generated'

/**
 * breakerLabels は `internal/breaker` の識別子を「何が止まっているか」の日本語にする。
 *
 * 表示は `{label}が停止中` の形になるので、**止まっている操作**を名詞句で書く。
 * 「なぜ止まったか」は breakerReasons が別に持つ --- ラベルに理由を混ぜると
 * 「予約の全件消失疑いによる削除が停止中」のように原因と結果が逆に読める文になる
 * （止まっているのは削除であって、全件消失は止めた理由）。
 *
 * 値は将来 `internal/breaker` に識別子が増える可能性があるため網羅チェックはせず、
 * 未知の値は識別子そのものをフォールバック表示する。
 */
const breakerLabels: Record<CircuitBreakerName, string> = {
  [CircuitBreakerName.ruler_deletes]: 'ルール評価による予約の削除',
  [CircuitBreakerName.reconcile_total_loss]: 'mirakc の録画予定の削除',
}

/**
 * breakerReasons は「なぜ止まったか」。運用者が再開を押す前に何を確認すべきかが
 * ブレーカーごとに違うため、疑うべき原因まで書く（docs/operations.md §2）。
 */
const breakerReasons: Record<CircuitBreakerName, string> = {
  [CircuitBreakerName.ruler_deletes]:
    '1 回の評価で消える予約が多すぎます。EPG の一時的な欠損を疑ってください',
  [CircuitBreakerName.reconcile_total_loss]:
    '予約が 1 件も無いのに録画予定が残っています。予約が失われていないか確認してください',
}

/** describeBreakerName はブレーカー識別子を「何が止まっているか」の日本語にする。 */
export function describeBreakerName(name: string): string {
  return breakerLabels[name as CircuitBreakerName] ?? name
}

/**
 * describeBreakerReason はブレーカー識別子を「なぜ止まったか」の日本語にする。
 * 未知の識別子については説明できることが無いので空文字を返す（呼び出し側は出さない）。
 */
export function describeBreakerReason(name: string): string {
  return breakerReasons[name as CircuitBreakerName] ?? ''
}
