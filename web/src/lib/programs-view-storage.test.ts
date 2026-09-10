import { afterEach, describe, expect, it, vi } from 'vitest'

import { loadProgramsView, saveProgramsView } from '@/lib/programs-view-storage'

const KEY = 'rokuban:programs:view'

afterEach(() => {
  localStorage.clear()
})

describe('load/saveProgramsView', () => {
  it('保存した表示形式を復元する', () => {
    saveProgramsView('grid')

    expect(loadProgramsView()).toBe('grid')

    saveProgramsView('list')
    expect(loadProgramsView()).toBe('list')
  })

  it('保存が無ければ undefined', () => {
    expect(loadProgramsView()).toBeUndefined()
  })

  it('不正な保存値は undefined として扱う', () => {
    localStorage.setItem(KEY, 'calendar')

    expect(loadProgramsView()).toBeUndefined()
  })

  it('private mode 等で getItem/setItem が例外を投げても、読み書きは無音で既定値へ戻る', () => {
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadProgramsView()).toBeUndefined()
      expect(() => saveProgramsView('grid')).not.toThrow()
    } finally {
      getItemSpy.mockRestore()
      setItemSpy.mockRestore()
    }
  })
})
