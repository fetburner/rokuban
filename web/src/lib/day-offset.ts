/** 番組タブの日付選択（`DayStrip`）に関する計算。表示とは独立にテストする。 */

/**
 * dayOrigin は日付選択に対応する時間窓の起点を返す。
 *
 * `dayOffset` が null なら「今」（時刻境界に切り捨てる。窓を時刻境界に揃えるため）。
 * 数値ならその日数だけ先の 0 時。日付を選べば「さらに読み込む」を何度も押さずに
 * 先の日付へ跳べる。
 *
 * `now` はテストから現在時刻を固定するための注入口。省略時は `Date.now()`。
 */
export function dayOrigin(dayOffset: number | null, now?: number): Date {
  const origin = new Date(now ?? Date.now())
  if (dayOffset === null) {
    origin.setMinutes(0, 0, 0)
    return origin
  }
  origin.setDate(origin.getDate() + dayOffset)
  origin.setHours(0, 0, 0, 0)
  return origin
}
