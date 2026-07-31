import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  loadPlaybackPosition,
  playbackStorageKey,
  recordingFileURL,
  savePlaybackPosition,
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

describe('timeupdate 間引きと savePlaybackPosition の結合', () => {
  it('同一秒内の連続した timeupdate 相当の呼び出しで書き込みが 1 回に収まる', () => {
    // RecordingPlayer の onTimeUpdate と同じ手順（間引き判定 → 更新 → 保存）を
    // 4Hz の timeupdate 相当（0.25 秒間隔）でシミュレートする
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    let lastSavedSecond: number | null = null
    for (const t of [10.0, 10.25, 10.5, 10.75]) {
      if (shouldSavePlaybackPosition(lastSavedSecond, t)) {
        lastSavedSecond = Math.floor(t)
        savePlaybackPosition(7, 'h264', t)
      }
    }
    expect(setItemSpy).toHaveBeenCalledTimes(1)
    expect(loadPlaybackPosition(7, 'h264')).toBe(10)
    setItemSpy.mockRestore()
  })

  it('秒をまたぐと再び保存する', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    let lastSavedSecond: number | null = null
    for (const t of [10.0, 10.5, 11.0, 11.5]) {
      if (shouldSavePlaybackPosition(lastSavedSecond, t)) {
        lastSavedSecond = Math.floor(t)
        savePlaybackPosition(7, 'h264', t)
      }
    }
    expect(setItemSpy).toHaveBeenCalledTimes(2)
    expect(loadPlaybackPosition(7, 'h264')).toBe(11)
    setItemSpy.mockRestore()
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
