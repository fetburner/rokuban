import { describe, expect, it } from 'vitest'

import type { ProgramSearchMatch, Recording } from '@/api/generated'
import { collapseUpcoming, isPlayableRecording, nextEpisode } from '@/lib/series'

/** recording は判定に効く列だけを持つ行を作る。 */
function recording(over: Partial<Recording> & { id: number }): Recording {
  return {
    title: `番組${over.id}`,
    startAt: '2026-09-01T12:00:00Z',
    site: 'default',
    status: 'finished',
    ...over,
  } as Recording
}

describe('isPlayableRecording', () => {
  it('原本のサイズか encoded 派生物があれば再生できる', () => {
    expect(isPlayableRecording(recording({ id: 1, sizeBytes: 1024 }))).toBe(true)
    expect(
      isPlayableRecording(
        recording({ id: 2, encodedAssets: [{ profile: 'h264', sizeBytes: 1024 }] }),
      ),
    ).toBe(true)
  })

  it('どちらも無ければ再生できない', () => {
    expect(isPlayableRecording(recording({ id: 3 }))).toBe(false)
    // 空配列は「encoded が無い」（ごみ箱の録画は encodedAssets を省略する）。
    expect(isPlayableRecording(recording({ id: 4, encodedAssets: [] }))).toBe(false)
  })
})

describe('nextEpisode', () => {
  const origin = recording({ id: 1, startAt: '2026-09-01T12:00:00Z', sizeBytes: 1 })

  it('起点より後の最も早い回を返す', () => {
    const rows = [
      origin,
      recording({ id: 2, startAt: '2026-09-08T12:00:00Z', sizeBytes: 1 }),
      recording({ id: 3, startAt: '2026-09-15T12:00:00Z', sizeBytes: 1 }),
    ]
    expect(nextEpisode(rows, origin)?.id).toBe(2)
  })

  it('起点と同じ時刻の回は「次」にしない', () => {
    const rows = [
      origin,
      recording({ id: 2, startAt: origin.startAt, sizeBytes: 1 }),
      recording({ id: 3, startAt: '2026-09-08T12:00:00Z', sizeBytes: 1 }),
    ]
    expect(nextEpisode(rows, origin)?.id).toBe(3)
  })

  it('再生できない回を飛ばす', () => {
    // 開始時刻がずれて supersede されなかった failed 行（原本も encoded も無い）
    // を「次」にすると、押した先の再生が 404 になる。
    const rows = [
      origin,
      recording({ id: 2, startAt: '2026-09-08T12:00:00Z', status: 'failed' }),
      recording({ id: 3, startAt: '2026-09-15T12:00:00Z', sizeBytes: 1 }),
    ]
    expect(nextEpisode(rows, origin)?.id).toBe(3)
  })

  it('同じ時刻の候補が複数あれば起点と同じ site を優先する', () => {
    const rows = [
      origin,
      recording({ id: 2, startAt: '2026-09-08T12:00:00Z', site: 'other', sizeBytes: 1 }),
      recording({ id: 3, startAt: '2026-09-08T12:00:00Z', site: 'default', sizeBytes: 1 }),
    ]
    expect(nextEpisode(rows, origin)?.id).toBe(3)
  })

  it('同じ site が無ければ id の小さい方を選ぶ', () => {
    const rows = [
      origin,
      recording({ id: 7, startAt: '2026-09-08T12:00:00Z', site: 'a', sizeBytes: 1 }),
      recording({ id: 4, startAt: '2026-09-08T12:00:00Z', site: 'b', sizeBytes: 1 }),
    ]
    expect(nextEpisode(rows, origin)?.id).toBe(4)
  })

  it('後の回が無ければ undefined', () => {
    const rows = [origin, recording({ id: 2, startAt: '2026-08-01T12:00:00Z', sizeBytes: 1 })]
    expect(nextEpisode(rows, origin)).toBeUndefined()
  })

  it('入力の並びを変えない', () => {
    const rows = [
      recording({ id: 9, startAt: '2026-09-08T12:00:00Z', site: 'a', sizeBytes: 1 }),
      recording({ id: 8, startAt: '2026-09-08T12:00:00Z', site: 'b', sizeBytes: 1 }),
    ]
    nextEpisode(rows, origin)
    expect(rows.map((r) => r.id)).toEqual([9, 8])
  })
})

describe('collapseUpcoming', () => {
  function match(over: Partial<ProgramSearchMatch>): ProgramSearchMatch {
    return {
      site: 'default',
      programId: 1,
      networkId: 32678,
      serviceId: 5168,
      startAt: '2026-09-30T12:00:00Z',
      durationMs: 1_800_000,
      name: 'アニメ　作品X　第2話',
      isFree: true,
      ...over,
    }
  }

  it('(networkId, serviceId, startAt) が同じ行を 1 行にまとめ、site を集める', () => {
    const rows = collapseUpcoming([
      match({ site: 'default', programId: 11 }),
      match({ site: 'other', programId: 11 }),
      match({ site: 'default', programId: 22, networkId: 32679, serviceId: 5169 }),
    ])
    expect(rows).toHaveLength(2)
    expect(rows[0].sites).toEqual(['default', 'other'])
    expect(rows[0].programIds).toEqual([11])
    expect(rows[1].sites).toEqual(['default'])
  })

  it('放送開始の昇順に並べる', () => {
    const rows = collapseUpcoming([
      match({ startAt: '2026-10-07T12:00:00Z', programId: 3 }),
      match({ startAt: '2026-09-30T12:00:00Z', programId: 2 }),
    ])
    expect(rows.map((row) => row.programIds[0])).toEqual([2, 3])
  })

  it('空なら空を返す', () => {
    expect(collapseUpcoming([])).toEqual([])
  })
})
