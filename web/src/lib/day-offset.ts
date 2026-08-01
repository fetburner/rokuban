/** 番組タブの日付選択（`DayStrip`）に関する計算。表示とは独立にテストする。 */

/**
 * dayOrigin は日付選択（ジャンプ先）に対応する時間窓の起点を返す。
 *
 * `dayOffset` が 0 なら「今日」で、起点は 0 時ではなく now を時刻境界に
 * 切り捨てた時刻（窓を時刻境界に揃えるため）。これによりリストは常に
 * 「今」から連続フィードとして始まり、`今` という別枠の選択肢が要らない
 * （今日のセルがその役目を引き受ける）。
 *
 * `dayOffset` が 0 より大きいならその日数だけ先の 0 時。日付を選べば
 * 「さらに読み込む」を何度も押さずに先の日付へ跳べる。
 *
 * `now` はテストから現在時刻を固定するための注入口。省略時は `Date.now()`。
 */
export function dayOrigin(dayOffset: number, now?: number): Date {
  const origin = new Date(now ?? Date.now())
  if (dayOffset === 0) {
    origin.setMinutes(0, 0, 0)
    return origin
  }
  origin.setDate(origin.getDate() + dayOffset)
  origin.setHours(0, 0, 0, 0)
  return origin
}
