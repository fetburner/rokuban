import { TriangleAlert } from 'lucide-react'

import { interruptionWarningMessage } from '@/lib/live-interruption'

/**
 * LiveInterruptionWarning は「近い将来に同じチャンネル種別で始まる録画予約が
 * ある」ことを知らせる（M7-2, issue #235）。
 *
 * `pages/live.tsx` の選択状態（値札）・視聴中の画面の両方から同じコンポーネントを
 * 呼ぶ --- 「切り替えたら見られなくなった」の予告版であり、視聴を選ぶ／始める
 * どちらの時点でも同じ判断材料を出す必要がある。
 *
 * `reservation` が null（該当する予約が無い）なら何も描画しない --- 「録画予約は
 * ありません = 安全に見られます」という肯定的な文言は出さない（沈黙は保証では
 * ない。`ProgramOverlapWarning` / `CapacityShortfallBadge` と同じ「余計な枠を
 * 出さない」流儀）。
 */
export function LiveInterruptionWarning({
  reservation,
}: {
  reservation: { startAt: string } | null
}) {
  if (reservation === null) return null

  return (
    <p className="flex items-start gap-1 text-xs text-warning">
      <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span>{interruptionWarningMessage(reservation)}</span>
    </p>
  )
}
