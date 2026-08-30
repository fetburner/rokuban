/**
 * ルール一覧の 1 行に出す条件の要約（純関数）。
 *
 * `rules.tsx` から切り出しているのは 2 つ理由がある。1 つは単体テストのため
 * （React に依存しない）。もう 1 つは lint 対策で、コンポーネントファイルから
 * 非コンポーネントの named export を増やすと oxlint の
 * `react(only-export-components)` warning が増える（CLAUDE.md の
 * 「oxlint の既存 warning 3 件を増やさない」に抵触する）。
 */

import type { Rule, RuleTextMatch, RuleTimeWindow } from '@/api/generated'
import { formatDate, formatDuration } from '@/lib/format'
import { allWeekdays, genreCodeLabel, hasWeekday, secToTimeValue, weekdayLabels } from '@/lib/program-search'

const textTargetSummaryLabels: Record<string, string> = {
  name: '番組名',
  description: '概要',
  extended: '詳細',
}

function textMatchSummary(m: RuleTextMatch): string {
  const label = textTargetSummaryLabels[m.target] ?? m.target
  const quoted = `「${m.value}」`
  if (m.mode === 'regex') {
    return m.negate ? `${label}が${quoted}にマッチしない` : `${label}が${quoted}にマッチ`
  }
  return m.negate ? `${label}に${quoted}を含まない` : `${label}に${quoted}を含む`
}

/**
 * weekdayRangeLabel は曜日ビットマスクを短いラベルにする。
 *
 * 全曜日は「毎日」、連続した範囲（月〜金 等）は「開始〜終了」、それ以外
 * （飛び石）は個別に列挙する。一覧の 1 行に収めるための要約であり、
 * `ConditionFields` の曜日チップ（個別選択）とは別の表現でよい。
 */
function weekdayRangeLabel(weekdays: number): string {
  if (weekdays === allWeekdays) return '毎日'
  const indices: number[] = []
  for (let i = 0; i < weekdayLabels.length; i++) {
    if (hasWeekday(weekdays, i)) indices.push(i)
  }
  if (indices.length === 0) return '曜日未設定'
  const isContiguous = indices.every((v, i) => i === 0 || v === indices[i - 1] + 1)
  if (isContiguous && indices.length > 1) {
    return `${weekdayLabels[indices[0]]}〜${weekdayLabels[indices[indices.length - 1]]}`
  }
  return indices.map((i) => weekdayLabels[i]).join('・')
}

function timeWindowSummary(t: RuleTimeWindow): string {
  return `${weekdayRangeLabel(t.weekdays)} ${secToTimeValue(t.startSec)}–${secToTimeValue(t.endSec)}`
}

/**
 * summarizeRuleConditions はルール一覧の 1 行に出す条件の要約。
 *
 * 空配列は「条件が 1 つも無い」ことを示す（呼び出し側が「すべての番組に
 * マッチする」という危険な状態として表示する）。次元ごとに 1 つの短い文字列に
 * まとめ、詳細（大文字小文字区別・サービス名など）は割愛する —— 一覧は
 * 「どのルールがどれか区別する」ためのものであり、編集フォーム
 * （`ConditionFields`）ほどの精度は要らない。サービスは名前解決に
 * `useListServices` が要るため、ここでは件数だけ出す。
 */
export function summarizeRuleConditions(rule: Rule): string[] {
  const parts: string[] = []

  for (const m of rule.textMatches ?? []) parts.push(textMatchSummary(m))

  if (rule.services && rule.services.length > 0) {
    parts.push(`チャンネル ${rule.services.length} 件`)
  }

  if (rule.channelTypes && rule.channelTypes.length > 0) {
    parts.push(rule.channelTypes.join('/'))
  }

  if (rule.genres && rule.genres.length > 0) {
    parts.push([...rule.genres].sort((a, b) => a - b).map(genreCodeLabel).join('/'))
  }

  for (const t of rule.times ?? []) parts.push(timeWindowSummary(t))

  if (rule.isFree === true) parts.push('無料のみ')
  else if (rule.isFree === false) parts.push('有料のみ')

  if (rule.durationMinMs != null && rule.durationMaxMs != null) {
    parts.push(`${formatDuration(rule.durationMinMs)}〜${formatDuration(rule.durationMaxMs)}`)
  } else if (rule.durationMinMs != null) {
    parts.push(`${formatDuration(rule.durationMinMs)}以上`)
  } else if (rule.durationMaxMs != null) {
    parts.push(`${formatDuration(rule.durationMaxMs)}以下`)
  }

  if (rule.periodStartAt != null && rule.periodEndAt != null) {
    parts.push(`${formatDate(rule.periodStartAt)}〜${formatDate(rule.periodEndAt)}`)
  } else if (rule.periodStartAt != null) {
    parts.push(`${formatDate(rule.periodStartAt)}から`)
  } else if (rule.periodEndAt != null) {
    parts.push(`${formatDate(rule.periodEndAt)}まで`)
  }

  return parts
}
