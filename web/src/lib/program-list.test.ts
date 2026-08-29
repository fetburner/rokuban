import { describe, expect, it } from 'vitest'

import type { ProgramListItem } from '@/api/generated'
import {
  filterProgramsFromListStart,
  firstIndexForDayOffset,
  programKeyAt,
  visibleDayOffset,
} from '@/lib/program-list'

const now = new Date(2026, 7, 1, 10, 0, 0, 0).getTime() // 2026-08-01 10:00

function programAt(daysFromNow: number, hour = 12): { startAt: string } {
  const d = new Date(2026, 7, 1 + daysFromNow, hour, 0, 0, 0)
  return { startAt: d.toISOString() }
}

describe('visibleDayOffset', () => {
  it('先頭が今日の番組なら 0 を返す', () => {
    const programs = [programAt(0)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
  })

  it('先頭が明日の番組なら 1 を返す', () => {
    const programs = [programAt(1)]
    expect(visibleDayOffset(programs, 0, now)).toBe(1)
  })

  it('日をまたぐ位置で切り替わる（先頭インデックスが変わると offset も変わる）', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
    expect(visibleDayOffset(programs, 1, now)).toBe(1)
    expect(visibleDayOffset(programs, 2, now)).toBe(1)
  })

  it('月をまたぐ番組でも正しい offset になる', () => {
    // 2026-08-01 から 2026-09-01 は 31 日先
    const programs = [{ startAt: new Date(2026, 8, 1, 12, 0, 0, 0).toISOString() }]
    expect(visibleDayOffset(programs, 0, now)).toBe(31)
  })

  it('先頭インデックスが範囲外（空リスト）なら 0 を返す', () => {
    expect(visibleDayOffset([], 0, now)).toBe(0)
    expect(visibleDayOffset([programAt(3)], 5, now)).toBe(0)
  })

  it('now より前の番組（時計のずれ等）が先頭に来ても 0 未満にはならない', () => {
    const programs = [programAt(-2)]
    expect(visibleDayOffset(programs, 0, now)).toBe(0)
  })
})

describe('firstIndexForDayOffset（visibleDayOffset と対になる向き）', () => {
  it('指定した dayOffset の暦日に一致する最初の番組の添字を返す', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1), programAt(2, 5)]
    expect(firstIndexForDayOffset(programs, 1, now)).toBe(1)
    expect(firstIndexForDayOffset(programs, 2, now)).toBe(3)
  })

  it('先頭に一致しても、複数候補があれば「最初」の添字を返す', () => {
    const programs = [programAt(0, 1), programAt(0, 2), programAt(1, 0)]
    expect(firstIndexForDayOffset(programs, 0, now)).toBe(0)
  })

  it('対照: visibleDayOffset で得た offset を firstIndexForDayOffset に渡すと同じ番組の添字に戻る', () => {
    const programs = [programAt(0, 23), programAt(1, 0), programAt(1, 1)]
    const offset = visibleDayOffset(programs, 2, now)
    expect(firstIndexForDayOffset(programs, offset, now)).toBe(1)
  })

  it('該当する暦日の番組が 1 件も無ければ null を返す（まだ読み込んでいない日）', () => {
    const programs = [programAt(0), programAt(1)]
    expect(firstIndexForDayOffset(programs, 5, now)).toBeNull()
  })

  it('番組が 0 件でも null を返す', () => {
    expect(firstIndexForDayOffset([], 0, now)).toBeNull()
  })
})

/** program は最小限のフィールドだけ埋めた `ProgramListItem`。programKeyAt は programId しか見ない。 */
function listItem(programId: number): ProgramListItem {
  return {
    programId,
    networkId: 32736,
    serviceId: 1024,
    eventId: programId,
    startAt: new Date(0).toISOString(),
    endAt: new Date(1_800_000).toISOString(),
    durationMs: 1_800_000,
    name: `番組 ${programId}`,
    description: '',
    genres: [],
    isFree: true,
  }
}

describe('programKeyAt（仮想化の getItemKey）', () => {
  it('先頭の内容がずれても、同じ番組には同じキー（programId）が付く', () => {
    const before = [listItem(10), listItem(20), listItem(30)]
    // 絞り込みの変更等で先頭に別の番組が来て添字がずれた状態を模す
    const after = [listItem(99), ...before]

    // ずれる前は index 0 が programId 10
    expect(programKeyAt(before, 0)).toBe(10)
    // ずれた後、同じ番組（10）は index 1 に移動するが、キーは変わらず 10 のまま
    expect(programKeyAt(after, 1)).toBe(10)
    // 対照: 添字そのものをキーにする実装（TanStack Virtual の既定 `(index) => index`）
    // だったら、この一致は成り立たない。両方向で違いを確認する
    expect(programKeyAt(after, 1)).not.toBe(1)
    expect(programKeyAt(after, 2)).toBe(20)
    expect(programKeyAt(after, 2)).not.toBe(2)
  })

  it('（対照）添字ベースの既定キーだと、ずれ前後で同じ番組のキーが変わってしまう', () => {
    // TanStack Virtual の既定 getItemKey は (index) => index。これと programKeyAt を
    // 突き合わせて、先頭がずれたときに何が壊れるかを明示する。
    // programId をわざと元の添字と同じ値にしておくと、「ずれる前は添字ベースでも
    // programId ベースでもたまたま同じキーになる（バグが表面化しない）」ことを
    // 素直に表現できる
    const indexBasedKey = (index: number) => index
    const before = [listItem(0), listItem(1), listItem(2)]
    const after = [listItem(99), ...before]

    // ずれる前は両者が一致してしまう（バグが表面化しない理由）
    expect(indexBasedKey(0)).toBe(programKeyAt(before, 0))
    // ずれた後は不一致になる ---
    // 添字ベースのキーは programId 0 の行（before の先頭。後ろへ 1 つ移動した）の
    // 実測値を programId 99 の行のものとして扱ってしまう、というのがこの
    // バグの実体
    expect(indexBasedKey(1)).not.toBe(programKeyAt(after, 1))
  })
})

/** program は startAt だけを持つ最小限のオブジェクト。 */
function startingAt(startAtMs: number): { startAt: string } {
  return { startAt: new Date(startAtMs).toISOString() }
}

const hour = 3_600_000

describe('filterProgramsFromListStart', () => {
  it('listStartMs が下限と一致しないとき、listStartMs より前に始まった番組を取り除く', () => {
    const listStartMs = 10 * hour
    const lowerBoundMs = 0 // 下限とは一致しない
    const programs = [
      startingAt(listStartMs - hour), // 窓の外との重なり（前日 23:30 相当）→ 除く
      startingAt(listStartMs), // ちょうど境界 → 残す
      startingAt(listStartMs + hour), // 境界より後 → 残す
    ]

    const result = filterProgramsFromListStart(programs, listStartMs, lowerBoundMs)

    expect(result.map((p) => p.startAt)).toEqual([
      programs[1].startAt,
      programs[2].startAt,
    ])
  })

  it('listStartMs が下限と一致するとき（今日を見ているとき）は絞り込まない', () => {
    const lowerBoundMs = 10 * hour
    const listStartMs = lowerBoundMs // 一致
    const programs = [
      startingAt(listStartMs - hour), // 放送中の番組（開始は境界より前）→ 消してはいけない
      startingAt(listStartMs),
      startingAt(listStartMs + hour),
    ]

    const result = filterProgramsFromListStart(programs, listStartMs, lowerBoundMs)

    expect(result).toHaveLength(3)
    expect(result.map((p) => p.startAt)).toEqual(programs.map((p) => p.startAt))
  })
})
