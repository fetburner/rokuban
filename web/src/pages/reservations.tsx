import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { useMemo } from 'react'

import { useListCapacityOverages, useListReservations, type Reservation } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { CapacityShortfallBadge } from '@/components/capacity-shortfall-badge'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { ReservationSkipBadge } from '@/components/reservation-skip-reason'
import { coveringWindow } from '@/lib/capacity'
import { formatDateTime, formatDuration } from '@/lib/format'
import { stateLabels } from '@/lib/reservation-labels'
import { cn } from '@/lib/utils'

export function ReservationsPage() {
  const query = useListReservations()
  const reservations = useMemo(() => unwrap(query.data) ?? [], [query.data])

  // 一覧に出ている予約すべてを覆う窓で超過区間を訊く。窓を固定幅にすると、
  // その外に出た予約のバッジが黙って消える。予約が無ければ問い合わせない
  // （窓が null。パラメータは必須なので値は入れるが enabled で止める）。
  const listedWindow = useMemo(() => coveringWindow(reservations), [reservations])
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(listedWindow?.startMs ?? 0).toISOString(),
      end: new Date(listedWindow?.endMs ?? 0).toISOString(),
    },
    { query: { enabled: listedWindow !== null } },
  )
  // 取得の失敗・未完了は「バッジが出ない」に落ちる。元から沈黙は「収まる」ことの
  // 保証ではないので（docs/data.md §6.5）、予約一覧そのものをエラーにはしない
  const overages = useMemo(() => unwrap(overagesQuery.data) ?? [], [overagesQuery.data])

  return (
    <>
      <PageHeader title="予約" />

      {query.isError ? (
        <ErrorState>予約の取得に失敗しました</ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : reservations.length === 0 ? (
        <EmptyState>予約がありません</EmptyState>
      ) : (
        <ul>
          {reservations.map((r) => {
            // 行本体のリンクの accessible name。子要素を持たない絶対配置の
            // リンク（下記）にするため、children から自動で組めない分を明示する。
            // skip / 容量バッジのテキストは含めない --- それらは行の中の通常
            // フロー要素として残り、ブラウズ（矢印キー走査）では読めるので、
            // 1 つの長いリンク名に押し込む必要はない。
            const rowLabel = [
              r.title || '（番組名なし）',
              formatDateTime(r.startAt),
              formatDuration(r.durationMs),
              r.state === 'active' ? null : stateLabels[r.state],
            ]
              .filter((s): s is string => s !== null)
              .join(' ')

            return (
              <li
                key={r.id}
                className="relative flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/50"
              >
                {/* 行本体のリンクは絶対配置で行全体を覆う「面」にし、通常フローから
                    外す（`position: relative` を li に置いて containing block に
                    する）。**入れ子を解く方向を反転させた**（issue #233 のレビュー
                    指摘）--- 最初の実装は容量バッジを行本体リンクの外（兄弟）へ
                    移して <a> の入れ子を消したが、それは配置文法（バッジの位置・
                    chevron の終端性・モバイルでのタイトル幅）を壊した。壊れていた
                    のは「バッジが行の中にある」ことではなく「行本体そのものが
                    子要素を抱えた <a> で、バッジという別の対話要素と競合する」こと
                    --- 行の中身（タイトル・バッジ列・chevron）は元の配置のまま
                    通常フローに残し、行本体のリンクだけを見えない全面カバーの層に
                    退避させる。
                    子要素を持たないため accessible name は aria-label で渡す
                    （children から計算できない）。 */}
                <Link
                  to="/reservations/$site/$programId"
                  params={{ site: r.site, programId: String(r.programId) }}
                  aria-label={rowLabel}
                  className="absolute inset-0"
                />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm">{r.title || '（番組名なし）'}</div>
                  <div
                    data-testid="reservation-secondary"
                    className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground"
                  >
                    <span className="shrink-0">{r.serviceName}</span>
                    <span className="shrink-0">{formatDateTime(r.startAt)}</span>
                    <span className="shrink-0">{formatDuration(r.durationMs)}</span>
                    <StateBadge state={r.state} />
                    <ReservationSkipBadge reservation={r} />
                    {/* 容量バッジは番組表への別の Link（issue #233 M6-5）。
                        `relative`（z-index は指定しない）を足すことで、CSS の
                        重ね順（positioned な要素は non-positioned な要素より
                        常に上、同じ z-index auto なら DOM 順）に乗り、行本体の
                        `absolute inset-0` リンクより手前に来る --- これでクリックが
                        バッジ自身の Link に届く。判定はサイトごとに独立している
                        （docs/data.md §6.5）ので予約自身の site を渡す。定数を
                        持たない。 */}
                    <CapacityShortfallBadge
                      className="relative"
                      overages={overages}
                      site={r.site}
                      startMs={new Date(r.startAt).getTime()}
                      endMs={new Date(r.startAt).getTime() + r.durationMs}
                    />
                  </div>
                </div>
                <ChevronRight
                  data-testid="reservation-chevron"
                  className="size-4 shrink-0 text-muted-foreground"
                />
              </li>
            )
          })}
        </ul>
      )}
    </>
  )
}

function StateBadge({ state }: { state: Reservation['state'] }) {
  if (state === 'active') return null
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-[0.65rem]',
        state === 'orphaned'
          ? 'bg-destructive/10 text-destructive'
          : 'bg-muted text-muted-foreground',
      )}
    >
      {stateLabels[state]}
    </span>
  )
}
