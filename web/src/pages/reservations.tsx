import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'

import { useListReservations, type Reservation } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
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
  const reservations = unwrap(query.data) ?? []

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
            <li key={r.id}>
              <Link
                to="/reservations/$reservationId"
                params={{ reservationId: String(r.id) }}
                className="flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/50"
              >
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm">{r.title || '（番組名なし）'}</div>
                  <div className="flex items-center gap-2 text-xs text-muted-foreground">
                    <span className="shrink-0">{formatDateTime(r.startAt)}</span>
                    <span className="shrink-0">{formatDuration(r.durationMs)}</span>
                    <StateBadge state={r.state} />
                  </div>
                </div>
                <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
              </Link>
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
