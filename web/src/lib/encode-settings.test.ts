import { describe, expect, it } from 'vitest'

import {
  encodeSettingsError,
  keepOriginalLabel,
  toggleProfile,
} from './encode-settings'

describe('encodeSettingsError', () => {
  it('until_encoded でプロファイル空ならエラー', () => {
    expect(encodeSettingsError('until_encoded', [])).toMatch(/プロファイル/)
  })

  it('until_encoded でプロファイルありなら通る', () => {
    expect(encodeSettingsError('until_encoded', ['h264'])).toBeUndefined()
  })

  it('always ならプロファイル空でも通る', () => {
    expect(encodeSettingsError('always', [])).toBeUndefined()
  })
})

describe('toggleProfile', () => {
  it('未選択なら末尾に足す', () => {
    expect(toggleProfile(['h264'], 'hevc')).toEqual(['h264', 'hevc'])
  })

  it('選択済みなら外す', () => {
    expect(toggleProfile(['h264', 'hevc'], 'h264')).toEqual(['hevc'])
  })
})

describe('keepOriginalLabel', () => {
  it('ラベルを返す', () => {
    expect(keepOriginalLabel('always')).toBe('常に保持')
    expect(keepOriginalLabel('until_encoded')).toBe('エンコード後に削除')
  })
})
