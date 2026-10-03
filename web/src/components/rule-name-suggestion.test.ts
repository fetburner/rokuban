import { describe, expect, it } from 'vitest'

import { suggestRuleName } from '@/components/rule-name-suggestion'
import { emptyDraft, type SearchDraft, type TextMatchDraft } from '@/lib/program-search'

function textMatch(overrides: Partial<TextMatchDraft>): TextMatchDraft {
  return {
    target: 'name',
    mode: 'keyword',
    value: '',
    caseSensitive: false,
    negate: false,
    ...overrides,
  }
}

const nhk = { networkId: 32736, serviceId: 1024 }

describe('suggestRuleName', () => {
  it('キーワードは trim し、重複を除いて出現順に連結する', () => {
    const draft: SearchDraft = {
      ...emptyDraft(),
      textMatches: [
        textMatch({ value: ' ガンダム' }),
        textMatch({ value: '水星' }),
        textMatch({ target: 'description', value: 'ガンダム ' }),
      ],
    }
    expect(suggestRuleName(draft, () => undefined)).toBe('ガンダム・水星')
  })

  it('正のキーワード条件を対象に関係なく出現順に連結し、他の条件より優先する', () => {
    const draft: SearchDraft = {
      ...emptyDraft(),
      textMatches: [
        textMatch({ target: 'name', value: 'ガンダム' }),
        textMatch({ target: 'description', mode: 'regex', value: 'Gundam' }),
        textMatch({ target: 'extended', value: '水星' }),
        textMatch({ target: 'name', value: '旧作', negate: true }),
      ],
      services: [nhk],
      genres: [3],
      times: [{ weekdays: 31, startSec: 68_400, endSec: 72_000 }],
    }

    expect(suggestRuleName(draft, () => 'NHK総合')).toBe('ガンダム・水星')
  })

  it('キーワードが無ければ解決したサービス・ジャンル・最初の時間帯を連結する', () => {
    const draft: SearchDraft = {
      ...emptyDraft(),
      services: [nhk],
      genres: [3, 0],
      times: [
        { weekdays: 31, startSec: 68_400, endSec: 72_000 },
        { weekdays: 127, startSec: 0, endSec: 3_600 },
      ],
    }

    expect(suggestRuleName(draft, () => 'NHK総合')).toBe(
      'NHK総合 ニュース・報道/ドラマ 月〜金 19:00–20:00',
    )
  })

  it('サービスが未解決なら局名を省き、サービス複数件は名前に含めない', () => {
    const draft: SearchDraft = { ...emptyDraft(), services: [nhk] }
    expect(suggestRuleName(draft, () => undefined)).toBe('')

    draft.services = [nhk, { networkId: 32737, serviceId: 1032 }]
    expect(suggestRuleName(draft, () => 'NHK総合')).toBe('')
  })

  it('除外・正規表現だけ、または条件なしでは候補を作らない', () => {
    const drafts: SearchDraft[] = [
      { ...emptyDraft(), textMatches: [textMatch({ value: '除外語', negate: true })] },
      { ...emptyDraft(), textMatches: [textMatch({ mode: 'regex', value: '.*' })] },
      emptyDraft(),
    ]

    for (const draft of drafts) {
      expect(suggestRuleName(draft, () => 'NHK総合')).toBe('')
    }
  })
})
