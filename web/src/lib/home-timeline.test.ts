import { describe, expect, it } from 'vitest'

import { buildHomeTimelineRows, packHomeTimelineEvents } from '@/lib/home-timeline'

const event = (
  key: string,
  startMs: number,
  endMs: number,
  site = 'default',
  channelType = 'GR',
) => ({ key, site, channelType, startMs, endMs, kind: 'reservation' as const, title: key })

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
  it('jammedTypes の全行へ同じsiteの区間を載せ、予約アイテム自体には印を付けない', () => {
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
    expect(rows.flatMap((row) => row.events).every((item) => !('overage' in item))).toBe(true)
  })

  it('データが無いサイト/種別の行を捏造しない', () => {
    expect(buildHomeTimelineRows([], [])).toEqual([])
  })
})
