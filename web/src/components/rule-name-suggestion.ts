import { weekdayRangeLabel } from '@/components/rule-condition-summary'
import {
  genreCodeLabel,
  secToTimeValue,
  type SearchDraft,
  type ServiceRefDraft,
} from '@/lib/program-search'

/**
 * suggestRuleName は新規ルールの名前候補を条件から導く。
 *
 * 正のキーワード条件があればそれを優先する。なければ解決できたサービス名・
 * ジャンル・最初の時間帯を補助候補にする。候補は表示時に導出し、呼び出し側が
 * ユーザーの入力を追従させる間だけ使う。
 */
export function suggestRuleName(
  draft: SearchDraft,
  serviceName: (ref: ServiceRefDraft) => string | undefined,
): string {
  const keywords = draft.textMatches
    .filter((match) => match.mode === 'keyword' && !match.negate && match.value.trim() !== '')
    .map((match) => match.value)

  if (keywords.length > 0) return keywords.join('・')

  const parts: string[] = []
  const onlyService = draft.services.length === 1 ? draft.services[0] : undefined
  if (onlyService !== undefined) {
    const name = serviceName(onlyService)
    if (name !== undefined && name.trim() !== '') parts.push(name)
  }
  if (draft.genres.length > 0) {
    parts.push([...draft.genres].sort((a, b) => a - b).map(genreCodeLabel).join('/'))
  }
  const firstTime = draft.times[0]
  if (firstTime !== undefined) {
    parts.push(
      `${weekdayRangeLabel(firstTime.weekdays)} ${secToTimeValue(firstTime.startSec)}–${secToTimeValue(firstTime.endSec)}`,
    )
  }

  return parts.join(' ')
}
