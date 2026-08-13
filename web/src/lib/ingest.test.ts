import { describe, expect, it } from 'vitest'

import type { Recording } from '@/api/generated'
import { hasLiveIngestProgress, ingestDisplay, ingestStaleAfterMs } from '@/lib/ingest'

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

  // サーバーは録画中の録画に state='unknown' を返す（record が finished でない）
  // が、UI 側でも二重に落とす。万一 pending が来ても「取り込み待ち」とは言わない。
  it('録画中は取り込みの状態を出さない（まだ始まらないのが正常）', () => {
    const rec = recording({ status: 'recording', ingest: { state: 'pending' } })
    expect(ingestDisplay(rec, now)).toBeUndefined()
  })

  // ingest ジョブが一度も投入されない録画。サーバーが state='unknown' を返す
  // ことは internal/api の TestListRecordingsIngestNoJobComing が固定している。
  // ここで「取り込み待ち」を出すと、来ない未来を UI が断定することになる。
  it('failed / canceled の録画には取り込みの状態を出さない', () => {
    expect(
      ingestDisplay(recording({ status: 'failed', ingest: { state: 'unknown' } }), now),
    ).toBeUndefined()
    expect(
      ingestDisplay(recording({ status: 'canceled', ingest: { state: 'unknown' } }), now),
    ).toBeUndefined()
  })

  it('ingest が無い（古い API）ときは推測で埋めない', () => {
    expect(ingestDisplay(recording(), now)).toBeUndefined()
  })

  it('unknown は何も出さない（言えることが無い）', () => {
    expect(ingestDisplay(recording({ ingest: { state: 'unknown' } }), now)).toBeUndefined()
  })
})

describe('hasLiveIngestProgress', () => {
  const transferring = (observedAgoMs: number) =>
    recording({
      ingest: {
        state: 'transferring',
        writtenBytes: 1,
        observedAt: new Date(now - observedAgoMs).toISOString(),
      },
    })

  it('進捗が新しい転送中だけ真になる', () => {
    expect(hasLiveIngestProgress(transferring(1_000), now)).toBe(true)
  })

  // 停滞（River のバックオフ待ち / discard 後の record_sweep 待ち）を真のままに
  // すると 5 秒ポーリングが恒久的に続く。再開すれば observedAt が新しくなって
  // 自動的に真へ戻る（自己回復）ので、ここで落としても取りこぼしにはならない。
  it('停滞した転送中は偽になり、進捗が戻れば再び真になる', () => {
    expect(hasLiveIngestProgress(transferring(ingestStaleAfterMs + 1), now)).toBe(false)
    expect(hasLiveIngestProgress(transferring(ingestStaleAfterMs + 1), now - ingestStaleAfterMs)).toBe(
      true,
    )
  })

  // pending には「失敗し続けている ingest」も落ちる（権限不足で MkdirAll に
  // 失敗すると進捗行が書かれる前に落ちる。issue #211 で実際に起きた壊れ方）。
  it('取り込み待ちは偽になる（いつ始まるか分からないものを 5 秒で叩かない）', () => {
    expect(hasLiveIngestProgress(recording({ ingest: { state: 'pending' } }), now)).toBe(false)
  })

  // ingest ジョブが投入されない録画。サーバーは state='unknown' を返す
  // （internal/api の TestListRecordingsIngestNoJobComing がその対応を固定して
  // いる）。ここが真になると失敗録画 1 件でポーリングが永久に続く。
  it('取り込みが来ない録画（failed / canceled / 録画中）は偽になる', () => {
    expect(
      hasLiveIngestProgress(recording({ status: 'failed', ingest: { state: 'unknown' } }), now),
    ).toBe(false)
    expect(
      hasLiveIngestProgress(recording({ status: 'canceled', ingest: { state: 'unknown' } }), now),
    ).toBe(false)
    expect(
      hasLiveIngestProgress(recording({ status: 'recording', ingest: { state: 'unknown' } }), now),
    ).toBe(false)
    // 万一サーバーが録画中に pending を返しても、表示と同じ判定を通すので偽。
    expect(
      hasLiveIngestProgress(recording({ status: 'recording', ingest: { state: 'pending' } }), now),
    ).toBe(false)
  })

  it('取り込み済み・ingest 無しは偽になる', () => {
    expect(hasLiveIngestProgress(recording({ ingest: { state: 'committed' } }), now)).toBe(false)
    expect(hasLiveIngestProgress(recording(), now)).toBe(false)
  })
})
