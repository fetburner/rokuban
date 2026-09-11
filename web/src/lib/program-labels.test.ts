import { describe, expect, it } from 'vitest'

import { programTitle } from '@/lib/program-labels'

describe('programTitle', () => {
  it('タイトルがあればそのまま返す', () => {
    expect(programTitle('ニュース7')).toBe('ニュース7')
  })

  it.each<[string, string | null | undefined]>([
    ['undefined', undefined],
    ['null', null],
    ['空文字列', ''],
  ])('%s は「番組名なし」と表示する', (_label, title) => {
    expect(programTitle(title)).toBe('（番組名なし）')
  })
})
