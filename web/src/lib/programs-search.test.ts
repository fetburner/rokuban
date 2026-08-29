import { describe, expect, it } from 'vitest'

import { ServiceChannelType, type Service } from '@/api/generated'
import { parseProgramsSearch, pickerServiceDomain } from '@/lib/programs-search'

describe('parseProgramsSearch', () => {
  it('何も無ければ絞り込みなし（service は明示的に undefined）になる', () => {
    const result = parseProgramsSearch({})
    expect(result).toEqual({ service: undefined, at: undefined })
    // 「キーが無い」ではなく「キーがあって値が undefined」であることそのものを見る
    // （omit-on-invalid の罠。CLAUDE.md「validateSearch の omit-on-invalid」）
    expect('service' in result).toBe(true)
  })

  it('service を重複除去し昇順に正準化する', () => {
    expect(parseProgramsSearch({ service: [600101, 400101, 600101, 400102] })).toEqual({
      service: [400101, 400102, 600101],
      at: undefined,
    })
  })

  it('service の不正値・0・負値・非整数を要素ごとに落とす', () => {
    expect(
      parseProgramsSearch({ service: ['bad', 0, -1, 1.5, '400101', 600101] }),
    ).toEqual({ service: [400101, 600101], at: undefined })
  })

  // issue #345: Number.MAX_SAFE_INTEGER を超える値は Number() の時点で既に
  // 別の値に丸まる。丸めた値を「利用者が指定した id」として通すと別の
  // チャンネルを指してしまう。リテラルではなく式で書くのは oxlint の
  // `no-loss-of-precision` を誤って踏まないため。
  const unsafeId = Number.MAX_SAFE_INTEGER + 2
  it('値域外の id は丸めずに落とす（上限は zod、整数性は asInteger）', () => {
    expect(parseProgramsSearch({ service: [unsafeId, 400101] })).toEqual({
      service: [400101],
      at: undefined,
    })
    expect(parseProgramsSearch({ service: [unsafeId] })).toEqual({
      service: undefined,
      at: undefined,
    })
  })

  it('at が無ければ明示的に undefined になる', () => {
    const result = parseProgramsSearch({})
    expect(result).toEqual({ service: undefined, at: undefined })
    expect('at' in result).toBe(true)
  })

  it('at は数値化できればそのまま受け取る（文字列も数値化する）', () => {
    expect(parseProgramsSearch({ at: 1_700_000_000_000 })).toEqual({
      service: undefined,
      at: 1_700_000_000_000,
    })
    expect(parseProgramsSearch({ at: '1700000000000' })).toEqual({
      service: undefined,
      at: 1700000000000,
    })
  })

  it('at は service と違い、0 以下・過去の値も落とさない', () => {
    expect(parseProgramsSearch({ at: -1 })).toEqual({ service: undefined, at: -1 })
    expect(parseProgramsSearch({ at: 0 })).toEqual({ service: undefined, at: 0 })
  })

  it('at が数値化できない・非整数なら undefined に落とす', () => {
    expect(parseProgramsSearch({ at: 'abc' })).toEqual({ service: undefined, at: undefined })
    expect(parseProgramsSearch({ at: 1.5 })).toEqual({ service: undefined, at: undefined })
    expect(parseProgramsSearch({ at: [1, 2] })).toEqual({ service: undefined, at: undefined })
  })

  // nit 3（レビュー）: 空文字は Number('') === 0 で「0 時ちょうど」という
  // 具体的な値に化ける。`?at=` という壊れたリンクを「0 時にジャンプ」ではなく
  // 「欠落（絞り込みなし）」と読む。
  it('at の空文字は 0 に変換せず undefined に落とす', () => {
    expect(parseProgramsSearch({ at: '' })).toEqual({ service: undefined, at: undefined })
    expect(parseProgramsSearch({ at: '   ' })).toEqual({ service: undefined, at: undefined })
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
      service: undefined,
      at: undefined,
    })
    expect(parseProgramsSearch({ at: '9000000000000000' })).toEqual({
      service: undefined,
      at: undefined,
    })
    expect(parseProgramsSearch({ at: 1e30 })).toEqual({ service: undefined, at: undefined })
    expect(parseProgramsSearch({ at: '99999999999999999999' })).toEqual({
      service: undefined,
      at: undefined,
    })
    // 負の側の定義域外も同じく落とす
    expect(parseProgramsSearch({ at: -9_000_000_000_000_000 })).toEqual({
      service: undefined,
      at: undefined,
    })
  })

  it('Date の time value の定義域の境界そのもの（±8,640,000,000,000,000）は受け入れる', () => {
    expect(parseProgramsSearch({ at: 8_640_000_000_000_000 })).toEqual({
      service: undefined,
      at: 8_640_000_000_000_000,
    })
    expect(parseProgramsSearch({ at: -8_640_000_000_000_000 })).toEqual({
      service: undefined,
      at: -8_640_000_000_000_000,
    })
    // 境界の 1ms 外は落ちる
    expect(parseProgramsSearch({ at: 8_640_000_000_000_001 })).toEqual({
      service: undefined,
      at: undefined,
    })
  })

  // issue #437: `view` は画面ローカルの表示形式（グリッド / リスト）。容量不足
  // バッジが `search={{ view: 'grid', at }}` を発行するので、往復（バッジが
  // 積んだ値がそのまま検証を通る）を確認する。
  it('view は grid / list をそのまま受け取る', () => {
    expect(parseProgramsSearch({ view: 'grid' })).toEqual({
      service: undefined,
      at: undefined,
      view: 'grid',
    })
    expect(parseProgramsSearch({ view: 'list' })).toEqual({
      service: undefined,
      at: undefined,
      view: 'list',
    })
  })

  it('view が grid / list 以外なら明示的に undefined に落とす', () => {
    expect(parseProgramsSearch({ view: 'bogus' })).toEqual({
      service: undefined,
      at: undefined,
      view: undefined,
    })
    expect(parseProgramsSearch({ view: 123 })).toEqual({
      service: undefined,
      at: undefined,
      view: undefined,
    })
    expect(parseProgramsSearch({ view: ['grid'] })).toEqual({
      service: undefined,
      at: undefined,
      view: undefined,
    })
  })

  it('view が無ければ明示的に undefined になる', () => {
    const result = parseProgramsSearch({})
    expect(result).toEqual({ service: undefined, at: undefined, view: undefined })
    expect('view' in result).toBe(true)
  })
})


describe('pickerServiceDomain', () => {
  function service(overrides: Partial<Service>): Service {
    const networkId = overrides.networkId ?? 1
    const serviceId = overrides.serviceId ?? 1024
    return {
      id: networkId * 100_000 + serviceId,
      networkId,
      serviceId,
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
    [101024, nhk],
    [101032, etv],
    [101040, sub],
  ])

  it('同じ serviceId の別 network を別候補として保つ（合成 id が違う）', () => {
    const bs = service({ networkId: 4, serviceId: 101, name: 'BS 101', channelType: ServiceChannelType.BS })
    const cs = service({ networkId: 6, serviceId: 101, name: 'CS 101', channelType: ServiceChannelType.CS })
    const result = pickerServiceDomain(
      [bs, cs],
      new Set(),
      new Map([
        [400101, bs],
        [600101, cs],
      ]),
    )

    expect(result).toEqual([bs, cs])
  })

  it('選択が無ければ filterable のまま', () => {
    expect(pickerServiceDomain([nhk, etv], new Set(), serviceById)).toEqual([nhk, etv])
  })

  it('選択が filterable に含まれていれば重複を作らない', () => {
    const result = pickerServiceDomain([nhk, etv], new Set([101024]), serviceById)
    expect(result).toHaveLength(2)
  })

  it('filterable に無いが serviceById に実在する選択は実名で候補に加わる（must-fix の核心）', () => {
    // hasPrograms: false の局（サブサービス等）への深いリンク。この局は
    // filterable（候補の生成元）に居ないが、選択（URL からの外部入力）には
    // 居るので、ピッカーが「0 件選択（＝すべて）」に見えてはならない
    const result = pickerServiceDomain([nhk, etv], new Set([101040]), serviceById)
    expect(result.map((s) => s.serviceId)).toContain(1040)
    expect(result.find((s) => s.serviceId === 1040)).toEqual(sub)
  })

  it('serviceById にも無い選択はプレースホルダー（チャンネル #<id>）になる', () => {
    // EPG から消えた局・実在しない id を含む古いブックマーク・共有リンク。
    // 名前は引けないが「何かで絞られている」ことは読める必要がある
    // （`describeRecordingsFilters` と同じ流儀）
    const result = pickerServiceDomain([nhk, etv], new Set([600101]), serviceById)
    const placeholder = result.find((s) => s.id === 600101)
    expect(placeholder?.name).toBe('チャンネル #600101')
    // 合成 id を network / service に分解して持つ（`id % 100000` が id 自身に
    // なる小さい値だけで試すと、分解を消しても気付けない）。
    expect(placeholder?.networkId).toBe(6)
    expect(placeholder?.serviceId).toBe(101)
    expect(placeholder?.hasPrograms).toBe(false)
  })

  it('複数選択（実在・プレースホルダーの混在）を両方とも候補に加える', () => {
    const result = pickerServiceDomain([nhk, etv], new Set([101040, 600101]), serviceById)
    expect(result.map((s) => s.id).sort((a, b) => a - b)).toEqual([101024, 101032, 101040, 600101])
  })
})
