import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'

import {
  allWeekdays,
  buildSearchRequest,
  draftError,
  emptyDraft,
  genreCodeLabel,
  hasWeekday,
  newTimeWindow,
  secToTimeValue,
  timeValueToSec,
  toggleWeekday,
  type SearchDraft,
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
