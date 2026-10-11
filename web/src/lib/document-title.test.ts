import { describe, expect, it } from 'vitest'

import { pageTitle } from './document-title'

describe('pageTitle', () => {
  it('画面名の後ろに区切りと製品名（Rokuban）を付ける', () => {
    expect(pageTitle('番組')).toBe('番組 · Rokuban')
  })
})
