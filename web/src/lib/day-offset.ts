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

/**
 * dayOffsetForMs は epoch ms の時刻が属する暦日を `dayOffset`（`DayStrip` が
 * 選べる範囲）に直す。`dayOrigin` の逆写像（`dayOrigin` は offset → 時刻、
 * これは時刻 → offset）。
 *
 * 容量不足バッジ（`components/capacity-shortfall-badge.tsx`）から番組表へ
 * 飛んだとき、グリッドが出ない画面幅（`lg` 未満）やリスト表示中でも
 * 「その時刻が属する日」だけは合わせるためのフォールバックに使う
 * （issue #233 M6-5）。判定は暦日境界（0 時）の比較で行う ---
 * `dayOffset` が 0 のときの `dayOrigin` は「now を時で切り捨てた時刻」だが、
 * それは時間窓の起点の話であって「今日かどうか」の判定は 0 時境界で揃えないと
 * 同じ日の未明の時刻が前日に誤判定される。
 *
 * 範囲外（過去・`selectableDays` 日より先）は境界にクランプする。番組表は
 * EPG のローリングウィンドウの外を表示できないため、収まる最も近い日に落とす
 * --- 存在しない日を選ばせて画面を壊すより、隣接する実在の日を見せる方がよい。
 */
export function dayOffsetForMs(atMs: number, nowMs: number, selectableDays: number): number {
  const today = new Date(nowMs)
  today.setHours(0, 0, 0, 0)
  const target = new Date(atMs)
  target.setHours(0, 0, 0, 0)
  const diffDays = Math.round((target.getTime() - today.getTime()) / 86_400_000)
  return Math.min(Math.max(diffDays, 0), selectableDays - 1)
}
