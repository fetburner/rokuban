import { TriangleAlert } from 'lucide-react'

import { useGetProgramOverlaps, type ProgramOverlaps } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { formatTime } from '@/lib/format'

/**
 * ProgramOverlapWarning は指定番組の放送時間帯と重なる既存予約の件数と内訳を出す。
 *
 * **チューナー本数は見ていない**（issue #21 の「案 C」）。勝敗や容量超過の判定は
 * M2-10（`tuner_sync` 射影 + 容量判定、docs/data.md §6.5）の領分なので、ここでは
 * 常に「事実の提示」にとどめる。「録画できません」「競合しています」のような
 * 断定は書かない — 同一物理チャンネルなら 1 本のチューナーで複数番組を賄えるため、
 * 重なりがあっても録画できないとは限らない。
 *
 * 予約後に知らせても遅いので、予約ボタンの近くに常時（展開操作なしで）表示する。
 * 0 件または重なり情報がまだ無いときは何も描画しない
 * （`CircuitBreakerBanner` と同じ「余計な枠を出さない」流儀）。情報がまだ無い状態を
 * 0 件と断定しないため、番組表の予約一覧が未取得の間も同じ扱いにする。
 */
export function ProgramOverlapWarning({ overlaps }: { overlaps?: ProgramOverlaps }) {
  if (!overlaps || overlaps.count === 0) return null

  return (
    <p className="flex items-start gap-1 text-xs text-warning">
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

/**
 * ProgramOverlapWarningFromApi は単一番組の重なり API を使う入口。
 *
 * 番組表は `GET /api/reservations` の取得済み一覧から `ProgramOverlapWarning` へ
 * データを渡すため、行ごとの API 取得を行わない。一方、予約詳細画面は単一番組を
 * 扱うので、従来どおり `GET /api/sites/{site}/programs/{programId}/overlaps` を使う。
 *
 * `site` は呼び出し側に必須で渡させる。
 * `ReservationDetailPage`（`/reservations/$site/$programId`）は URL の `$site`
 * が対象を決める資源同定であり、画面全体の site と一致するとは限らない --- 一致させると、対象サイト以外の予約詳細を
 * 開いたときに常に別サイトの重なりを問い合わせてしまう（issue #184 M4-12）。
 */
export function ProgramOverlapWarningFromApi({
  site,
  programId,
  enabled = true,
}: {
  site: string
  programId: number
  /** 呼び出し側で問い合わせ自体を止めたい場合に false を渡す（例: 既に予約取消済み）。 */
  enabled?: boolean
}) {
  const query = useGetProgramOverlaps(site, programId, { query: { enabled } })
  return <ProgramOverlapWarning overlaps={unwrap(query.data)} />
}
