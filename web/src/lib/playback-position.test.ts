import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  loadPlaybackPosition,
  loadPlaybackRate,
  playbackStorageKey,
  recordingFileURL,
  savePlaybackPosition,
  savePlaybackRate,
  shouldSavePlaybackPosition,
} from '@/lib/playback-position'

afterEach(() => {
  localStorage.clear()
})

describe('playbackStorageKey', () => {
  it('録画 ID とプロファイルでキーを分ける', () => {
    expect(playbackStorageKey(1, 'h264')).toBe('rokuban:playback:1:h264')
    expect(playbackStorageKey(1, 'h265')).not.toBe(playbackStorageKey(1, 'h264'))
    expect(playbackStorageKey(2, 'h264')).not.toBe(playbackStorageKey(1, 'h264'))
  })
})

describe('load/savePlaybackRate', () => {
  it('保存した速度を復元する（録画をまたいで 1 つ）', () => {
    savePlaybackRate(1.5)
    expect(loadPlaybackRate()).toBe(1.5)
    // キーに録画 ID を含めない（含めた実装ならこの 1 つのキーには入らない）
    expect(localStorage.getItem('rokuban:playback-rate')).toBe('1.5')
  })

  it('保存が無ければ 1 倍', () => {
    expect(loadPlaybackRate()).toBe(1)
  })

  it('1 倍はキーごと消す（既定値の行を作らない）', () => {
    savePlaybackRate(2)
    savePlaybackRate(1)
    expect(localStorage.getItem('rokuban:playback-rate')).toBeNull()
    expect(loadPlaybackRate()).toBe(1)
  })

  it('選択肢に無い値・壊れた値は 1 倍に落とす', () => {
    for (const raw of ['3', '1.1', '0', '-1', 'fast', '']) {
      localStorage.setItem('rokuban:playback-rate', raw)
      expect(loadPlaybackRate()).toBe(1)
    }
  })
})

describe('load/savePlaybackPosition', () => {
  it('保存した位置を復元する', () => {
    savePlaybackPosition(7, 'h264', 123)
    expect(loadPlaybackPosition(7, 'h264')).toBe(123)
  })

  it('プロファイルが違えば別位置', () => {
    savePlaybackPosition(7, 'h264', 10)
    savePlaybackPosition(7, 'h265', 50)
    expect(loadPlaybackPosition(7, 'h264')).toBe(10)
    expect(loadPlaybackPosition(7, 'h265')).toBe(50)
  })

  it('先頭付近は保存しない', () => {
    savePlaybackPosition(7, 'h264', 1)
    expect(loadPlaybackPosition(7, 'h264')).toBeNull()
  })

  it('終端付近はクリアする', () => {
    savePlaybackPosition(7, 'h264', 100)
    expect(loadPlaybackPosition(7, 'h264')).toBe(100)
    savePlaybackPosition(7, 'h264', 296, 300)
    expect(loadPlaybackPosition(7, 'h264')).toBeNull()
  })

  it('未保存は null', () => {
    expect(loadPlaybackPosition(99, 'h264')).toBeNull()
  })
})

describe('shouldSavePlaybackPosition', () => {
  it('未保存（null）からは常に保存する', () => {
    expect(shouldSavePlaybackPosition(null, 0.4)).toBe(true)
    expect(shouldSavePlaybackPosition(null, 10.9)).toBe(true)
  })

  it('同じ秒の間は保存しない', () => {
    expect(shouldSavePlaybackPosition(10, 10.1)).toBe(false)
    expect(shouldSavePlaybackPosition(10, 10.5)).toBe(false)
    expect(shouldSavePlaybackPosition(10, 10.999)).toBe(false)
  })

  it('秒が変わったら保存する', () => {
    expect(shouldSavePlaybackPosition(10, 11.0)).toBe(true)
    expect(shouldSavePlaybackPosition(10, 9.9)).toBe(true)
  })
})

describe('private mode 等で localStorage が例外を投げる場合', () => {
  it('getItem/setItem/removeItem が例外を投げても、読みは既定値・書きは無音で落ちる', () => {
    const getItemSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied')
    })
    const removeItemSpy = vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadPlaybackRate()).toBe(1)
      expect(() => savePlaybackRate(1.5)).not.toThrow()
      expect(loadPlaybackPosition(7, 'h264')).toBeNull()
      expect(() => savePlaybackPosition(7, 'h264', 123)).not.toThrow()
      // 終端付近（removeItem を叩く分岐）も例外を外に漏らさない
      expect(() => savePlaybackPosition(7, 'h264', 296, 300)).not.toThrow()
    } finally {
      getItemSpy.mockRestore()
      setItemSpy.mockRestore()
      removeItemSpy.mockRestore()
    }
  })
})

describe('recordingFileURL', () => {
  it('原本は query 無し', () => {
    expect(recordingFileURL(3)).toBe('/api/recordings/3/file')
  })

  it('encoded は profile query', () => {
    expect(recordingFileURL(3, 'h264')).toBe('/api/recordings/3/file?profile=h264')
  })

  it('プロファイル名を encode する', () => {
    expect(recordingFileURL(3, 'a b')).toBe('/api/recordings/3/file?profile=a%20b')
  })
})
