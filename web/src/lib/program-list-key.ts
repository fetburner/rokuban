import type { ProgramListItem } from '@/api/generated'

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
