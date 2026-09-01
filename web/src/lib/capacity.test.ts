import { describe, expect, it } from 'vitest'

import type { CapacityOverage } from '@/api/generated'
import {
  countProgramsInShortfall,
  coveringWindow,
  intersectingOverages,
  shortageLabel,
  shortageLabelCompact,
  shortageMessage,
  shortageRangeMessage,
  shortfallDetail,
  worstOverage,
} from '@/lib/capacity'

/** 時刻はローカルの 0 時基準で組む（文言に時刻が入るのでタイムゾーンに依存させない）。 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

/** at は 0 時からの分数を ISO 文字列に直す。 */
function at(minutes: number): string {
  return new Date(dayStart.getTime() + minutes * 60_000).toISOString()
}

/** ms は 0 時からの分数を epoch ms に直す。 */
function ms(minutes: number): number {
  return dayStart.getTime() + minutes * 60_000
}

function overage(
  startMinutes: number,
  endMinutes: number,
  options: Partial<CapacityOverage> = {},
): CapacityOverage {
  return {
    site: 'default',
    startAt: at(startMinutes),
    endAt: at(endMinutes),
    shortfall: 1,
    jammedTypes: ['BS'],
    ...options,
  }
}

describe('超過区間の交差', () => {
  it('区間と重なる予約だけを拾う', () => {
    const overages = [overage(19 * 60, 20 * 60)]

    // 内側 / 前後にまたぐ / 完全に含む
    expect(intersectingOverages(overages, 'default', ms(19 * 60 + 10), ms(19 * 60 + 20))).toHaveLength(1)
    expect(intersectingOverages(overages, 'default', ms(18 * 60), ms(19 * 60 + 1))).toHaveLength(1)
    expect(intersectingOverages(overages, 'default', ms(18 * 60), ms(21 * 60))).toHaveLength(1)
    // 完全に外
    expect(intersectingOverages(overages, 'default', ms(21 * 60), ms(22 * 60))).toHaveLength(0)
  })

  it('端で接するだけなら交差しない（半開区間）', () => {
    const overages = [overage(19 * 60, 20 * 60)]

    // 19:00 に終わる予約 / 20:00 に始まる予約はどちらも不足の外側
    expect(intersectingOverages(overages, 'default', ms(18 * 60), ms(19 * 60))).toHaveLength(0)
    expect(intersectingOverages(overages, 'default', ms(20 * 60), ms(21 * 60))).toHaveLength(0)
    // 1ms でも食い込めば交差する（境界の判定が反転していないことの確認）
    expect(intersectingOverages(overages, 'default', ms(18 * 60), ms(19 * 60) + 1)).toHaveLength(1)
    expect(intersectingOverages(overages, 'default', ms(20 * 60) - 1, ms(21 * 60))).toHaveLength(1)
  })

  it('別サイトの区間は交差させない（判定はサイトごとに独立）', () => {
    const overages = [
      overage(19 * 60, 20 * 60, { site: 'default' }),
      overage(19 * 60, 20 * 60, { site: 'takamatsu' }),
    ]

    const found = intersectingOverages(overages, 'default', ms(19 * 60), ms(20 * 60))
    expect(found).toHaveLength(1)
    expect(found[0].site).toBe('default')
    // 高松のチューナー不足は高松の予約にだけ効く
    expect(intersectingOverages(overages, 'takamatsu', ms(19 * 60), ms(20 * 60))).toHaveLength(1)
    expect(intersectingOverages(overages, 'osaka', ms(19 * 60), ms(20 * 60))).toHaveLength(0)
  })
})

describe('複数区間に跨るとき', () => {
  it('最も不足の大きい区間を採り、種別を合併しない', () => {
    const worst = worstOverage([
      overage(19 * 60, 20 * 60, { shortfall: 1, jammedTypes: ['GR'] }),
      overage(20 * 60, 21 * 60, { shortfall: 2, jammedTypes: ['BS'] }),
    ])

    // 合併して「GR・BS が 3 本不足」と言うと、どの区間でも成り立たない主張になる
    expect(worst?.shortfall).toBe(2)
    expect(worst?.jammedTypes).toEqual(['BS'])
  })

  it('交差する区間が無ければ null（何も言わない）', () => {
    expect(worstOverage([])).toBeNull()
  })
})

describe('文言', () => {
  it('不足本数と詰まった種別を具体的に出す', () => {
    expect(shortfallDetail(overage(0, 60, { shortfall: 1, jammedTypes: ['BS'] }))).toBe(
      'BS が 1 本',
    )
    expect(shortfallDetail(overage(0, 60, { shortfall: 2, jammedTypes: ['GR', 'BS'] }))).toBe(
      'GR・BS が 2 本',
    )
    // 種別が無くても本数は出す（判定の副産物の欠落で警告そのものを消さない）
    expect(shortfallDetail(overage(0, 60, { shortfall: 1, jammedTypes: [] }))).toBe('1 本')
  })

  it('主語は時間帯であって予約ではない', () => {
    const message = shortageMessage(overage(19 * 60, 20 * 60))

    expect(message).toBe('この時間帯はチューナーが不足しています（BS が 1 本不足）')
    // 勝敗の主張になる語を持たない（どの予約が負けるかは mirakc が決める）
    expect(message).not.toContain('競合')
    expect(message).not.toContain('録画できません')
    expect(message).not.toContain('この予約')
  })

  it('バッジのラベルは短く、読み上げ用の文には時刻が入る', () => {
    expect(shortageLabel(overage(19 * 60, 20 * 60))).toBe('チューナー不足（BS が 1 本）')
    expect(shortageRangeMessage(overage(19 * 60, 20 * 60))).toBe(
      '19:00〜20:00 はチューナーが不足しています（BS が 1 本不足）',
    )
  })

  it('グリッドの時間軸列用はさらに短い形（種別 1 つなら種別も残す）', () => {
    expect(shortageLabelCompact(overage(19 * 60, 20 * 60, { jammedTypes: ['BS'] }))).toBe('BS-1')
    // 種別が 2 つ以上は列挙すると幅を食うので本数だけにする
    expect(
      shortageLabelCompact(overage(19 * 60, 20 * 60, { shortfall: 2, jammedTypes: ['GR', 'BS'] })),
    ).toBe('-2')
    // 種別が無くても本数は出す
    expect(shortageLabelCompact(overage(19 * 60, 20 * 60, { jammedTypes: [] }))).toBe('-1')
  })
})

describe('不足区間と重なる番組を数える', () => {
  const overages = [overage(19 * 60, 20 * 60)]

  it('半開区間の端は数えない（接するだけ / 1ms 食い込めば数える）', () => {
    const count = (startMinutes: number, durationMinutes: number) =>
      countProgramsInShortfall(overages, 'default', [
        { startAt: at(startMinutes), durationMs: durationMinutes * 60_000 },
      ])

    expect(count(18 * 60, 60)).toBe(0) // 19:00 に終わる
    expect(count(20 * 60, 60)).toBe(0) // 20:00 に始まる
    expect(count(19 * 60 + 10, 10)).toBe(1) // 内側
    expect(count(18 * 60, 30)).toBe(0) // 完全に外
    expect(
      countProgramsInShortfall(overages, 'default', [
        { startAt: at(18 * 60), durationMs: 60 * 60_000 + 1 },
      ]),
    ).toBe(1) // 1ms 食い込む
  })

  it('終了未定番組（幅 0 の区間）は開始の瞬間をまたぐ区間しか数えない', () => {
    const undetermined = [{ startAt: at(19 * 60 + 30), durationMs: 0 }]

    // 19:30 を厳密にまたぐ不足区間なら数える
    expect(countProgramsInShortfall(overages, 'default', undetermined)).toBe(1)
    // 不足区間の開始が番組開始と同時刻（端点は予約境界由来なので現実に起きうる）
    expect(countProgramsInShortfall([overage(19 * 60 + 30, 20 * 60)], 'default', undetermined)).toBe(0)
    // 20:00 開始・終了未定の最中に 21:00〜21:30 の不足がある形は原理的に数えられない
    expect(
      countProgramsInShortfall([overage(21 * 60, 21 * 60 + 30)], 'default', [
        { startAt: at(20 * 60), durationMs: 0 },
      ]),
    ).toBe(0)
  })

  it('別サイトの不足区間では数えない', () => {
    expect(
      countProgramsInShortfall(overages, 'takamatsu', [
        { startAt: at(19 * 60 + 10), durationMs: 10 * 60_000 },
      ]),
    ).toBe(0)
  })
})

describe('問い合わせる窓', () => {
  it('一覧の予約すべてを覆う', () => {
    const window = coveringWindow([
      { startAt: at(19 * 60), durationMs: 60 * 60_000 },
      { startAt: at(10 * 60), durationMs: 30 * 60_000 },
    ])

    expect(window).toEqual({ startMs: ms(10 * 60), endMs: ms(20 * 60) })
  })

  it('予約が無ければ null（訊く意味がない）', () => {
    expect(coveringWindow([])).toBeNull()
  })
})
