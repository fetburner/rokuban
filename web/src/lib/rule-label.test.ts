import { describe, expect, it } from 'vitest'

import type { Rule } from '@/api/generated'
import { ruleDisambiguator } from '@/lib/rule-label'

function rule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 1,
    name: 'ニュース録画ルール',
    enabled: true,
    priority: 0,
    keepOriginal: 'always',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('ruleDisambiguator', () => {
  it('名前が重複しないルールには何も返さない', () => {
    const rules = [
      rule({ id: 12, name: '平日のニュース' }),
      rule({ id: 34, name: '休日のニュース' }),
    ]
    const disambiguate = ruleDisambiguator(rules)

    expect(disambiguate(rules[0])).toBeUndefined()
    expect(disambiguate(rules[1])).toBeUndefined()
  })

  it('同名のルールにはそれぞれ一意な id の補助ラベルを返す', () => {
    const rules = [rule({ id: 12 }), rule({ id: 34 })]
    const disambiguate = ruleDisambiguator(rules)

    expect(rules.map((current) => disambiguate(current))).toEqual(['#12', '#34'])
  })

  it('3 本以上の同名ルールでも全員を区別する', () => {
    const rules = [rule({ id: 12 }), rule({ id: 34 }), rule({ id: 56 })]
    const disambiguate = ruleDisambiguator(rules)

    expect(rules.map((current) => disambiguate(current))).toEqual(['#12', '#34', '#56'])
  })

  it('渡した配列とは別オブジェクトでも同じ id なら引ける', () => {
    const rules = [rule({ id: 12 }), rule({ id: 34 })]
    const disambiguate = ruleDisambiguator(rules)

    expect(disambiguate({ ...rules[0] })).toBe('#12')
    expect(disambiguate({ ...rules[1] })).toBe('#34')
  })
})
