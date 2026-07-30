import { TriangleAlert } from 'lucide-react'

import { useGetProgramOverlaps } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { formatTime } from '@/lib/format'
import { DEFAULT_SITE } from '@/lib/site'

/**
 * ProgramOverlapWarning は指定番組の放送時間帯と重なる既存予約の件数と内訳を出す
 * （`GET /api/sites/{site}/programs/{programId}/overlaps`、issue #24 M2-8）。
 *
 * **チューナー本数は見ていない**（issue #21 の「案 C」）。勝敗や容量超過の判定は
 * M2-10（`tuner_sync` 射影 + 容量判定、docs/data.md §6.5）の領分なので、ここでは
 * 常に「事実の提示」にとどめる。「録画できません」「競合しています」のような
 * 断定は書かない — 同一物理チャンネルなら 1 本のチューナーで複数番組を賄えるため、
 * 重なりがあっても録画できないとは限らない。
 *
 * 予約後に知らせても遅いので、予約ボタンの近くに常時（展開操作なしで）表示する
 * 想定（`ProgramRow` / `ReservationDetailPage` から呼ぶ）。0 件のときは何も
 * 描画しない（`CircuitBreakerBanner` と同じ「余計な枠を出さない」流儀）。
 */
export function ProgramOverlapWarning({
  programId,
  enabled = true,
}: {
  programId: number
  /** 呼び出し側で問い合わせ自体を止めたい場合に false を渡す（例: 既に予約取消済み）。 */
  enabled?: boolean
}) {
  const query = useGetProgramOverlaps(DEFAULT_SITE, programId, { query: { enabled } })
  const overlaps = unwrap(query.data)

  if (!overlaps || overlaps.count === 0) return null

  return (
    <p className="flex items-start gap-1 text-xs text-amber-700 dark:text-amber-500">
      <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span>
        同じ時間帯に{overlaps.count}件の予約があります（
        {overlaps.reservations
          .map((r) => `${formatTime(r.startAt)} ${r.title || '（番組名なし）'}`)
          .join('・')}
        ）
      </span>
    </p>
  )
}
