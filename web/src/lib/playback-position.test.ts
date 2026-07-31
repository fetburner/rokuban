import { afterEach, describe, expect, it } from 'vitest'

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
