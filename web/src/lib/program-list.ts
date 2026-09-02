/**
 * 番組リスト（`components/program-list.tsx` と `pages/programs.tsx`）の純関数。
 *
 * 「いま見ている日」の導出・仮想化のキー・先頭のはみ出し番組の除去は、いずれも
 * 表示と独立にテストできる形にしてある —— jsdom はレイアウトもスクロール位置も
 * 測れないので、ここで固定できる部分は全部ここに寄せる（`web/e2e/README.md`）。
 */

import { programIdentity, type SiteProgram } from '@/lib/all-sites-services'

// --- いま見ている日 --------------------------------------------------------

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

// --- 仮想化のキーと添字 ----------------------------------------------------

/**
 * programKeyAt は仮想化（`components/program-list.tsx` の `useWindowVirtualizer`）の
 * `getItemKey` に渡す関数の中身。`programs[index]` の `programId` をキーにすることで、
 * 先頭の内容がずれても、行の実体（programId）に結びついた計測値
 * （TanStack Virtual の `itemSizeCache`）がそのまま引き継がれる。
 *
 * `getItemKey` の既定は `(index) => index` で、絞り込みの変更等でリストの
 * 先頭の番組が変わると既存の行の添字がずれ、記録済みの実測値が別の番組の
 * ものとして使われてしまう。
 *
 * コンポーネントファイルではなくここに置くのは、コンポーネントファイルが
 * 純関数の値エクスポートを持つと Fast Refresh の対象外になる警告
 * （oxlint の `react(only-export-components)`）が出るため。テストから直接
 * 呼べるようにする狙いも同じ理由から自然にここに来る。
 */
export function programKeyAt(programs: readonly SiteProgram[], index: number): string {
  return programIdentity(programs[index].site, programs[index].programId)
}

// --- 先頭のはみ出し番組の除去 ----------------------------------------------

/**
 * filterProgramsFromListStart は、`listStartMs`（読み込み済みの先頭の窓の
 * 開始時刻。下限で clamp 済み）より前に始まった番組を取り除く。
 *
 * API は問い合わせた時間窓に**重なる**番組を返す（`start_at < window_end AND
 * end_at > window_start`）ため、`listStartMs` より前に始まった番組（窓の外との
 * 重なりで一緒に返ってきた番組）がリストの先頭に混ざることがある。
 *
 * ただし `listStartMs` が `lowerBoundMs`（now を時で切り捨てた時刻）と一致する
 * とき（＝今日を見ているとき）は絞り込まない。単純に「先頭の窓の開始時刻より
 * 前は切る」だけにすると、いま放送中の番組が消えてしまう ---
 * 「今日」（起点が時刻境界）を 20:15 に見ると、19:30 開始の番組が
 * `startAt < listStartMs` に該当して落ちる。この番組は「窓の外との重なり」
 * ではなく「いま見えているべき番組」なので、今日を見ているときは例外的に
 * 開始が前の番組も出す。
 */
export function filterProgramsFromListStart<T extends { startAt: string }>(
  programs: readonly T[],
  listStartMs: number,
  lowerBoundMs: number,
): T[] {
  if (listStartMs === lowerBoundMs) return [...programs]
  return programs.filter((program) => new Date(program.startAt).getTime() >= listStartMs)
}
