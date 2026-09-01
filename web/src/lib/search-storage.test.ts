import { afterEach, describe, expect, it, vi } from 'vitest'

import { loadLastSearchConditions, saveLastSearchConditions } from '@/lib/search-storage'

const KEY = 'rokuban:search:last'

afterEach(() => {
  localStorage.clear()
})

describe('load/saveLastSearchConditions', () => {
  it('保存した条件を復元する', () => {
    saveLastSearchConditions({
      textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }],
      genres: [7],
    })

    expect(loadLastSearchConditions()).toEqual({
      textMatches: [
        // openapi の既定（caseSensitive / negate）はスキーマ側で埋まる
        { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: false, negate: false },
      ],
      genres: [7],
    })
  })

  it('保存が無ければ undefined', () => {
    expect(loadLastSearchConditions()).toBeUndefined()
  })

  it('条件なし（空）はキーごと消す', () => {
    saveLastSearchConditions({ genres: [7] })
    saveLastSearchConditions({})
    expect(localStorage.getItem(KEY)).toBeNull()
    expect(loadLastSearchConditions()).toBeUndefined()
  })

  it('JSON として壊れていれば undefined', () => {
    localStorage.setItem(KEY, '{genres:')
    expect(loadLastSearchConditions()).toBeUndefined()
  })

  it('openapi の制約を外れた値は読まない（ジャンルは 0..15）', () => {
    // 保存後に openapi が変わった / 手で書き換えた場合。読むたびに検証する。
    localStorage.setItem(KEY, JSON.stringify({ genres: [99] }))
    expect(loadLastSearchConditions()).toBeUndefined()
    // 境界の内側は通る（両方向を見る）
    localStorage.setItem(KEY, JSON.stringify({ genres: [15] }))
    expect(loadLastSearchConditions()).toEqual({ genres: [15] })
  })

  it('条件の形を外れた値（テキスト条件の target が未知）は読まない', () => {
    localStorage.setItem(
      KEY,
      JSON.stringify({ textMatches: [{ target: 'summary', mode: 'keyword', value: 'x' }] }),
    )
    expect(loadLastSearchConditions()).toBeUndefined()
  })

  it('private mode 等で getItem/setItem が例外を投げても、読みは既定値・書きは無音で落ちる', () => {
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadLastSearchConditions()).toBeUndefined()
      expect(() => saveLastSearchConditions({ genres: [7] })).not.toThrow()
    } finally {
      getItemSpy.mockRestore()
      setItemSpy.mockRestore()
    }
  })
})
