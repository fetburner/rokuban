/**
 * visibleDayOffset に関する計算。「いま見ている日」の導出を表示・仮想化とは
 * 独立にテストできるよう、純関数として切り出す（`lib/day-offset.ts` /
 * `lib/list-virtualization.ts` と同じ方針）。
 */

/**
 * visibleDayOffset は、可視範囲の先頭にある番組の開始日から「いま見ている日」の
 * dayOffset を返す。
 *
 * 比較は暦日（ローカルタイムの年月日）で行う。日付ヘッダのグルーピング
 * （`lib/format.ts` の `dayKey`）と同じ基準にするため、時刻境界（`dayOrigin(0)`
 * が「今」から始まること）とは揃えない —— 今日のセルが 0 時始まりでなくても、
 * 今日の番組はすべて offset 0 として扱われるべきだからである。
 *
 * `firstVisibleIndex` が範囲外（空リストや負のインデックス）なら 0 を返す。
 * 過去日（診断上あり得ないはずだが、時計のずれ等で `now` より前の番組が
 * 先頭に来た場合）も 0 に丸める —— 今回のスコープに「今より前」の日は無いため。
 *
 * `now` はテストから現在時刻を固定するための注入口。
 */
export function visibleDayOffset(
  programs: readonly { startAt: string }[],
  firstVisibleIndex: number,
  now: number,
): number {
  const program = programs[firstVisibleIndex]
  if (!program) return 0

  const startOfToday = new Date(now)
  startOfToday.setHours(0, 0, 0, 0)

  const startOfProgramDay = new Date(program.startAt)
  startOfProgramDay.setHours(0, 0, 0, 0)

  const diffDays = Math.round(
    (startOfProgramDay.getTime() - startOfToday.getTime()) / 86_400_000,
  )
  return Math.max(0, diffDays)
}

/**
 * firstIndexForDayOffset は `visibleDayOffset` と対になる向きの関数。
 * `dayOffset`（暦日、`DayStrip` が渡すジャンプ先）に一致する暦日を持つ、
 * 先頭から見て最初の番組の添字を返す。
 *
 * 日付ストリップで**既にジャンプ先になっている日**をもう一度タップしたときに使う
 * ---
 * `dayOffset`（state）が変わらないので `setDayOffset` は再レンダーを起こさず、
 * クエリもスクロールも動かない。しかしユーザーはスクロールでその日から離れた
 * 場所を見ていることがあるので、タップは「読み込み済みなら、その日の先頭へ
 * scrollToIndex する」を意味する必要がある（`components/program-list.tsx` の
 * `ProgramListHandle.scrollToDayOffset`）。
 *
 * 比較は `visibleDayOffset` と同じ基準（暦日、ローカルタイム）。該当する番組が
 * 1 件も無ければ（まだ読み込んでいない日、など）`null` を返す ---
 * 呼び出し側はその場合 `scrollToIndex` を試みない。
 */
export function firstIndexForDayOffset(
  programs: readonly { startAt: string }[],
  dayOffset: number,
  now: number,
): number | null {
  const startOfToday = new Date(now)
  startOfToday.setHours(0, 0, 0, 0)
  const targetDayStartMs = startOfToday.getTime() + dayOffset * 86_400_000

  for (let index = 0; index < programs.length; index++) {
    const startOfProgramDay = new Date(programs[index].startAt)
    startOfProgramDay.setHours(0, 0, 0, 0)
    if (startOfProgramDay.getTime() === targetDayStartMs) return index
  }
  return null
}
