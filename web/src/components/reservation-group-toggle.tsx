import { cn } from '@/lib/utils'
import type { ReservationGrouping } from '@/lib/reservation-grouping'

/** 予約一覧のシリーズ / 時間順切替。同一ルート内で表示だけを変える。 */
export function ReservationGroupToggle({
  grouping,
  onChange,
}: {
  grouping: ReservationGrouping
  onChange: (grouping: ReservationGrouping) => void
}) {
  return (
    <div
      role="group"
      aria-label="予約のまとめ方"
      className="flex shrink-0 items-center rounded-md border border-border p-0.5"
    >
      {(['series', 'time'] as const).map((value) => {
        const active = grouping === value
        const label = value === 'series' ? 'シリーズ' : '時間順'
        return (
          <button
            key={value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(value)}
            className={cn(
              'flex min-h-7 items-center rounded px-2 py-1 text-xs whitespace-nowrap transition-colors',
              active
                ? 'bg-muted font-medium text-foreground'
                : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
            )}
          >
            {label}
          </button>
        )
      })}
    </div>
  )
}
