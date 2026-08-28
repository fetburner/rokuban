import { describe, expect, it } from 'vitest'

import { formatDateTime } from '@/lib/format'
import {
  buildListRecordingsParams,
  clearRecordingsFilters,
  describeRecordingsFilters,
  emptyRecordingsSearch,
  hasAnyRecordingsCondition,
  isoToLocalDateTimeInput,
  localDateTimeInputToIso,
  parseRecordingsSearch,
  type RecordingsPageSearch,
} from '@/lib/recording-search'

describe('parseRecordingsSearch', () => {
  it('何も無ければ空の検索条件になる', () => {
    expect(parseRecordingsSearch({})).toEqual({})
  })

  it('有効な値をそのまま受け取る', () => {
    expect(
      parseRecordingsSearch({
        q: 'ニュース',
        genre: [0, 1],
        service: [3273601024],
        status: 'failed',
        source: 'manual',
        ruleId: 5,
        from: '2026-01-01T00:00:00.000Z',
        to: '2026-01-02T00:00:00.000Z',
        order: 'asc',
      }),
    ).toEqual({
      q: 'ニュース',
      genre: [0, 1],
      service: [3273601024],
      status: 'failed',
      source: 'manual',
      ruleId: 5,
      from: '2026-01-01T00:00:00.000Z',
      to: '2026-01-02T00:00:00.000Z',
      order: 'asc',
    })
  })

  // 「壊れたリンクを踏んでも画面は開く」（/search の ruleId と同じ流儀）。
  // 例外にせず、それぞれの次元だけを「指定なし」に落とす。
  it('不正な値は例外にせずキーごと落とす', () => {
    const result = parseRecordingsSearch({
      q: 42, // 文字列でない
      genre: [99, -1, 2.5, 2], // 範囲外・非整数は落ちる。2 だけ残る
      service: ['abc', 3273605168], // 数値化できないものは落ちる
      status: 'bogus', // enum に無い
      source: 'nobody', // enum に無い
      ruleId: 'not-a-number',
      from: 'not-a-date',
      order: 'sideways',
    })
    expect(result).toEqual({ genre: [2], service: [3273605168] })
  })

  it('単一値（配列でない）の genre/service は 1 要素の配列に正規化する', () => {
    // ?genre=5 のような、リピートキーでない URL を手で叩いた場合。
    expect(parseRecordingsSearch({ genre: 5, service: 3273601024 })).toEqual({
      genre: [5],
      service: [3273601024],
    })
  })

  // 検証は 2 段。値域（1..6553565535）は openapi.yaml 由来の zod スキーマが、
  // 整数性（`1.5` を落とす）は `lib/url-search.ts` の `asInteger` が担う。
  // 下の `unsafeId` を実際に落としているのは**値域の側** --- 合成 id の上限は
  // 安全整数よりはるかに小さいので、丸めが起きる値はどのみち max で落ちる。
  it('不正・0・負値・値域外の service は要素ごとに落とす（丸めない）', () => {
    // Number.MAX_SAFE_INTEGER を超える値は Number() の時点で別の値に丸まる。
    // 丸めた値を「利用者が指定した id」として通すと別チャンネルを指す。
    const unsafeId = Number.MAX_SAFE_INTEGER + 2
    expect(
      parseRecordingsSearch({ service: ['bad', 0, -1, 1.5, unsafeId, 3273601024] }),
    ).toEqual({ service: [3273601024] })
  })

  it('service は Service.id、site は別軸として保つ', () => {
    const search = parseRecordingsSearch({
      site: ['site2', 'default', 'site2'],
      service: [600101, 400101],
    })
    // どちらの軸も重複除去して正準化する（順序が揺れると queryKey が変わる）。
    expect(search).toEqual({ site: ['default', 'site2'], service: [400101, 600101] })
    expect(buildListRecordingsParams(search, false)).toEqual({
      trash: false,
      site: ['default', 'site2'],
      service: [400101, 600101],
    })
  })

  // `Number('') === 0` なので、空文字を弾かないと `?genre=` が
  // 「ニュース・報道（0）で絞る」に化ける。`genre` は zod 側が min(0) なので、
  // `asNumber` の空文字判定がこの軸の唯一の防壁になる。
  it('空文字の genre は 0 に化けず落ちる', () => {
    expect(parseRecordingsSearch({ genre: '' })).toEqual({})
    // 空白のみも同じ（`Number(' ') === 0`）。
    expect(parseRecordingsSearch({ genre: '  ' })).toEqual({})
    expect(parseRecordingsSearch({ genre: ['', '1'] })).toEqual({ genre: [1] })
  })

  // ⑧ 単一値スキーマに配列が来たときは先頭を採る（`validValue` の契約）。
  it('単一値の軸に配列が来たら先頭を採る', () => {
    expect(parseRecordingsSearch({ status: ['finished', 'failed'] })).toEqual({ status: 'finished' })
  })

  it('site 名の構文に合わない要素は落とす', () => {
    expect(parseRecordingsSearch({ site: ['Tokyo', '-bad', 'ok2'] })).toEqual({ site: ['ok2'] })
  })

  // TanStack Router の既定 parseSearch は `?site=123` を**数値**にして渡す。
  // site 名は全数字でも構文上合法（internal/config の mirakcSiteNamePattern）
  // なので、数値のまま文字列スキーマに掛けると実在の site が落ちる。
  it('全数字の site 名（数値で届く）も受け取る', () => {
    expect(parseRecordingsSearch({ site: 123 })).toEqual({ site: ['123'] })
    expect(parseRecordingsSearch({ site: [123, 'tokyo'] })).toEqual({ site: ['123', 'tokyo'] })
  })

  it('空文字列の q は「指定なし」に落ちる', () => {
    expect(parseRecordingsSearch({ q: '' })).toEqual({})
  })

  it('genre がすべて範囲外なら genre キー自体を作らない（空配列を作らない）', () => {
    expect(parseRecordingsSearch({ genre: [99, -1] })).toEqual({})
  })

  // rules.id は bigint（Go 側 int64 バインド）なので、非整数を送ると 400 になる。
  // Number.isFinite だけでは 1.5 を通してしまうので Number.isSafeInteger も見る
  // （isInteger も含む上位互換。issue #275 で isSafeInteger に揃えた）。
  it('非整数の ruleId は落とす', () => {
    expect(parseRecordingsSearch({ ruleId: 1.5 })).toEqual({})
    expect(parseRecordingsSearch({ ruleId: '1.5' })).toEqual({})
    expect(parseRecordingsSearch({ ruleId: 5 })).toEqual({ ruleId: 5 })
  })

  it('from/to は解釈できる日時なら ISO 8601（UTC）へ正規化する', () => {
    const result = parseRecordingsSearch({ from: '2026-01-01T09:00:00+09:00' })
    expect(result.from).toBe('2026-01-01T00:00:00.000Z')
  })

  // issue #275: parseRuleId が空文字を 0 に、非安全整数を黙って丸めていた。
  // 直す前の実装ではそれぞれ ''→0, '0'→0, '-1'→-1, '1e30'→1e+30,
  // '9007199254740993'→9007199254740992 を返していた（実測。丸めが
  // 「別のルールを指す値」を黙って作っていた）。
  it.each<[string, unknown]>([
    ['空文字列は「欠落」であり id 0 ではない', ''],
    ['空白のみも同様', '   '],
    ['0 はルール id として存在しない（シーケンス由来で 1 以上）', 0],
    ['0（文字列）', '0'],
    ['負値も同様に存在しない', -1],
    ['負値（文字列）', '-1'],
    ['安全整数の外は「別の id に丸まった値」であり利用者が書いた id ではない', 1e30],
    ['安全整数の外（文字列）', '1e30'],
    // 数値リテラルとして書くと oxlint(no-loss-of-precision) に引っかかる
    // （リテラル自体が構文解析時に丸まる、というこのテストの主張と同じ理由）ので
    // `Number()` 経由で同じ丸め後の値を作る。
    ['MAX_SAFE_INTEGER を超える値は黙って別の値に丸まる経路を塞ぐ', Number('9007199254740993')],
    ['同上（文字列）', '9007199254740993'],
    ['数値に変換できない文字列', 'not-a-number'],
    ['数値でも文字列でもない値', true],
  ])('%s は undefined に落ちる', (_label, raw) => {
    expect(parseRecordingsSearch({ ruleId: raw }).ruleId).toBeUndefined()
  })

  // 指数表記は数値として一意なので通す（parseAt と同じ流儀。文字列形の門
  // （`/^\d+$/` 等）を足すと `+5` や前後空白まで落ちてしまう一方、指数表記を
  // 拒む理由にはならない）。
  it.each<[string, unknown, number]>([
    ['整数', 1, 1],
    ['整数（文字列）', '1', 1],
    ['指数表記', 1e3, 1000],
    ['指数表記（文字列）', '1e3', 1000],
  ])('%s は通す', (_label, raw, expected) => {
    expect(parseRecordingsSearch({ ruleId: raw }).ruleId).toBe(expected)
  })
})

describe('hasAnyRecordingsCondition', () => {
  it('空の条件は false', () => {
    expect(hasAnyRecordingsCondition(emptyRecordingsSearch())).toBe(false)
  })

  it('order だけの指定は条件に数えない（並び順は絞り込みではない）', () => {
    expect(hasAnyRecordingsCondition({ order: 'asc' })).toBe(false)
  })

  it.each<[string, RecordingsPageSearch]>([
    ['q', { q: 'ニュース' }],
    ['genre', { genre: [1] }],
    ['service', { service: [3273601024] }],
    ['site', { site: ['site2'] }],
    ['status', { status: 'failed' }],
    ['source', { source: 'manual' }],
    ['ruleId', { ruleId: 3 }],
    ['from', { from: '2026-01-01T00:00:00Z' }],
    ['to', { to: '2026-01-01T00:00:00Z' }],
  ])('%s が指定されていれば true', (_label, search) => {
    expect(hasAnyRecordingsCondition(search)).toBe(true)
  })

  it('空文字列の q は条件に数えない', () => {
    expect(hasAnyRecordingsCondition({ q: '   ' })).toBe(false)
  })
})

describe('buildListRecordingsParams', () => {
  it('空の条件は trash だけを持つパラメータになる', () => {
    expect(buildListRecordingsParams(emptyRecordingsSearch(), false)).toEqual({ trash: false })
    expect(buildListRecordingsParams(emptyRecordingsSearch(), true)).toEqual({ trash: true })
  })

  it('指定した次元をすべてパラメータに落とす', () => {
    const search: RecordingsPageSearch = {
      q: 'ニュース',
      genre: [0, 1],
      service: [3273601024],
      status: 'failed',
      source: 'manual',
      ruleId: 5,
      from: '2026-01-01T00:00:00.000Z',
      to: '2026-01-02T00:00:00.000Z',
      order: 'asc',
    }
    expect(buildListRecordingsParams(search, false)).toEqual({
      trash: false,
      q: 'ニュース',
      genre: [0, 1],
      service: [3273601024],
      status: 'failed',
      source: 'manual',
      ruleId: 5,
      from: '2026-01-01T00:00:00.000Z',
      to: '2026-01-02T00:00:00.000Z',
      order: 'asc',
    })
  })

  it('空白だけの q は送らない', () => {
    expect(buildListRecordingsParams({ q: '   ' }, false)).toEqual({ trash: false })
  })
})

describe('clearRecordingsFilters', () => {
  it('order 以外の条件を全部外す', () => {
    const search: RecordingsPageSearch = { q: 'x', genre: [1], status: 'failed', order: 'asc' }
    expect(clearRecordingsFilters(search)).toEqual({ order: 'asc' })
  })

  it('order が無ければ空の条件になる', () => {
    expect(clearRecordingsFilters({ q: 'x' })).toEqual({ order: undefined })
  })
})

describe('datetime-local と ISO の相互変換', () => {
  it('往復で値が保たれる', () => {
    const iso = new Date(2026, 0, 15, 9, 30, 0, 0).toISOString()
    const local = isoToLocalDateTimeInput(iso)
    expect(localDateTimeInputToIso(local)).toBe(iso)
  })

  it('未指定は空文字列、空文字列は未指定に戻る', () => {
    expect(isoToLocalDateTimeInput(undefined)).toBe('')
    expect(localDateTimeInputToIso('')).toBeUndefined()
  })
})

describe('describeRecordingsFilters', () => {
  const services = new Map<number, string>([[3273601024, 'ＮＨＫ総合 (default)']])

  it('条件が無ければチップも無い', () => {
    expect(describeRecordingsFilters(emptyRecordingsSearch(), services)).toEqual([])
  })

  it('ジャンル・チャンネルは値ごとに 1 チップになり、外すとその値だけ落ちる', () => {
    const search: RecordingsPageSearch = { genre: [0, 1], service: [3273601024] }
    const chips = describeRecordingsFilters(search, services)
    expect(chips.map((c) => c.label)).toEqual([
      'ジャンル: ニュース・報道',
      'ジャンル: スポーツ',
      'チャンネル: ＮＨＫ総合 (default)',
    ])

    const genreChip = chips.find((c) => c.key === 'genre-0')
    expect(genreChip?.clear(search)).toEqual({ genre: [1], service: [3273601024] })
  })

  it('最後の 1 件を外すと配列キー自体が消える（空配列を残さない）', () => {
    const search: RecordingsPageSearch = { genre: [0] }
    const chips = describeRecordingsFilters(search, services)
    expect(chips[0].clear(search)).toEqual({ genre: undefined })
  })

  // site 軸のチップ（ラベルと clear）。他の軸と同じく、押して消せること・
  // 最後の 1 つを消したらキーごと undefined になることを見る。
  it('site のチップを出し、押すとその値だけ消える', () => {
    const search: RecordingsPageSearch = { site: ['default', 'site2'] }
    const chips = describeRecordingsFilters(search, services)
    const siteChips = chips.filter((c) => c.key.startsWith('site-'))
    expect(siteChips.map((c) => c.label)).toEqual(['サイト: default', 'サイト: site2'])
    expect(siteChips[0].clear(search).site).toEqual(['site2'])
    expect(siteChips[0].clear({ site: ['default'] }).site).toBeUndefined()
  })

  it('チャンネル名が分からない service は id で出す', () => {
    const search: RecordingsPageSearch = { service: [9999] }
    const chips = describeRecordingsFilters(search, services)
    expect(chips[0].label).toBe('チャンネル: チャンネル #9999')
  })

  it('状態・種別・ルール・期間はスカラーの 1 チップになる', () => {
    const search: RecordingsPageSearch = {
      status: 'failed',
      source: 'manual',
      ruleId: 7,
      from: '2026-01-01T00:00:00Z',
    }
    const chips = describeRecordingsFilters(search, services)
    expect(chips.map((c) => c.key)).toEqual(['status', 'source', 'ruleId', 'period'])
    expect(chips.find((c) => c.key === 'status')?.label).toBe('状態: 失敗')
    expect(chips.find((c) => c.key === 'ruleId')?.label).toBe('ルール #7')
    // 「期間指定」のような値の読めないラベルにしない --- チップだけで何を
    // 絞っているか分かる必要がある（レビューで指摘）。
    expect(chips.find((c) => c.key === 'period')?.label).toBe(
      `期間: ${formatDateTime('2026-01-01T00:00:00Z')} 〜`,
    )
  })

  it('期間チップは from/to 両方あれば範囲を、片方だけなら開いた側を「〜」で示す', () => {
    const both = describeRecordingsFilters(
      { from: '2026-01-01T00:00:00Z', to: '2026-01-02T00:00:00Z' },
      services,
    )
    expect(both[0].label).toBe(
      `期間: ${formatDateTime('2026-01-01T00:00:00Z')} 〜 ${formatDateTime('2026-01-02T00:00:00Z')}`,
    )

    const toOnly = describeRecordingsFilters({ to: '2026-01-02T00:00:00Z' }, services)
    expect(toOnly[0].label).toBe(`期間: 〜 ${formatDateTime('2026-01-02T00:00:00Z')}`)
  })

  it('期間チップを外すと from と to が両方消える', () => {
    const search: RecordingsPageSearch = {
      from: '2026-01-01T00:00:00Z',
      to: '2026-01-02T00:00:00Z',
    }
    const chips = describeRecordingsFilters(search, services)
    expect(chips[0].clear(search)).toEqual({ from: undefined, to: undefined })
  })

  // キーワードはチップにしない（検索欄自体が値を表示しているため）。
  it('q はチップにならない', () => {
    expect(describeRecordingsFilters({ q: 'ニュース' }, services)).toEqual([])
  })
})

// site の正準化はチップ側（`Array.prototype.sort()` = コード単位順）と
// 一致していなければならない。`localeCompare` にするとロケール依存になり、
// 同じ共有 URL がブラウザによって違う並びになる。
describe('site の並びはチップ側と同じ正準形になる', () => {
  it('コード単位順（localeCompare とは違う並びになる組で確認）', () => {
    // 'a0b' < 'a_b' はコード単位順（'0' = 0x30 < '_' = 0x5F）。
    // ICU の既定照合では逆順になる。
    expect(parseRecordingsSearch({ site: ['a_b', 'a0b'] }).site).toEqual(['a0b', 'a_b'])
    expect(['a_b', 'a0b'].sort()).toEqual(['a0b', 'a_b'])
  })
})
