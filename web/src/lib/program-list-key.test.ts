import { describe, expect, it } from 'vitest'

import type { ProgramListItem } from '@/api/generated'
import { findProgramIndex, programKeyAt } from '@/lib/program-list-key'

/** program は最小限のフィールドだけ埋めた `ProgramListItem`。programKeyAt は programId しか見ない。 */
function program(programId: number): ProgramListItem {
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
  it('先頭に差し込んで添字がずれても、同じ番組には同じキー（programId）が付く', () => {
    const before = [program(10), program(20), program(30)]
    // 遡行で先頭に 1 件差し込んだ状態を模す
    const after = [program(99), ...before]

    // 差し込み前は index 0 が programId 10
    expect(programKeyAt(before, 0)).toBe(10)
    // 差し込み後、同じ番組（10）は index 1 に移動するが、キーは変わらず 10 のまま
    expect(programKeyAt(after, 1)).toBe(10)
    // 対照: 添字そのものをキーにする実装（TanStack Virtual の既定 `(index) => index`）
    // だったら、この一致は成り立たない。両方向で違いを確認する
    expect(programKeyAt(after, 1)).not.toBe(1)
    expect(programKeyAt(after, 2)).toBe(20)
    expect(programKeyAt(after, 2)).not.toBe(2)
  })

  it('（対照）添字ベースの既定キーだと、差し込み前後で同じ番組のキーが変わってしまう', () => {
    // TanStack Virtual の既定 getItemKey は (index) => index。これと programKeyAt を
    // 突き合わせて、差し込みが起きたときに何が壊れるかを明示する。
    // programId をわざと元の添字と同じ値にしておくと、「挿入前は添字ベースでも
    // programId ベースでもたまたま同じキーになる（バグが表面化しない）」ことを
    // 素直に表現できる
    const indexBasedKey = (index: number) => index
    const before = [program(0), program(1), program(2)]
    const after = [program(99), ...before]

    // 差し込み前は両者が一致してしまう（バグが表面化しない理由）
    expect(indexBasedKey(0)).toBe(programKeyAt(before, 0))
    // 差し込み後は不一致になる ---
    // 添字ベースのキーは programId 0 の行（before の先頭。後ろへ 1 つ移動した）の
    // 実測値を programId 99 の行のものとして扱ってしまう、というのがこの
    // バグの実体
    expect(indexBasedKey(1)).not.toBe(programKeyAt(after, 1))
  })
})

describe('findProgramIndex（遡行アンカーの新しい添字を引く）', () => {
  it('先頭に差し込まれた後でも、控えておいた programId から新しい添字を引ける', () => {
    const before = [program(10), program(20), program(30)]
    // 遡行で先頭に 1 件差し込まれた状態を模す
    const after = [program(99), ...before]

    // 差し込み前に控えた「programId 10 の行」は、差し込み後は添字 1 に移動している
    expect(findProgramIndex(after, 10)).toBe(1)
    expect(findProgramIndex(after, 20)).toBe(2)
    expect(findProgramIndex(after, 30)).toBe(3)
  })

  it('対照: 控えた programId が新しい配列に存在しない場合は null を返す（呼び出し側は何もしない）', () => {
    const after = [program(99), program(10), program(20)]

    expect(findProgramIndex(after, 404)).toBeNull()
  })
})
