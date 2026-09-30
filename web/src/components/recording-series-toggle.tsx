import { Link } from '@tanstack/react-router'

import { cn } from '@/lib/utils'

/** 録画一覧とシリーズ一覧の間を移動する 2 択。ごみ箱は録画側の内側に残す。 */
export function RecordingSeriesToggle({ active }: { active: 'recordings' | 'series' }) {
  return (
    <div
      role="group"
      aria-label="録画とシリーズの表示切替"
      className="flex items-center rounded-md border border-border p-0.5"
    >
      <Link
        to="/recordings"
        aria-current={active === 'recordings' ? 'page' : undefined}
        className={cn(
          'rounded px-2 py-1 text-xs transition-colors',
          active === 'recordings'
            ? 'bg-muted font-medium text-foreground'
            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
        )}
      >
        録画
      </Link>
      <Link
        to="/series"
        aria-current={active === 'series' ? 'page' : undefined}
        className={cn(
          'rounded px-2 py-1 text-xs transition-colors',
          active === 'series'
            ? 'bg-muted font-medium text-foreground'
            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
        )}
      >
        シリーズ
      </Link>
    </div>
  )
}
