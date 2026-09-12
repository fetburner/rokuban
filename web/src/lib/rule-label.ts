import type { Rule } from '@/api/generated'

/**
 * ruleDisambiguator は、名前が重複するルールにだけ `#<id>` の補助ラベルを与える。
 *
 * `rules.name` は利用者が付ける説明であり、一意性を要求しない。名前を DB の一意
 * 制約で縛ると、既存ルールの複製や名前の変更を不必要に拒否するため、区別は表示側
 * で行う。名前が重複していないルールには補助ラベルを返さず、通常の表示を短く保つ。
 * `Rule.id` は主キーなので、同名グループ内でも `#<id>` は必ず一意になる。
 *
 * 引き当ては `Rule.id` で行う。ルール一覧の再取得で別オブジェクトになっても、同じ
 * id のルールには同じラベルを返す。
 */
export function ruleDisambiguator(
  rules: readonly Rule[],
): (rule: Rule) => string | undefined {
  const groups = new Map<string, Rule[]>()
  for (const rule of rules) {
    const group = groups.get(rule.name)
    if (group) group.push(rule)
    else groups.set(rule.name, [rule])
  }

  const labelOf = new Map<number, string>()
  for (const group of groups.values()) {
    if (group.length <= 1) continue
    for (const rule of group) {
      labelOf.set(rule.id, `#${rule.id}`)
    }
  }

  return (rule) => labelOf.get(rule.id)
}
