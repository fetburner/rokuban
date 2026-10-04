import { afterEach, describe, expect, it } from 'vitest'

import {
  loadReservationGrouping,
  RESERVATION_GROUPING_KEY,
  saveReservationGrouping,
} from '@/lib/reservation-grouping'

afterEach(() => {
  localStorage.clear()
})

describe('予約一覧の表示設定', () => {
  it('保存が無ければシリーズ表示を既定にする', () => {
    expect(localStorage.getItem(RESERVATION_GROUPING_KEY)).toBeNull()
    expect(loadReservationGrouping()).toBe('series')
  })

  it('時間順を端末の localStorage に保存して読み戻す', () => {
    saveReservationGrouping('time')

    expect(localStorage.getItem(RESERVATION_GROUPING_KEY)).toBe('time')
    expect(loadReservationGrouping()).toBe('time')
  })

  it('不明な値はシリーズ表示へ戻す', () => {
    localStorage.setItem(RESERVATION_GROUPING_KEY, 'rule')

    expect(loadReservationGrouping()).toBe('series')
  })
})
