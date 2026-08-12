import { describe, expect, it } from 'vitest'

import {
  emptyProgramsSearch,
  parseProgramsSearch,
  serviceIdsFromSet,
  serviceIdsToSet,
} from '@/lib/programs-search'

describe('parseProgramsSearch', () => {
  it('何も無ければ絞り込みなし（serviceId は明示的に undefined）になる', () => {
    const result = parseProgramsSearch({})
    expect(result).toEqual({ serviceId: undefined })
    // 「キーが無い」ではなく「キーがあって値が undefined」であることそのものを見る
    // （omit-on-invalid の罠。CLAUDE.md「validateSearch の omit-on-invalid」）
    expect('serviceId' in result).toBe(true)
  })

  it('有効な値をそのまま受け取る（重複を除き昇順にソートする）', () => {
    expect(parseProgramsSearch({ serviceId: [1032, 1024, 1032] })).toEqual({
      serviceId: [1024, 1032],
    })
  })

  it('単一値（配列でない ?serviceId=1024）は 1 要素の配列に正規化する', () => {
    expect(parseProgramsSearch({ serviceId: 1024 })).toEqual({ serviceId: [1024] })
  })

  it('数値化できない要素・0 以下の要素は落とす（丸めない）', () => {
    expect(parseProgramsSearch({ serviceId: ['abc', 1024, 0, -5, 1032] })).toEqual({
      serviceId: [1024, 1032],
    })
  })

  it('文字列の serviceId も数値化する', () => {
    expect(parseProgramsSearch({ serviceId: ['1024', '1032'] })).toEqual({
      serviceId: [1024, 1032],
    })
  })

  it('全要素が不正なら serviceId キー自体を undefined にする（空配列を作らない）', () => {
    expect(parseProgramsSearch({ serviceId: ['abc', 0, -1] })).toEqual({ serviceId: undefined })
  })

  it('非整数（1.5）は落とす', () => {
    expect(parseProgramsSearch({ serviceId: [1.5, 1024] })).toEqual({ serviceId: [1024] })
  })
})

describe('serviceIdsToSet / serviceIdsFromSet', () => {
  it('未指定は空集合、空集合は undefined（往復で一致する）', () => {
    expect(serviceIdsToSet(undefined)).toEqual(new Set())
    expect(serviceIdsFromSet(new Set())).toBeUndefined()
  })

  it('往復すると同じ集合になる（順序は昇順に正規化される）', () => {
    const set = serviceIdsToSet([1032, 1024])
    expect(serviceIdsFromSet(set)).toEqual([1024, 1032])
  })

  it('emptyProgramsSearch は serviceId 未指定', () => {
    expect(emptyProgramsSearch()).toEqual({})
  })
})
