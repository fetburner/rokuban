import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'

import type { Rule } from '@/api/generated'
import { SearchProgramsBody } from '@/api/zod'
import {
  allWeekdays,
  buildRuleInput,
  buildSearchRequest,
  canonicalSearchConditions,
  draftError,
  emptyDraft,
  emptyRuleMeta,
  genreCodeLabel,
  hasWeekday,
  newTimeWindow,
  ruleMetaError,
  conditionsToDraft,
  ruleToMeta,
  type RuleMetaDraft,
  type SearchDraft,
  secToTimeValue,
  timeValueToSec,
  toggleWeekday,
} from '@/lib/program-search'

function draft(patch: Partial<SearchDraft> = {}): SearchDraft {
  return { ...emptyDraft(), ...patch }
}

describe('buildSearchRequest', () => {
  it('何も指定していない下書きは空のリクエストになる', () => {
    // 「問わない」次元をキーとして送らないこと。null を送っても API の意味は
    // 同じだが、リクエストを見て「何を指定したか」が読めなくなる
    expect(buildSearchRequest(emptyDraft())).toEqual({})
  })

  it('無料放送の 3 値を boolean に落とす', () => {
    expect(buildSearchRequest(draft({ isFree: 'yes' }))).toEqual({ isFree: true })
    expect(buildSearchRequest(draft({ isFree: 'no' }))).toEqual({ isFree: false })
    expect(buildSearchRequest(draft({ isFree: 'any' })).isFree).toBeUndefined()
  })

  it('放送時間は分をミリ秒にする', () => {
    const request = buildSearchRequest(
      draft({ durationMinMinutes: '30', durationMaxMinutes: '120' }),
    )
    expect(request.durationMinMs).toBe(1_800_000)
    expect(request.durationMaxMs).toBe(7_200_000)
  })

  it('放送時間の 0 分は「指定なし」と区別する', () => {
    // 空欄は送らない。0 は送る（0 分以上 = 全件だが、ユーザーが打った値である）
    expect(buildSearchRequest(draft({ durationMinMinutes: '' })).durationMinMs).toBeUndefined()
    expect(buildSearchRequest(draft({ durationMinMinutes: '0' })).durationMinMs).toBe(0)
  })

  describe('期間', () => {
    // **タイムゾーンを固定する。** このマシン（と CI）が UTC だと、
    // 「末尾に Z を足すだけ」の実装でもローカル時刻の検証が通ってしまう
    // （CLAUDE.md の「非同期の空虚な成功」と同じ、条件が揃わず検証にならない形）。
    beforeAll(() => {
      vi.stubEnv('TZ', 'Asia/Tokyo')
    })
    afterAll(() => {
      vi.unstubAllEnvs()
    })

    it('datetime-local の値をローカル時刻として読み、UTC の ISO で送る', () => {
      const request = buildSearchRequest(
        draft({ periodStartAt: '2026-07-29T21:30', periodEndAt: '2026-08-05T00:00' }),
      )
      expect(request.periodStartAt).toBe('2026-07-29T12:30:00.000Z')
      expect(request.periodEndAt).toBe('2026-08-04T15:00:00.000Z')
    })

    it('空欄の期間は送らない', () => {
      const request = buildSearchRequest(draft({ periodStartAt: '2026-07-29T21:30' }))
      expect(request.periodEndAt).toBeUndefined()
    })
  })

  it('テキスト条件は既定値と同じフラグを送らない', () => {
    const request = buildSearchRequest(
      draft({
        textMatches: [
          { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: false, negate: false },
          { target: 'description', mode: 'regex', value: '^再', caseSensitive: true, negate: true },
        ],
      }),
    )
    expect(request.textMatches).toEqual([
      { target: 'name', mode: 'keyword', value: 'ニュース' },
      { target: 'description', mode: 'regex', value: '^再', caseSensitive: true, negate: true },
    ])
  })

  it('サービス・チャンネル種別・時間帯をそのまま渡す', () => {
    const request = buildSearchRequest(
      draft({
        services: [{ networkId: 32736, serviceId: 1024 }],
        channelTypes: ['BS', 'GR'],
        times: [{ weekdays: 0b0000001, startSec: 75_600, endSec: 82_800 }],
      }),
    )
    expect(request.services).toEqual([{ networkId: 32736, serviceId: 1024 }])
    expect(request.channelTypes).toEqual(['BS', 'GR'])
    expect(request.times).toEqual([{ weekdays: 1, startSec: 75_600, endSec: 82_800 }])
  })

  it('ジャンルは選んだ順ではなくコード順で送る', () => {
    expect(buildSearchRequest(draft({ genres: [7, 0, 3] })).genres).toEqual([0, 3, 7])
  })
})

describe('draftError', () => {
  it('問題のない下書きでは undefined', () => {
    expect(
      draftError(
        draft({
          textMatches: [
            { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: false, negate: false },
          ],
          times: [{ weekdays: allWeekdays, startSec: 75_600, endSec: 82_800 }],
          durationMinMinutes: '30',
        }),
      ),
    ).toBeUndefined()
  })

  it('値が空のテキスト条件を止める', () => {
    // keyword なら LIKE '%%'、regex なら ~ '' で全件にマッチする。
    // 絞り込んだつもりで絞り込めていない状態を送らせない
    expect(
      draftError(
        draft({
          textMatches: [
            { target: 'name', mode: 'keyword', value: '', caseSensitive: false, negate: false },
          ],
        }),
      ),
    ).toBe('テキスト条件の値を入力してください')
  })

  it('曜日が 1 つも選ばれていない時間帯を止める', () => {
    // weekdays の下限は 1。0 を送ると rulequery が範囲外エラーを返し、
    // API はそれを 400 に変換しないので 500 になる
    expect(draftError(draft({ times: [{ weekdays: 0, startSec: 0, endSec: 3600 }] }))).toBe(
      '時間帯には曜日を 1 つ以上選んでください',
    )
  })

  it('幅ゼロの時間帯を止める', () => {
    // sec >= X AND sec < X は決してマッチしない。追加直後の時間帯がこれ
    expect(draftError(draft({ times: [newTimeWindow()] }))).toBe(
      '時間帯の開始と終了には違う時刻を指定してください',
    )
  })

  it('翌日跨ぎの時間帯は止めない', () => {
    // 終了 < 開始 は「翌日跨ぎ」として rulequery が解釈する正しい指定
    expect(
      draftError(draft({ times: [{ weekdays: allWeekdays, startSec: 82_800, endSec: 3600 }] })),
    ).toBeUndefined()
  })

  it('放送時間に数値でない値を止める', () => {
    expect(draftError(draft({ durationMinMinutes: '-5' }))).toBe(
      '放送時間の下限には 0 以上の分数を入力してください',
    )
    expect(draftError(draft({ durationMaxMinutes: 'abc' }))).toBe(
      '放送時間の上限には 0 以上の分数を入力してください',
    )
  })

  it('下限が上限より大きくても止めない（0 件という答えが正しい）', () => {
    expect(
      draftError(draft({ durationMinMinutes: '120', durationMaxMinutes: '30' })),
    ).toBeUndefined()
  })
})

describe('曜日ビットと時刻の変換', () => {
  it('bit0 が月曜、bit6 が日曜', () => {
    expect(hasWeekday(0b0000001, 0)).toBe(true)
    expect(hasWeekday(0b0000001, 6)).toBe(false)
    expect(hasWeekday(0b1000000, 6)).toBe(true)
    expect(hasWeekday(allWeekdays, 3)).toBe(true)
  })

  it('toggleWeekday は該当ビットだけを反転する', () => {
    expect(toggleWeekday(0, 2)).toBe(0b0000100)
    expect(toggleWeekday(allWeekdays, 0)).toBe(126)
    expect(toggleWeekday(toggleWeekday(0b0010010, 4), 4)).toBe(0b0010010)
  })

  it('時刻と秒を往復できる', () => {
    expect(secToTimeValue(0)).toBe('00:00')
    expect(secToTimeValue(75_600)).toBe('21:00')
    expect(secToTimeValue(82_980)).toBe('23:03')
    expect(timeValueToSec('21:00')).toBe(75_600)
    expect(timeValueToSec('00:00')).toBe(0)
    expect(timeValueToSec('')).toBe(0)
  })
})

describe('genreCodeLabel', () => {
  it('知っているコードは日本語ラベル', () => {
    expect(genreCodeLabel(3)).toBe('ドラマ')
    expect(genreCodeLabel(15)).toBe('その他')
  })

  it('知らないコード（予備）は数値のまま出す', () => {
    // 「その他」に丸めると ARIB の本物の「その他」（15）と区別できなくなる
    expect(genreCodeLabel(12)).toBe('ジャンル 12')
  })
})

/**
 * fullRule は全次元を埋めたルール（往復テストの基準値）。
 *
 * textMatches は 2 件用意し、1 件目は caseSensitive/negate を明示的に
 * true にして往復させ、2 件目は省略（既定 false）のまま往復させる —— 両方が
 * 保たれることを 1 つの fixture で確かめる。
 */
function fullRule(overrides: Partial<Rule> = {}): Rule {
  return {
    id: 1,
    name: '元のルール',
    description: 'テスト用ルール',
    enabled: true,
    priority: 5,
    isFree: true,
    durationMinMs: 1_800_000,
    durationMaxMs: 7_200_000,
    periodStartAt: '2026-07-29T12:30:00.000Z',
    periodEndAt: '2026-08-04T15:00:00.000Z',
    textMatches: [
      { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: true, negate: true },
      { target: 'description', mode: 'regex', value: '^再' },
    ],
    services: [{ networkId: 32736, serviceId: 1024 }],
    channelTypes: ['BS', 'GR'],
    genres: [7, 0, 3],
    times: [{ weekdays: 0b0000001, startSec: 75_600, endSec: 82_800 }],
    sites: ['default', 'takamatsu'],
    dedupeEnabled: true,
    dedupeThreshold: 0.8,
    dedupeWindowSeconds: 3600,
    keepOriginal: 'always',
    encodeProfiles: [],
    filenameTemplate: '{title}',
    metadata: { foo: 'bar' },
    createdAt: '2026-01-01T00:00:00.000Z',
    updatedAt: '2026-01-01T00:00:00.000Z',
    ...overrides,
  }
}

describe('conditionsToDraft と buildRuleInput の往復', () => {
  // **タイムゾーンを固定する。** このマシン（と CI）が UTC だと、末尾に Z を
  // 足すだけの実装でもローカル時刻の往復が通ってしまう
  // （program-search.test.ts の既存の「期間」テストと同じ理由）。
  beforeAll(() => {
    vi.stubEnv('TZ', 'Asia/Tokyo')
  })
  afterAll(() => {
    vi.unstubAllEnvs()
  })

  it('全次元（テキスト条件・サービス・チャンネル種別・ジャンル・時間帯・放送時間・期間）を保つ', () => {
    const rule = fullRule()
    const draft = conditionsToDraft(rule)
    const meta = ruleToMeta(rule)
    const input = buildRuleInput(draft, meta, rule)

    expect(input).toEqual({
      name: rule.name,
      enabled: rule.enabled,
      priority: rule.priority,
      keepOriginal: rule.keepOriginal,
      encodeProfiles: rule.encodeProfiles,
      isFree: true,
      durationMinMs: 1_800_000,
      durationMaxMs: 7_200_000,
      periodStartAt: '2026-07-29T12:30:00.000Z',
      periodEndAt: '2026-08-04T15:00:00.000Z',
      textMatches: [
        { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: true, negate: true },
        { target: 'description', mode: 'regex', value: '^再' },
      ],
      services: [{ networkId: 32736, serviceId: 1024 }],
      channelTypes: ['BS', 'GR'],
      // ジャンルは buildSearchRequest がコード順に並べ替える
      genres: [0, 3, 7],
      times: [{ weekdays: 1, startSec: 75_600, endSec: 82_800 }],
      description: 'テスト用ルール',
      dedupeEnabled: true,
      dedupeThreshold: 0.8,
      dedupeWindowSeconds: 3600,
      filenameTemplate: '{title}',
      metadata: { foo: 'bar' },
      sites: ['default', 'takamatsu'],
    })
  })

  it('isFree の 3 値（yes / no / 問わない）を保つ', () => {
    for (const [isFree, expected] of [
      [true, true],
      [false, false],
      [undefined, undefined],
    ] as const) {
      const rule = fullRule({ isFree })
      const input = buildRuleInput(conditionsToDraft(rule), ruleToMeta(rule))
      expect(input.isFree).toBe(expected)
    }
  })

  it('放送時間・期間が無いルールは空欄に、空欄の下書きはキーごと落ちる', () => {
    const rule = fullRule({
      durationMinMs: undefined,
      durationMaxMs: undefined,
      periodStartAt: undefined,
      periodEndAt: undefined,
    })
    const draft = conditionsToDraft(rule)
    expect(draft.durationMinMinutes).toBe('')
    expect(draft.durationMaxMinutes).toBe('')
    expect(draft.periodStartAt).toBe('')
    expect(draft.periodEndAt).toBe('')

    const input = buildRuleInput(draft, ruleToMeta(rule))
    expect(input.durationMinMs).toBeUndefined()
    expect(input.durationMaxMs).toBeUndefined()
    expect(input.periodStartAt).toBeUndefined()
    expect(input.periodEndAt).toBeUndefined()
  })
})

describe('分 ⇄ ms の変換（conditionsToDraft）', () => {
  it('durationMs を分の文字列にする', () => {
    const rule = fullRule({ durationMinMs: 1_800_000, durationMaxMs: 7_200_000 })
    const draft = conditionsToDraft(rule)
    expect(draft.durationMinMinutes).toBe('30')
    expect(draft.durationMaxMinutes).toBe('120')
  })

  it('0 ms は「指定なし」と区別する（空欄にしない）', () => {
    const rule = fullRule({ durationMinMs: 0 })
    expect(conditionsToDraft(rule).durationMinMinutes).toBe('0')
  })
})

describe('ローカル時刻 ⇄ UTC ISO の変換（conditionsToDraft）', () => {
  beforeAll(() => {
    vi.stubEnv('TZ', 'Asia/Tokyo')
  })
  afterAll(() => {
    vi.unstubAllEnvs()
  })

  it('UTC の ISO をローカル時刻の datetime-local 値にする', () => {
    const rule = fullRule({
      periodStartAt: '2026-07-29T12:30:00.000Z',
      periodEndAt: '2026-08-04T15:00:00.000Z',
    })
    const draft = conditionsToDraft(rule)
    // JST は UTC+9。日付が繰り上がる終了日時（15:00 UTC → 翌 00:00 JST）を
    // 含めることで、時刻だけでなく日付の繰り上がりも見る
    expect(draft.periodStartAt).toBe('2026-07-29T21:30')
    expect(draft.periodEndAt).toBe('2026-08-05T00:00')
  })
})

describe('buildRuleInput の preserve', () => {
  it('preserve を渡すと UI を持たない項目（description / dedupe* / filenameTemplate / metadata / sites）を引き継ぐ', () => {
    const rule = fullRule()
    const input = buildRuleInput(emptyDraft(), emptyRuleMeta(), rule)

    expect(input.description).toBe('テスト用ルール')
    expect(input.dedupeEnabled).toBe(true)
    expect(input.dedupeThreshold).toBe(0.8)
    expect(input.dedupeWindowSeconds).toBe(3600)
    expect(input.filenameTemplate).toBe('{title}')
    expect(input.metadata).toEqual({ foo: 'bar' })
    expect(input.sites).toEqual(['default', 'takamatsu'])
  })

  it('preserve を渡さない（新規作成）ときはこれらを一切送らない', () => {
    const input = buildRuleInput(emptyDraft(), emptyRuleMeta())

    expect(input.description).toBeUndefined()
    expect(input.dedupeEnabled).toBeUndefined()
    expect(input.dedupeThreshold).toBeUndefined()
    expect(input.dedupeWindowSeconds).toBeUndefined()
    expect(input.filenameTemplate).toBeUndefined()
    expect(input.metadata).toBeUndefined()
    // sites は検索フォームが出していない次元なので、preserve が無ければ
    // 常に付かない（空 = 全サイト）
    expect(input.sites).toBeUndefined()
  })
})

describe('emptyRuleMeta / ruleToMeta', () => {
  it('emptyRuleMeta は新規作成の既定値', () => {
    expect(emptyRuleMeta()).toEqual({
      name: '',
      enabled: true,
      priority: '10',
      keepOriginal: 'always',
      encodeProfiles: [],
    })
  })

  it('ruleToMeta は既存ルールをそのままフォーム状態にする', () => {
    const rule = fullRule({ name: 'ニュース全部', priority: 20, encodeProfiles: ['h264'] })
    expect(ruleToMeta(rule)).toEqual({
      name: 'ニュース全部',
      enabled: true,
      priority: '20',
      keepOriginal: 'always',
      encodeProfiles: ['h264'],
    })
  })
})

describe('ruleMetaError', () => {
  function meta(patch: Partial<RuleMetaDraft> = {}): RuleMetaDraft {
    return { ...emptyRuleMeta(), name: 'ニュース', ...patch }
  }

  it('名前が空（空白のみ含む）なら止める', () => {
    expect(ruleMetaError(meta({ name: '' }))).toBe('名前は必須です')
    expect(ruleMetaError(meta({ name: '   ' }))).toBe('名前は必須です')
  })

  it('until_encoded かつプロファイル未選択なら止める', () => {
    expect(ruleMetaError(meta({ keepOriginal: 'until_encoded', encodeProfiles: [] }))).toBe(
      'エンコード後に原本を削除するには、プロファイルを 1 つ以上選んでください',
    )
  })

  it('until_encoded でもプロファイルを選んでいれば止めない', () => {
    expect(
      ruleMetaError(meta({ keepOriginal: 'until_encoded', encodeProfiles: ['h264'] })),
    ).toBeUndefined()
  })

  it('always なら常に止めない', () => {
    expect(ruleMetaError(meta({ keepOriginal: 'always', encodeProfiles: [] }))).toBeUndefined()
  })
})

/**
 * URL（`?cond=`）と localStorage に載せた条件は openapi 由来の zod スキーマを
 * 通して読む。**そのスキーマは既定値を埋める**ので、送ったリクエストと読み戻した
 * 条件は文字列として一致しない --- `pages/search.tsx` が「適用済みか」を生の JSON で
 * 判定すると、自分で書いた URL を別の条件と誤認して同じ検索を 2 回叩く。
 *
 * 「既定値なんて埋まらないでしょ」とガードを単純化する手前で止めるため、
 * **埋まること**と**畳めば元に戻ること**の両方をここで固定する（実ブラウザでの
 * 「押下 1 回 = 検索 1 回」は `e2e/personalization.mjs` ③）。
 */
describe('canonicalSearchConditions（URL・localStorage から読んだ条件の畳み込み）', () => {
  const request = buildSearchRequest({
    ...emptyDraft(),
    textMatches: [
      { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: false, negate: false },
    ],
  })

  it('openapi のスキーマを通すと既定値が埋まり、送った形と一致しなくなる', () => {
    const parsed = SearchProgramsBody.parse(request)
    expect(request).toEqual({ textMatches: [{ target: 'name', mode: 'keyword', value: 'ニュース' }] })
    expect(parsed).toEqual({
      textMatches: [
        { target: 'name', mode: 'keyword', value: 'ニュース', caseSensitive: false, negate: false },
      ],
    })
  })

  it('畳むと送った形に戻る', () => {
    expect(canonicalSearchConditions(SearchProgramsBody.parse(request))).toEqual(request)
  })

  it('フォームに無い次元（sites）だけの条件は空に畳む', () => {
    // 畳まないと「画面には何も出ていないのに全件検索が走る」（routes.tsx の cond）
    expect(canonicalSearchConditions({ sites: ['default'] })).toEqual({})
  })
})
