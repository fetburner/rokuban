import { describe, expect, it } from 'vitest'

import {
  buildHomeTimelineRows,
  homeTimelineDayLabel,
  homeTimelineTickLabel,
  packHomeTimelineEvents,
  sortHomeTimelineEvents,
} from '@/lib/home-timeline'

const event = (
  key: string,
  startMs: number,
  endMs: number,
  site = 'default',
  channelType = 'GR',
) => ({
  key,
  site,
  channelType,
  startMs,
  endMs,
  kind: 'reservation' as const,
  title: key,
  hasDrop: false,
  href: { to: '/reservations/$site/$programId' as const, site, programId: 1 },
})

describe('packHomeTimelineEvents', () => {
  it('時間が重ならない番組を同じ段へ入れる', () => {
    const packed = packHomeTimelineEvents([
      event('late', 60, 90),
      event('early', 0, 30),
    ])

    expect(packed.map(({ key, track }) => [key, track])).toEqual([
      ['early', 0],
      ['late', 0],
    ])
  })

  it('時間が重なる番組を別の段へ入れる', () => {
    const packed = packHomeTimelineEvents([
      event('first', 0, 60),
      event('overlap', 30, 90),
    ])

    expect(packed.map(({ key, track }) => [key, track])).toEqual([
      ['first', 0],
      ['overlap', 1],
    ])
  })

  it('終了時刻と次の開始時刻が等しければ同じ段を再利用する', () => {
    const packed = packHomeTimelineEvents([
      event('first', 0, 30),
      event('next', 30, 60),
    ])

    expect(packed.map(({ key, track }) => [key, track])).toEqual([
      ['first', 0],
      ['next', 0],
    ])
  })

  it('同時刻開始は入力順に依存せず、key順で段が安定する', () => {
    const packed = packHomeTimelineEvents([
      event('z-last', 0, 30),
      event('a-first', 0, 30),
    ])

    expect(packed.map(({ key, track }) => [key, track])).toEqual([
      ['a-first', 0],
      ['z-last', 1],
    ])
  })
})

describe('buildHomeTimelineRows', () => {
  it('jammedTypes の全行へ同じsiteの区間を載せる（予約への印が無いことは home.test.tsx が描画で見る）', () => {
    const recordings = [
      event('north-gr', 0, 30, 'north', 'GR'),
      event('north-bs', 0, 30, 'north', 'BS'),
      event('south-gr', 0, 30, 'south', 'GR'),
    ]
    const overages = [
      {
        site: 'north',
        startAt: new Date(10).toISOString(),
        endAt: new Date(20).toISOString(),
        shortfall: 2,
        jammedTypes: ['GR', 'BS'] as ('GR' | 'BS')[],
      },
    ]

    const rows = buildHomeTimelineRows(recordings, overages)

    expect(rows.map((row) => [row.site, row.channelType, row.overages.length])).toEqual([
      ['north', 'GR', 1],
      ['north', 'BS', 1],
      ['south', 'GR', 0],
    ])
  })

  it('データが無いサイト/種別の行を捏造しない', () => {
    expect(buildHomeTimelineRows([], [])).toEqual([])
  })
})

describe('sortHomeTimelineEvents', () => {
  it('入力の連結順ではなく、開始 → 終了 → key の順に並べ、入力を壊さない', () => {
    const input = [
      event('reservation', 300, 400),
      event('b-same', 100, 200),
      event('a-same', 100, 200),
      event('short', 100, 150),
    ]
    expect(sortHomeTimelineEvents(input).map((item) => item.key)).toEqual([
      'short',
      'a-same',
      'b-same',
      'reservation',
    ])
    expect(input[0]!.key).toBe('reservation')
  })
})

describe('日付ラベル', () => {
  const day = (d: number, h = 0) => new Date(2026, 6, d, h).getTime()

  it('目盛りは始端と同じ日は時だけ、翌日は「翌」を付ける', () => {
    expect(homeTimelineTickLabel(day(25, 18), day(25))).toBe('18時')
    expect(homeTimelineTickLabel(day(25, 0), day(25))).toBe('0時')
    expect(homeTimelineTickLabel(day(26, 0), day(25))).toBe('翌0時')
    expect(homeTimelineTickLabel(day(26, 3), day(25))).toBe('翌3時')
    expect(homeTimelineTickLabel(day(27, 0), day(25))).toBe('翌々0時')
  })

  it('予定の日は今日 / 明日 / M/D', () => {
    expect(homeTimelineDayLabel(day(25, 23), day(25, 1))).toBe('今日')
    expect(homeTimelineDayLabel(day(26, 0), day(25, 23))).toBe('明日')
    expect(homeTimelineDayLabel(day(28, 5), day(25))).toBe('7/28')
  })
})
