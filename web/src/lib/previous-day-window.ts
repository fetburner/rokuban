/**
 * 「前を読み込む」を押したときに次に取得すべき時間窓（前日 00:00〜当日 00:00）
 * を決める純関数だけを置く。
 *
 * ## なぜ遡行だけ 1 暦日単位にするか
 *
 * 番組リストの日付ヘッダは「直前の番組と暦日が変わったか」で決まる
 * （`components/program-list.tsx` の `showDateHeader`）。`lastDay` の初期値が
 * 空文字列なので、**リストの先頭行には必ず帯が付く**。
 *
 * 遡行で差し込む窓の境界が暦日の境界（0 時）でないと、それまで先頭だった
 * 行（同じ日の続きになる）が帯を失い、その行の高さぶんだけ下の内容が
 * ずれる（実機で「日付の帯のぶん位置がずれる」不具合として確認済み）。
 * 境界を常に暦日にすれば、この帯の増減による位置ずれが構造的に起きない
 * ---
 * `A`（フレーム跳ねの補正を描画前に前倒しする）と `B`（sticky の裏に
 * 隠れる行をアンカーに選ばない）を直しても、帯の増減そのものは別の原因
 * なので両方直す必要がある。
 *
 * 進行方向（下スクロールでの自動読み込み）は、増分読み込みとして単に
 * 「次の 6 時間」を足すだけなので暦日に揃える理由が無く、`pages/programs.tsx`
 * の `windowHours`（6 時間）のまま変えていない。
 */

/** ミリ秒単位の半開区間 [startMs, endMs)。 */
export type TimeWindow = {
  startMs: number
  endMs: number
}

/**
 * previousDayWindow は、現在読み込み済みの最も手前の窓の開始時刻
 * （`earliestLoadedMs`。既に `lowerBoundMs` で clamp 済みの値 ---
 * `pages/programs.tsx` の `listStartMs` を渡す想定）から、次に遡って読むべき
 * 「前日 00:00〜当日 00:00」の窓を返す。
 *
 * `earliestLoadedMs` が `lowerBoundMs`（now を時で切り捨てた時刻。遡行の下限。
 * `pages/programs.tsx` 参照）以下ならこれ以上遡る内容が無いので `null`
 * （「前を読み込む」ボタン自体を出さない判断に使う）。
 *
 * 前日の 0 時が `lowerBoundMs` より前になる場合は `lowerBoundMs` で打ち切る
 * ---
 * それより前は放送済みで今回のスコープ外（`lib/program-list-window.ts` と
 * 同じ前提）。この場合に返る窓は 24 時間に満たないことがある。次に
 * `previousDayWindow` を呼ぶと（`startMs === lowerBoundMs` になっているはずなので）
 * `null` が返り、「前を読み込む」ボタンはそこで消える。
 */
export function previousDayWindow(
  earliestLoadedMs: number,
  lowerBoundMs: number,
): TimeWindow | null {
  if (earliestLoadedMs <= lowerBoundMs) return null
  const dayStart = new Date(earliestLoadedMs)
  dayStart.setDate(dayStart.getDate() - 1)
  dayStart.setHours(0, 0, 0, 0)
  const startMs = Math.max(dayStart.getTime(), lowerBoundMs)
  return { startMs, endMs: earliestLoadedMs }
}
