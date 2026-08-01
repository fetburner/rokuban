/**
 * 番組リストの先頭に、まだ読み込んでいない前の時間窓からはみ出した番組を
 * 出さないための絞り込み。
 *
 * API は問い合わせた時間窓に**重なる**番組を返す（`start_at < window_end AND
 * end_at > window_start`）。そのため、先頭に読み込んだ窓の開始時刻より前に
 * 始まった番組（＝まだ読み込んでいない前の窓との重なりで一緒に返ってきた
 * 番組）がリストの先頭に混ざる。これを見せたままにすると、日付ヘッダと
 * 「いま見ている日」の導出（`lib/visible-day.ts`。どちらもリストの先頭の
 * 番組から日を決める）が両方とも前日を指してしまう（実機で確認済みの不具合。
 * `docs/frontend.md` 「番組リスト」参照）。
 */

/**
 * filterProgramsFromListStart は、`listStartMs`（読み込み済みの最も手前の
 * 窓の開始時刻。下限で clamp 済み）より前に始まった番組を取り除く。
 *
 * ただし `listStartMs` が `lowerBoundMs`（遡行の下限。now を時で切り捨てた
 * 時刻）と一致するときは絞り込まない。単純に「先頭の窓の開始時刻より前は
 * 切る」だけにすると、いま放送中の番組が消えてしまう ---
 * 「今日」（起点が時刻境界）を 20:15 に見ると、19:30 開始の番組が
 * `startAt < listStartMs` に該当して落ちる。この番組は「前の窓との重なり」
 * ではなく「いま見えているべき番組」なので、下限に達しているときは
 * 例外的に開始が前の番組も出す。
 */
export function filterProgramsFromListStart<T extends { startAt: string }>(
  programs: readonly T[],
  listStartMs: number,
  lowerBoundMs: number,
): T[] {
  if (listStartMs === lowerBoundMs) return [...programs]
  return programs.filter((program) => new Date(program.startAt).getTime() >= listStartMs)
}
