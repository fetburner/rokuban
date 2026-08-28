/**
 * 番組リスト（`components/program-list.tsx` と `pages/programs.tsx`）の純関数。
 *
 * 「いま見ている日」の導出・仮想化のキー・遡行の時間窓は、いずれも表示と
 * 独立にテストできる形にしてある —— jsdom はレイアウトもスクロール位置も
 * 測れないので、ここで固定できる部分は全部ここに寄せる（`web/e2e/README.md`）。
 * 4 つの関心が 1 ファイルなのは、どれも「リストの先頭がどの番組か」から
 * 派生していて、片方だけ直すと壊れる関係にあるため（下記 `previousDayWindow` と
 * `filterProgramsFromListStart` のコメント参照）。
 */

import type { ProgramListItem } from '@/api/generated'

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
 * 先頭への挿入で添字がずれても、行の実体（programId）に結びついた計測値
 * （TanStack Virtual の `itemSizeCache`）がそのまま引き継がれる。
 *
 * `getItemKey` の既定は `(index) => index` で、遡行（前の時間窓の読み込み）で
 * リスト先頭に行を差し込むと既存の全行の添字が N ずれ、記録済みの実測値が
 * 別の番組のものとして使われてしまう（遡行のスクロール位置が飛ぶ不具合の原因
 * だった）。
 *
 * コンポーネントファイルではなくここに置くのは、コンポーネントファイルが
 * 純関数の値エクスポートを持つと Fast Refresh の対象外になる警告
 * （oxlint の `react(only-export-components)`）が出るため。テストから直接
 * 呼べるようにする狙いも同じ理由から自然にここに来る。
 */
export function programKeyAt(programs: readonly ProgramListItem[], index: number): number {
  return programs[index].programId
}

/**
 * findProgramIndex は、指定した `programId` の現在の添字を返す。
 *
 * 遡行のアンカー位置合わせ（`components/program-list.tsx`）が使う ---
 * 「前を読み込む」を押す前に控えた programId から、先頭への挿入で行われた
 * 番組配列に対する現在の添字を引き直し、`virtualizer.scrollToIndex` に渡す。
 * 見つからなければ `null`（呼び出し側は何もしない）。
 */
export function findProgramIndex(
  programs: readonly ProgramListItem[],
  programId: number,
): number | null {
  const index = programs.findIndex((program) => program.programId === programId)
  return index === -1 ? null : index
}

// --- 遡行の時間窓 ----------------------------------------------------------

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
 * それより前は放送済みで今回のスコープ外（下記 `filterProgramsFromListStart` と
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

// --- 先頭のはみ出し番組の除去 ----------------------------------------------

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
