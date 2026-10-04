import { afterEach, describe, expect, it, vi } from 'vitest'
import vectorsJSON from '../../../testdata/playback-position-vectors.json?raw'

import {
  clearLegacyPlaybackPositions,
  cutMsToOriginalMs,
  effectivePlaybackRate,
  loadPlaybackRate,
  originalMsToCutMs,
  persistPlaybackPosition,
  playbackPositionWrite,
  playbackResumeSeconds,
  recordingFileURL,
  savePlaybackRate,
} from '@/lib/playback-position'

type Vector = { fromMs: number; toMs: number }
type Vectors = {
  ranges: { startMs: number; endMs: number }[]
  originalToCut: Vector[]
  cutToOriginal: Vector[]
  outsideOriginalToCut: Vector[]
}

const vectors = JSON.parse(vectorsJSON) as Vectors

afterEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
})

describe('カット版と原本の位置変換', () => {
  it('keep 区間が空なら原本の 0 を返す', () => {
    expect(cutMsToOriginalMs(5000, [])).toBe(0)
  })

  it('Go と共有するベクタで原本→カット、カット→原本を検証する', () => {
    for (const vector of vectors.originalToCut) {
      expect(originalMsToCutMs(vector.fromMs, vectors.ranges)).toBe(vector.toMs)
    }
    for (const vector of vectors.cutToOriginal) {
      expect(cutMsToOriginalMs(vector.fromMs, vectors.ranges)).toBe(vector.toMs)
    }
  })

  it('keep 外の原本位置は次の keep 区間の先頭へ寄せる', () => {
    for (const vector of vectors.outsideOriginalToCut) {
      expect(originalMsToCutMs(vector.fromMs, vectors.ranges)).toBe(vector.toMs)
    }
  })

  it('原本の再開 ms を再生中のカット版 seconds へ変換する', () => {
    expect(playbackResumeSeconds(15000, vectors.ranges)).toBe(10)
    expect(playbackResumeSeconds(30000, vectors.ranges)).toBe(20)
    expect(playbackResumeSeconds(undefined, vectors.ranges)).toBeNull()
  })

  it('再生位置は原本 ms で書き、cut の継ぎ目は次の keep 区間へ戻す', () => {
    expect(playbackPositionWrite(10, 60, false, vectors.ranges)).toEqual({ kind: 'put', positionMs: 20000 })
  })
})

describe('位置保存の終端判定', () => {
  it('先頭 2 秒未満は消し、終端 90% は確定後だけ視聴済みにする', () => {
    expect(playbackPositionWrite(1.9, 100, true)).toEqual({ kind: 'delete' })
    expect(playbackPositionWrite(90, 100, false)).toEqual({ kind: 'put', positionMs: 90000 })
    expect(playbackPositionWrite(89.9, 100, true)).toEqual({ kind: 'put', positionMs: 89900 })
    expect(playbackPositionWrite(90, 100, true)).toEqual({ kind: 'watched' })
  })

  it('古い localStorage 位置だけを消して端末ごとの再生速度は残す', () => {
    localStorage.setItem('rokuban:playback:7:h264', '123')
    localStorage.setItem('rokuban:playback:8:original', '456')
    localStorage.setItem('rokuban:playback-rate', '1.5')
    clearLegacyPlaybackPositions()
    expect(localStorage.getItem('rokuban:playback:7:h264')).toBeNull()
    expect(localStorage.getItem('rokuban:playback:8:original')).toBeNull()
    expect(loadPlaybackRate()).toBe(1.5)
  })
})

describe('再生状態 API', () => {
  it('位置を PUT、先頭位置を DELETE、視聴済みを PUT し pagehide は keepalive にする', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 204,
      statusText: 'No Content',
      headers: new Headers(),
    })
    vi.stubGlobal('fetch', fetchMock)

    await expect(persistPlaybackPosition(7, { kind: 'put', positionMs: 12_000 }, true)).resolves.toBe(true)
    await expect(persistPlaybackPosition(8, { kind: 'delete' })).resolves.toBe(true)
    await expect(persistPlaybackPosition(9, { kind: 'watched' })).resolves.toBe(true)

    expect(fetchMock.mock.calls).toEqual([
      ['/api/recordings/7/playback-position', expect.objectContaining({
        method: 'PUT',
        body: JSON.stringify({ positionMs: 12_000 }),
        keepalive: true,
      })],
      ['/api/recordings/8/playback-position', expect.objectContaining({ method: 'DELETE' })],
      ['/api/recordings/9/watched', expect.objectContaining({ method: 'PUT' })],
    ])
  })

  it('先行の位置 PUT が settle するまで watched を送らない', async () => {
    let release!: (r: Response) => void
    const fetchMock = vi
      .fn()
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { release = resolve }))
      .mockResolvedValue(new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)

    const put = persistPlaybackPosition(7, { kind: 'put', positionMs: 12_000 })
    const watched = persistPlaybackPosition(7, { kind: 'watched' })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(fetchMock).toHaveBeenCalledTimes(1)

    release(new Response(null, { status: 204 }))
    await expect(put).resolves.toBe(true)
    await expect(watched).resolves.toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock.mock.calls[1]![0]).toBe('/api/recordings/7/watched')
  })
})

describe('再生速度は端末ごとに 1 つ', () => {
  it('保存した速度を復元し、既定値はキーごと消す', () => {
    savePlaybackRate(1.5)
    expect(loadPlaybackRate()).toBe(1.5)
    expect(localStorage.getItem('rokuban:playback-rate')).toBe('1.5')
    savePlaybackRate(1)
    expect(localStorage.getItem('rokuban:playback-rate')).toBeNull()
    expect(loadPlaybackRate()).toBe(1)
  })

  it('0 以下・壊れた値は 1 倍に落とす', () => {
    for (const raw of ['0', '-1', 'fast', '', 'Infinity', 'NaN']) {
      localStorage.setItem('rokuban:playback-rate', raw)
      expect(loadPlaybackRate()).toBe(1)
    }
  })

  it('private mode で localStorage が例外でも既定値を使う', () => {
    const getSpy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied')
    })
    try {
      expect(loadPlaybackRate()).toBe(1)
    } finally {
      getSpy.mockRestore()
    }
  })

  it('ENDLIST 前のネイティブ HLS 録画再生だけ 1 倍にする', () => {
    expect(effectivePlaybackRate(1.5, true, true, Infinity)).toBe(1)
    expect(effectivePlaybackRate(1.5, true, true, 120)).toBe(1.5)
    expect(effectivePlaybackRate(1.5, true, false, Infinity)).toBe(1.5)
    expect(effectivePlaybackRate(1.5, false, true, Infinity)).toBe(1)
  })
})

describe('recordingFileURL', () => {
  it('原本は query 無し', () => {
    expect(recordingFileURL(3)).toBe('/api/media/recordings/3/file')
  })

  it('encoded は profile query を encode する', () => {
    expect(recordingFileURL(3, 'h264')).toBe('/api/media/recordings/3/file?profile=h264')
    expect(recordingFileURL(3, 'a b')).toBe('/api/media/recordings/3/file?profile=a%20b')
  })
})
