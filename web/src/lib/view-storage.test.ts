import { afterEach, describe, expect, it, vi } from 'vitest'

import { loadPreferredView, savePreferredView, type PreferredView } from '@/lib/view-storage'

const KEY = 'rokuban:programs:view'

afterEach(() => {
  localStorage.clear()
})

describe('load/savePreferredView', () => {
  it('保存した表示形式を復元する', () => {
    savePreferredView('grid')

    expect(loadPreferredView()).toBe('grid')

    savePreferredView('list')
    expect(loadPreferredView()).toBe('list')
  })

  it('保存が無ければ undefined', () => {
    expect(loadPreferredView()).toBeUndefined()
  })

  it('不正な保存値は undefined として扱う', () => {
    localStorage.setItem(KEY, 'calendar')

    expect(loadPreferredView()).toBeUndefined()
  })

  it('不正な値は保存しない', () => {
    savePreferredView('calendar' as PreferredView)

    expect(localStorage.getItem(KEY)).toBeNull()
    expect(loadPreferredView()).toBeUndefined()
  })

  it('private mode 等で getItem/setItem が例外を投げても、読み書きは無音で既定値へ戻る', () => {
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadPreferredView()).toBeUndefined()
      expect(() => savePreferredView('grid')).not.toThrow()
    } finally {
      getItemSpy.mockRestore()
      setItemSpy.mockRestore()
    }
  })
})
