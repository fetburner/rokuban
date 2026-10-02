import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { chooseHomeHero, homeNewArrivals, type HeroSource } from '@/lib/home-selection'

function recording(
  id: number,
  startAt: string,
  overrides: Partial<Recording> = {},
): Recording {
  return {
    id,
    site: 'default',
    source: 'manual',
    serviceName: 'テスト局',
    channelType: 'GR',
    channel: '27',
    networkId: 1,
    serviceId: 2,
    eventId: id,
    title: `録画 ${id}`,
    startAt,
    durationMs: 60_000,
    status: 'finished',
    keepOriginal: 'always',
    cmDetection: { state: 'disabled' },
    createdAt: startAt,
    sizeBytes: 100,
    ...overrides,
  }
}

const ok = (items: Recording[]): HeroSource => ({ pending: false, error: false, items })
const pending: HeroSource = { pending: true, error: false, items: [] }
const failed: HeroSource = { pending: false, error: true, items: [] }

describe('ホームの「次に見る 1 本」', () => {
  it('続きからの先頭を完了録画より優先する', () => {
    const continuation = recording(1, '2026-09-01T00:00:00Z', { resumePositionMs: 120_000 })
    const newest = recording(2, '2026-10-01T00:00:00Z')
    expect(chooseHomeHero(ok([continuation]), ok([newest]))).toEqual({
      recording: continuation,
      kind: 'continue',
    })
  })

  it('再生可能で未視聴のうち放送日時が最新の完了録画へフォールバックする', () => {
    const old = recording(1, '2026-09-01T00:00:00Z')
    const watched = recording(2, '2026-10-03T00:00:00Z', { watchedAt: '2026-10-04T00:00:00Z' })
    const latest = recording(3, '2026-10-02T00:00:00Z')
    const unavailable = recording(4, '2026-10-05T00:00:00Z', { sizeBytes: undefined })

    expect(chooseHomeHero(ok([]), ok([old, watched, latest, unavailable]))).toEqual({
      recording: latest,
      kind: 'unwatched',
    })
  })

  it('両クエリが解決するまで決めず、候補が無い場合は null にする', () => {
    expect(chooseHomeHero(pending, ok([]))).toBeUndefined()
    expect(chooseHomeHero(ok([]), pending)).toBeUndefined()
    expect(chooseHomeHero(ok([]), ok([]))).toBeNull()
  })

  it('取得失敗: 続きからがあれば完了側の失敗に依らず選び、確認できなければフォールバックしない', () => {
    const continuation = recording(1, '2026-09-01T00:00:00Z')
    const newest = recording(2, '2026-10-01T00:00:00Z')
    expect(chooseHomeHero(ok([continuation]), failed)).toEqual({ recording: continuation, kind: 'continue' })
    expect(chooseHomeHero(ok([]), failed)).toBeNull()
    expect(chooseHomeHero(failed, ok([newest]))).toBeNull()
  })

  it('ほかの新着を続きからの 2 件目以降、未視聴完了の順に並べ、重複を除く', () => {
    const hero = recording(1, '2026-10-01T00:00:00Z')
    const continuation = recording(2, '2026-09-01T00:00:00Z')
    const repeated = recording(3, '2026-08-01T00:00:00Z')
    const newest = recording(4, '2026-10-03T00:00:00Z')
    const rest = recording(5, '2026-10-02T00:00:00Z')

    expect(homeNewArrivals([hero, continuation, repeated], [newest, repeated, rest], hero.id, 6)).toEqual([
      continuation,
      repeated,
      newest,
      rest,
    ])
    expect(homeNewArrivals([hero, continuation, repeated], [newest, rest], hero.id, 1)).toEqual([
      continuation,
    ])
  })
})
