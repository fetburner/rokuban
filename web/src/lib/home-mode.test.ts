import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  HOME_MODE_STORAGE_KEY,
  parseHomeMode,
  readHomeModePreference,
  resolveHomeMode,
  saveHomeModePreference,
} from '@/lib/home-mode'

afterEach(() => {
  localStorage.removeItem(HOME_MODE_STORAGE_KEY)
  vi.restoreAllMocks()
})

describe('ホームのモード選択', () => {
  it('URL > localStorage > 既定「見る」の順で選ぶ', () => {
    expect(resolveHomeMode('ops', 'watch')).toBe('ops')
    expect(resolveHomeMode('watch', 'ops')).toBe('watch')
    expect(resolveHomeMode(undefined, 'ops')).toBe('ops')
    expect(resolveHomeMode(undefined, undefined)).toBe('watch')
  })

  it('未知の URL 値と保存値を無視する', () => {
    expect(parseHomeMode('admin')).toBeUndefined()
    expect(resolveHomeMode('unknown', 'admin')).toBe('watch')
    expect(resolveHomeMode('watch', 'admin')).toBe('watch')
  })

  it('保存値を読み込むたびに検証し、保存と読み取りの例外を握りつぶす', () => {
    localStorage.setItem(HOME_MODE_STORAGE_KEY, 'ops')
    expect(readHomeModePreference()).toBe('ops')

    localStorage.setItem(HOME_MODE_STORAGE_KEY, 'unknown')
    expect(readHomeModePreference()).toBeUndefined()

    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked')
    })
    expect(readHomeModePreference()).toBeUndefined()
    expect(() => saveHomeModePreference('watch')).not.toThrow()
  })
})
