import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'

import {
  courOf,
  courRange,
  dateInputToFrom,
  dateInputToTo,
  fromToDateInput,
  periodLabel,
  periodPresets,
  toToDateInput,
} from '@/lib/recording-period'

// 区切りは日本時間で明示して計算する（ブラウザのタイムゾーンに頼らない）。vite.config.ts は
// テスト全体を Asia/Tokyo に固定しているので、ローカル時刻で計算する実装でも通ってしまう。
// ここだけ JST 以外で回し、ローカル時刻に頼る実装が落ちるようにする。
describe.each(['UTC', 'America/Los_Angeles'])('TZ=%s', (tz) => {
  beforeAll(() => {
    vi.stubEnv('TZ', tz)
  })
  afterAll(() => {
    vi.unstubAllEnvs()
  })

  it('タイムゾーンが実際に切り替わっている（以下の判定が空虚にならない）', () => {
    expect(new Date('2026-07-01T00:00:00Z').getTimezoneOffset()).toBe(tz === 'UTC' ? 0 : 420)
  })

  it('4 期のクールを日本時間の 0:00 で区切る', () => {
    expect(courRange({ year: 2026, season: 0 })).toEqual({
      // 冬は UTC では前年の 12/31 から始まる
      from: '2025-12-31T15:00:00.000Z',
      to: '2026-03-31T15:00:00.000Z',
    })
    expect(courRange({ year: 2026, season: 1 })).toEqual({
      from: '2026-03-31T15:00:00.000Z',
      to: '2026-06-30T15:00:00.000Z',
    })
    expect(courRange({ year: 2026, season: 2 })).toEqual({
      from: '2026-06-30T15:00:00.000Z',
      to: '2026-09-30T15:00:00.000Z',
    })
    // 秋は年をまたいで翌年 1/1 0:00 JST まで
    expect(courRange({ year: 2026, season: 3 })).toEqual({
      from: '2026-09-30T15:00:00.000Z',
      to: '2026-12-31T15:00:00.000Z',
    })
  })

  it('日付欄の終了日はその日を含み、to から戻すと同じ日になる', () => {
    expect(dateInputToTo('2026-06-30')).toBe('2026-06-30T15:00:00.000Z')
    expect(toToDateInput('2026-06-30T15:00:00.000Z')).toBe('2026-06-30')
    expect(dateInputToFrom('2026-06-30')).toBe('2026-06-29T15:00:00.000Z')
    expect(fromToDateInput('2026-06-29T15:00:00.000Z')).toBe('2026-06-30')
    // 月末・年末の繰り上がり
    expect(dateInputToTo('2026-12-31')).toBe('2026-12-31T15:00:00.000Z')
    expect(toToDateInput('2026-12-31T15:00:00.000Z')).toBe('2026-12-31')
    expect(dateInputToFrom('')).toBeUndefined()
    expect(toToDateInput(undefined)).toBe('')
  })

  it('年を打っている途中の 2 桁以下の年を 1900 年代に読み替えない', () => {
    expect(fromToDateInput(dateInputToFrom('0002-06-30'))).toBe('0002-06-30')
  })

  it('期間の表示: 1 クールちょうど / 上段の選択肢 / それ以外の日付範囲', () => {
    const now = new Date('2026-10-07T03:00:00Z') // 2026-10-07（水）12:00 JST
    expect(periodLabel({}, now)).toBeUndefined()
    expect(periodLabel(courRange({ year: 2026, season: 2 }), now)).toBe('2026 夏')
    // 表記の揺れ（.000 の有無）があっても同じ時刻なら 1 クールとみなす
    expect(periodLabel({ from: '2026-06-30T15:00:00Z', to: '2026-09-30T15:00:00Z' }, now)).toBe('2026 夏')
    expect(periodLabel(courRange({ year: 2026, season: 3 }), now)).toBe('2026 秋')
    // 今週は月曜（10/5）0:00 JST から、終わりは開いたまま
    expect(periodLabel({ from: '2026-10-04T15:00:00.000Z' }, now)).toBe('今週')
    expect(periodLabel({ from: '2026-09-30T15:00:00.000Z' }, now)).toBe('今月')
    expect(periodLabel({ from: '2026-08-09T15:00:00.000Z', to: '2026-08-16T15:00:00.000Z' }, now)).toBe(
      '8/10〜8/16',
    )
    // 1 クールからずれた範囲はクールの名前にしない
    expect(periodLabel({ from: '2026-06-30T15:00:00.000Z', to: '2026-10-01T15:00:00.000Z' }, now)).toBe(
      '7/1〜10/1',
    )
    expect(periodLabel({ from: '2026-08-09T15:00:00.000Z' }, now)).toBe('8/10〜')
    expect(periodLabel({ to: '2026-08-16T15:00:00.000Z' }, now)).toBe('〜8/16')
    // 今年でない端には年を添える
    expect(periodLabel({ from: '2025-08-09T15:00:00.000Z', to: '2025-08-16T15:00:00.000Z' }, now)).toBe(
      '2025/8/10〜2025/8/16',
    )
    // 0:00 JST でない端（古い共有 URL）は時刻を出す
    expect(periodLabel({ from: '2026-08-10T00:30:00.000Z', to: '2026-08-16T03:00:00.000Z' }, now)).toBe(
      '8/10 9:30〜8/16 12:00 前',
    )
  })

  it('上段の選択肢: 今週・今月は to なし、前クールは年をまたぐ', () => {
    // 2026-01-04（日）23:30 JST。週の始まりは月曜なので今週は 12/29 から
    const now = new Date('2026-01-04T14:30:00Z')
    const presets = Object.fromEntries(periodPresets(now).map((p) => [p.label, p]))
    expect(presets['すべての期間'].range).toEqual({})
    expect(presets['今週'].range).toEqual({ from: '2025-12-28T15:00:00.000Z' })
    expect(presets['今月'].range).toEqual({ from: '2025-12-31T15:00:00.000Z' })
    expect(presets['今クール'].detail).toBe('2026 冬')
    expect(presets['前クール'].detail).toBe('2025 秋')
    expect(presets['前クール'].range).toEqual(courRange({ year: 2025, season: 3 }))
  })

  it('courOf は 1 クールちょうどの範囲だけを返す', () => {
    expect(courOf(courRange({ year: 2026, season: 0 }))).toEqual({ year: 2026, season: 0 })
    expect(courOf({ from: '2025-12-31T15:00:00.000Z' })).toBeUndefined()
  })
})
