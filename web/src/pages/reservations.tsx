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
import { cn } from '@/lib/utils'

/** stateLabels は reservations.state の表示名（docs/schema.md §3）。 */
const stateLabels: Record<Reservation['state'], string> = {
  active: '有効',
  detached: 'ルール外',
  orphaned: 'EPG から消失',
}

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
          {reservations.map((r) => (
            <li key={r.id} className="flex min-h-14 items-center border-b border-border">
              <Link
                to="/reservations/$site/$programId"
                params={{ site: r.site, programId: String(r.programId) }}
                className="flex min-w-0 flex-1 items-center gap-3 px-4 py-2.5 hover:bg-muted/50"
              >
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm">{r.title || '（番組名なし）'}</div>
                  <div className="flex items-center gap-2 text-xs text-muted-foreground">
                    <span className="shrink-0">{formatDateTime(r.startAt)}</span>
                    <span className="shrink-0">{formatDuration(r.durationMs)}</span>
                    <StateBadge state={r.state} />
                    <ReservationSkipBadge reservation={r} />
                  </div>
                </div>
                <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
              </Link>
              {/* 容量バッジは番組表への別の Link（issue #233 M6-5）。行本体の Link の
                  外（兄弟要素）に置く --- <a> の中に <a> は不正で、クリックの宛先が
                  不定になる（`components/capacity-shortfall-badge.tsx` の doc コメント
                  参照）。
                  判定はサイトごとに独立している（docs/data.md §6.5）ので予約自身の
                  site を渡す。定数を持たない。 */}
              <CapacityShortfallBadge
                className="mr-4 shrink-0"
                overages={overages}
                site={r.site}
                startMs={new Date(r.startAt).getTime()}
                endMs={new Date(r.startAt).getTime() + r.durationMs}
              />
            </li>
          ))}
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
