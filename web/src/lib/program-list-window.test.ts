import { describe, expect, it } from 'vitest'

import { filterProgramsFromListStart } from '@/lib/program-list-window'

/** program は startAt だけを持つ最小限のオブジェクト。 */
function program(startAtMs: number): { startAt: string } {
  return { startAt: new Date(startAtMs).toISOString() }
}

const hour = 3_600_000

describe('filterProgramsFromListStart', () => {
  it('listStartMs が下限と一致しないとき、listStartMs より前に始まった番組を取り除く', () => {
    const listStartMs = 10 * hour
    const lowerBoundMs = 0 // 下限とは一致しない
    const programs = [
      program(listStartMs - hour), // 前の窓との重なり（前日 23:30 相当）→ 除く
      program(listStartMs), // ちょうど境界 → 残す
      program(listStartMs + hour), // 境界より後 → 残す
    ]

    const result = filterProgramsFromListStart(programs, listStartMs, lowerBoundMs)

    expect(result.map((p) => p.startAt)).toEqual([
      programs[1].startAt,
      programs[2].startAt,
    ])
  })

  it('listStartMs が下限と一致するとき（今日、または遡行が下限まで達したとき）は絞り込まない', () => {
    const lowerBoundMs = 10 * hour
    const listStartMs = lowerBoundMs // 一致
    const programs = [
      program(listStartMs - hour), // 放送中の番組（開始は境界より前）→ 消してはいけない
      program(listStartMs),
      program(listStartMs + hour),
    ]

    const result = filterProgramsFromListStart(programs, listStartMs, lowerBoundMs)

    expect(result).toHaveLength(3)
    expect(result.map((p) => p.startAt)).toEqual(programs.map((p) => p.startAt))
  })
})
