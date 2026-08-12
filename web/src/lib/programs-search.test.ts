import { describe, expect, it } from 'vitest'

import { ServiceChannelType, type Service } from '@/api/generated'
import {
  parseProgramsSearch,
  pickerServiceDomain,
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

  // `at`（issue #233 M6-5 の容量バッジ導線）。omit-on-invalid の罠は `serviceId` と
  // 同じなので、キー自体は常に存在させる。
  it('at が無ければ明示的に undefined になる', () => {
    const result = parseProgramsSearch({})
    expect(result).toEqual({ serviceId: undefined, at: undefined })
    expect('at' in result).toBe(true)
  })

  it('at は数値化できればそのまま受け取る（文字列も数値化する）', () => {
    expect(parseProgramsSearch({ at: 1_700_000_000_000 })).toEqual({
      serviceId: undefined,
      at: 1_700_000_000_000,
    })
    expect(parseProgramsSearch({ at: '1700000000000' })).toEqual({
      serviceId: undefined,
      at: 1700000000000,
    })
  })

  it('at は serviceId と違い、0 以下・過去の値も落とさない', () => {
    expect(parseProgramsSearch({ at: -1 })).toEqual({ serviceId: undefined, at: -1 })
    expect(parseProgramsSearch({ at: 0 })).toEqual({ serviceId: undefined, at: 0 })
  })

  it('at が数値化できない・非整数なら undefined に落とす', () => {
    expect(parseProgramsSearch({ at: 'abc' })).toEqual({ serviceId: undefined, at: undefined })
    expect(parseProgramsSearch({ at: 1.5 })).toEqual({ serviceId: undefined, at: undefined })
    expect(parseProgramsSearch({ at: [1, 2] })).toEqual({ serviceId: undefined, at: undefined })
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
})

describe('pickerServiceDomain', () => {
  function service(overrides: Partial<Service>): Service {
    return {
      networkId: 1,
      serviceId: 1024,
      name: 'サービス',
      channelType: ServiceChannelType.GR,
      channel: '27',
      remoteControlKeyId: 1,
      hasLogoData: false,
      hasPrograms: true,
      ...overrides,
    }
  }

  const nhk = service({ serviceId: 1024, name: 'NHK総合' })
  const etv = service({ serviceId: 1032, name: 'NHKEテレ', remoteControlKeyId: 2 })
  // hasPrograms: false（サブサービス等）なので filterable には入らないが、
  // serviceById（EPG プロジェクション全体）には実在する
  const sub = service({ serviceId: 1040, name: 'サブサービス', hasPrograms: false })

  const serviceById = new Map([
    [nhk.serviceId, nhk],
    [etv.serviceId, etv],
    [sub.serviceId, sub],
  ])

  it('選択が無ければ filterable のまま', () => {
    expect(pickerServiceDomain([nhk, etv], new Set(), serviceById)).toEqual([nhk, etv])
  })

  it('選択が filterable に含まれていれば重複を作らない', () => {
    const result = pickerServiceDomain([nhk, etv], new Set([1024]), serviceById)
    expect(result).toHaveLength(2)
  })

  it('filterable に無いが serviceById に実在する選択は実名で候補に加わる（must-fix の核心）', () => {
    // hasPrograms: false の局（サブサービス等）への深いリンク。この局は
    // filterable（候補の生成元）に居ないが、選択（URL からの外部入力）には
    // 居るので、ピッカーが「0 件選択（＝すべて）」に見えてはならない
    const result = pickerServiceDomain([nhk, etv], new Set([1040]), serviceById)
    expect(result.map((s) => s.serviceId)).toContain(1040)
    expect(result.find((s) => s.serviceId === 1040)).toEqual(sub)
  })

  it('serviceById にも無い選択はプレースホルダー（チャンネル #<id>）になる', () => {
    // EPG から消えた局・実在しない id を含む古いブックマーク・共有リンク。
    // 名前は引けないが「何かで絞られている」ことは読める必要がある
    // （`describeRecordingsFilters` と同じ流儀）
    const result = pickerServiceDomain([nhk, etv], new Set([9999]), serviceById)
    const placeholder = result.find((s) => s.serviceId === 9999)
    expect(placeholder?.name).toBe('チャンネル #9999')
    expect(placeholder?.hasPrograms).toBe(false)
  })

  it('複数選択（実在・プレースホルダーの混在）を両方とも候補に加える', () => {
    const result = pickerServiceDomain([nhk, etv], new Set([1040, 9999]), serviceById)
    expect(result.map((s) => s.serviceId).sort((a, b) => a - b)).toEqual([1024, 1032, 1040, 9999])
  })
})
