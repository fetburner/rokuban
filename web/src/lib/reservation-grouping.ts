/** 予約一覧の並べ方。表示情報は同じで並びだけが変わるため端末に保存する。 */
export type ReservationGrouping = 'series' | 'time'

export const RESERVATION_GROUPING_KEY = 'rokuban:reservations:group'

/** 保存値が無い・不正・読めないときはシリーズ表示にする。 */
export function loadReservationGrouping(): ReservationGrouping {
  try {
    return localStorage.getItem(RESERVATION_GROUPING_KEY) === 'time' ? 'time' : 'series'
  } catch {
    return 'series'
  }
}

/** 保存に失敗しても現在の画面の切替は成立させる。 */
export function saveReservationGrouping(grouping: ReservationGrouping): void {
  try {
    localStorage.setItem(RESERVATION_GROUPING_KEY, grouping)
  } catch {
    // private mode などでは次回起動時にシリーズ表示へ戻る。
  }
}
