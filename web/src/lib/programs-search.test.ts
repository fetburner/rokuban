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
      service: undefined,
      serviceId: [1024, 1032],
    })
  })

  it('厳密な service 組を重複除去し networkId, serviceId 順に正準化する', () => {
    expect(parseProgramsSearch({ service: ['6:101', '4:101', '6:101', '4:102'] })).toEqual({
      service: ['4:101', '4:102', '6:101'],
      serviceId: undefined,
    })
  })

  it('service の不正値・0・先頭0・int32上限超を要素ごとに落とす', () => {
    expect(
      parseProgramsSearch({
        service: ['bad', '0:101', '04:101', '4:0', '4:0101', '2147483648:101', '4:2147483648', '4:101'],
      }),
    ).toEqual({ service: ['4:101'], serviceId: undefined })
  })

  it('service と serviceId が無ければ両方を明示的に undefined にする', () => {
    const result = parseProgramsSearch({})
    expect(result.service).toBeUndefined()
    expect(result.serviceId).toBeUndefined()
    expect('service' in result).toBe(true)
    expect('serviceId' in result).toBe(true)
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

  // issue #345: Number.MAX_SAFE_INTEGER を超える値は Number() の時点で既に
  // 別の値に丸まる（`Number.MAX_SAFE_INTEGER + 2` は `Number.MAX_SAFE_INTEGER + 1`
  // に丸まる）。丸めた値を「利用者が指定した id」として通すと、別の serviceId を
  // 指してしまう。リテラルではなく式で書くのは oxlint の
  // `no-loss-of-precision`（数値リテラルの丸めそのものを警告する規則）を
  // 誤って踏まないため --- ここでは丸めが起きることこそテストの主張。
  const unsafeId = Number.MAX_SAFE_INTEGER + 2
  it('安全整数を超える要素は丸めずに落とす', () => {
    expect(parseProgramsSearch({ serviceId: [unsafeId, 1024] })).toEqual({
      serviceId: [1024],
    })
    expect(parseProgramsSearch({ serviceId: [unsafeId] })).toEqual({
      serviceId: undefined,
    })
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

  // nit 3（レビュー）: 空文字は Number('') === 0 で「0 時ちょうど」という
  // 具体的な値に化ける。`?at=` という壊れたリンクを「0 時にジャンプ」ではなく
  // 「欠落（絞り込みなし）」と読む。
  it('at の空文字は 0 に変換せず undefined に落とす', () => {
    expect(parseProgramsSearch({ at: '' })).toEqual({ serviceId: undefined, at: undefined })
    expect(parseProgramsSearch({ at: '   ' })).toEqual({ serviceId: undefined, at: undefined })
  })

  // レビューの must-fix 1: `Date` の time value の定義域（±8,640,000,000,000,000ms）
  // を超える at は落とす。落とさないと `new Date(at)` が Invalid Date になり、
  // 後続の `dayOffsetForMs` → `dayOrigin` → `.toISOString()` が
  // `RangeError: Invalid time value` を投げて番組表ページ全体がエラー境界に
  // 落ちる（実測: 実ブラウザ・jsdom の両方で `/programs?at=1e30`（当時は `/`。
  // ホーム新設（M8-3）で番組表は `/programs` に移設した）等が
  // "Something went wrong!" になった）。
  it('Date の time value の定義域を超える at は undefined に落とす（実測で RangeError の原因だった値）', () => {
    expect(parseProgramsSearch({ at: 9_000_000_000_000_000 })).toEqual({
      serviceId: undefined,
      at: undefined,
    })
    expect(parseProgramsSearch({ at: '9000000000000000' })).toEqual({
      serviceId: undefined,
      at: undefined,
    })
    expect(parseProgramsSearch({ at: 1e30 })).toEqual({ serviceId: undefined, at: undefined })
    expect(parseProgramsSearch({ at: '99999999999999999999' })).toEqual({
      serviceId: undefined,
      at: undefined,
    })
    // 負の側の定義域外も同じく落とす
    expect(parseProgramsSearch({ at: -9_000_000_000_000_000 })).toEqual({
      serviceId: undefined,
      at: undefined,
    })
  })

  it('Date の time value の定義域の境界そのもの（±8,640,000,000,000,000）は受け入れる', () => {
    expect(parseProgramsSearch({ at: 8_640_000_000_000_000 })).toEqual({
      serviceId: undefined,
      at: 8_640_000_000_000_000,
    })
    expect(parseProgramsSearch({ at: -8_640_000_000_000_000 })).toEqual({
      serviceId: undefined,
      at: -8_640_000_000_000_000,
    })
    // 境界の 1ms 外は落ちる
    expect(parseProgramsSearch({ at: 8_640_000_000_000_001 })).toEqual({
      serviceId: undefined,
      at: undefined,
    })
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
  // serviceByKey（EPG プロジェクション全体）には実在する
  const sub = service({ serviceId: 1040, name: 'サブサービス', hasPrograms: false })

  const serviceByKey = new Map([
    ['1:1024', nhk],
    ['1:1032', etv],
    ['1:1040', sub],
  ])

  it('同じ serviceId の別 network を別候補として保つ', () => {
    const bs = service({ networkId: 4, serviceId: 101, name: 'BS 101', channelType: ServiceChannelType.BS })
    const cs = service({ networkId: 6, serviceId: 101, name: 'CS 101', channelType: ServiceChannelType.CS })
    const result = pickerServiceDomain(
      [bs, cs],
      new Set(),
      new Map([
        ['4:101', bs],
        ['6:101', cs],
      ]),
    )

    expect(result).toEqual([bs, cs])
  })

  it('選択が無ければ filterable のまま', () => {
    expect(pickerServiceDomain([nhk, etv], new Set(), serviceByKey)).toEqual([nhk, etv])
  })

  it('選択が filterable に含まれていれば重複を作らない', () => {
    const result = pickerServiceDomain([nhk, etv], new Set(['1:1024']), serviceByKey)
    expect(result).toHaveLength(2)
  })

  it('filterable に無いが serviceByKey に実在する選択は実名で候補に加わる（must-fix の核心）', () => {
    // hasPrograms: false の局（サブサービス等）への深いリンク。この局は
    // filterable（候補の生成元）に居ないが、選択（URL からの外部入力）には
    // 居るので、ピッカーが「0 件選択（＝すべて）」に見えてはならない
    const result = pickerServiceDomain([nhk, etv], new Set(['1:1040']), serviceByKey)
    expect(result.map((s) => s.serviceId)).toContain(1040)
    expect(result.find((s) => s.serviceId === 1040)).toEqual(sub)
  })

  it('serviceByKey にも無い選択はプレースホルダー（チャンネル #<id>）になる', () => {
    // EPG から消えた局・実在しない id を含む古いブックマーク・共有リンク。
    // 名前は引けないが「何かで絞られている」ことは読める必要がある
    // （`describeRecordingsFilters` と同じ流儀）
    const result = pickerServiceDomain([nhk, etv], new Set(['0:9999']), serviceByKey)
    const placeholder = result.find((s) => s.serviceId === 9999)
    expect(placeholder?.name).toBe('チャンネル #9999')
    expect(placeholder?.hasPrograms).toBe(false)
  })

  it('複数選択（実在・プレースホルダーの混在）を両方とも候補に加える', () => {
    const result = pickerServiceDomain([nhk, etv], new Set(['1:1040', '0:9999']), serviceByKey)
    expect(result.map((s) => s.serviceId).sort((a, b) => a - b)).toEqual([1024, 1032, 1040, 9999])
  })
})
