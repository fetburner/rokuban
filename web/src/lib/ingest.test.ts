import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { ingestDisplay, ingestStaleAfterMs, isIngestInFlight } from '@/lib/ingest'

const now = Date.parse('2026-01-01T12:00:00Z')

function recording(overrides: Partial<Recording> = {}): Recording {
  return {
    id: 1,
    site: 'default',
    source: 'manual',
    serviceName: 'ＯＨＫ',
    channelType: 'GR',
    channel: '27',
    networkId: 32678,
    serviceId: 5168,
    eventId: 1,
    title: '録画',
    startAt: '2026-01-01T10:00:00Z',
    durationMs: 1_800_000,
    status: 'finished',
    createdAt: '2026-01-01T10:30:00Z',
    ...overrides,
  }
}

describe('ingestDisplay', () => {
  it('取り込み済みで原本もある録画には何も出さない', () => {
    const rec = recording({ ingest: { state: 'committed' }, sizeBytes: 1024 })
    expect(ingestDisplay(rec, now)).toBeUndefined()
  })

  // issue #211: 「まだ取り込めていない」と「取り込んだ後に消した」は
  // sizeBytes の省略という同じ形を取るので、ingest.state でしか分けられない。
  it('取り込み済みだが原本が無い録画は「原本削除済み」として出す', () => {
    const rec = recording({ ingest: { state: 'committed' } })
    expect(ingestDisplay(rec, now)).toEqual({ kind: 'originalDeleted' })
  })

  it('未 ingest（pending）は「原本削除済み」ではなく取り込み待ちとして出す', () => {
    const rec = recording({ ingest: { state: 'pending' } })
    expect(ingestDisplay(rec, now)).toEqual({ kind: 'pending' })
  })

  it('転送中は分母があれば % を出す', () => {
    const rec = recording({
      ingest: {
        state: 'transferring',
        writtenBytes: 250,
        expectedBytes: 1000,
        observedAt: new Date(now - 1000).toISOString(),
      },
    })
    expect(ingestDisplay(rec, now)).toEqual({
      kind: 'transferring',
      writtenBytes: 250,
      expectedBytes: 1000,
      percent: 25,
      stale: false,
    })
  })

  it('分母が無ければ % をでっち上げない', () => {
    const rec = recording({
      ingest: {
        state: 'transferring',
        writtenBytes: 250,
        observedAt: new Date(now - 1000).toISOString(),
      },
    })
    const got = ingestDisplay(rec, now)
    expect(got).toMatchObject({ kind: 'transferring', writtenBytes: 250 })
    expect(got && 'percent' in got ? got.percent : 'missing').toBeUndefined()
  })

  it('分母が 0 でも 0 除算にしない', () => {
    const rec = recording({
      ingest: { state: 'transferring', writtenBytes: 0, expectedBytes: 0 },
    })
    const got = ingestDisplay(rec, now)
    expect(got && 'percent' in got ? got.percent : 'missing').toBeUndefined()
  })

  it('分母を超えて書けていても 100% で頭打ちにする', () => {
    const rec = recording({
      ingest: { state: 'transferring', writtenBytes: 1200, expectedBytes: 1000 },
    })
    expect(ingestDisplay(rec, now)).toMatchObject({ percent: 100 })
  })

  it('観測時刻が古ければ停滞と判定する', () => {
    const fresh = recording({
      ingest: {
        state: 'transferring',
        writtenBytes: 1,
        observedAt: new Date(now - ingestStaleAfterMs).toISOString(),
      },
    })
    expect(ingestDisplay(fresh, now)).toMatchObject({ stale: false })

    const stale = recording({
      ingest: {
        state: 'transferring',
        writtenBytes: 1,
        observedAt: new Date(now - ingestStaleAfterMs - 1).toISOString(),
      },
    })
    expect(ingestDisplay(stale, now)).toMatchObject({ stale: true })
  })

  it('録画中は取り込みの状態を出さない（まだ始まらないのが正常）', () => {
    const rec = recording({ status: 'recording', ingest: { state: 'pending' } })
    expect(ingestDisplay(rec, now)).toBeUndefined()
  })

  it('ingest が無い（古い API）ときは推測で埋めない', () => {
    expect(ingestDisplay(recording(), now)).toBeUndefined()
  })

  it('unknown は何も出さない（言えることが無い）', () => {
    expect(ingestDisplay(recording({ ingest: { state: 'unknown' } }), now)).toBeUndefined()
  })
})

describe('isIngestInFlight', () => {
  it('取り込みが終わっていない録画だけ真になる', () => {
    expect(isIngestInFlight(recording({ ingest: { state: 'pending' } }))).toBe(true)
    expect(
      isIngestInFlight(recording({ ingest: { state: 'transferring', writtenBytes: 1 } })),
    ).toBe(true)
    // 録画中は取り込みがこれから始まる（画面に追随させたい）。
    expect(isIngestInFlight(recording({ status: 'recording', ingest: { state: 'unknown' } }))).toBe(
      true,
    )
    expect(isIngestInFlight(recording({ ingest: { state: 'committed' } }))).toBe(false)
    expect(isIngestInFlight(recording({ ingest: { state: 'unknown' } }))).toBe(false)
    expect(isIngestInFlight(recording())).toBe(false)
  })
})
