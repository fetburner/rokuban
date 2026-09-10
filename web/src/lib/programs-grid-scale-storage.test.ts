import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  loadProgramsGridPxPerHour,
  saveProgramsGridPxPerHour,
} from '@/lib/programs-grid-scale-storage'

const KEY = 'rokuban:programs:grid-scale'

afterEach(() => {
  localStorage.clear()
})

describe('load/saveProgramsGridPxPerHour', () => {
  it('保存した 3 段階の縮尺を復元する', () => {
    saveProgramsGridPxPerHour(240)
    expect(loadProgramsGridPxPerHour()).toBe(240)

    saveProgramsGridPxPerHour(480)
    expect(loadProgramsGridPxPerHour()).toBe(480)

    saveProgramsGridPxPerHour(120)
    expect(loadProgramsGridPxPerHour()).toBe(120)
  })

  it('保存が無ければ undefined', () => {
    expect(loadProgramsGridPxPerHour()).toBeUndefined()
  })

  it('選択肢に無い保存値は undefined として扱う', () => {
    for (const value of ['0', '121', '960', 'NaN']) {
      localStorage.setItem(KEY, value)
      expect(loadProgramsGridPxPerHour()).toBeUndefined()
    }
  })

  it('private mode 等で getItem/setItem が例外を投げても、読み書きは無音で既定値へ戻る', () => {
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadProgramsGridPxPerHour()).toBeUndefined()
      expect(() => saveProgramsGridPxPerHour(240)).not.toThrow()
    } finally {
      getItemSpy.mockRestore()
      setItemSpy.mockRestore()
    }
  })
})
