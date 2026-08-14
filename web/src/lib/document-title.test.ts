import { describe, expect, it } from 'vitest'

import { pageTitle } from './document-title'

describe('pageTitle', () => {
  it('画面名の後ろに区切りと製品名（録番）を付ける', () => {
    expect(pageTitle('番組')).toBe('番組 · 録番')
  })

  it('英語表記（Rokuban）ではなく UI 表記（録番）に揃える', () => {
    expect(pageTitle('ホーム')).not.toContain('Rokuban')
    expect(pageTitle('ホーム')).toContain('録番')
  })
})
