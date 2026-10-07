import { Link } from '@tanstack/react-router'

import { toSeriesSearch, type RecordingsPageSearch } from '@/lib/recording-search'
import { cn } from '@/lib/utils'

/**
 * 録画一覧とシリーズ一覧の間を移動する 2 択。ごみ箱は録画側の内側に残す。
 *
 * 絞り込み条件は両方向で引き継ぐ（同じ条件を録画単位とシリーズ単位で見比べる）。
 * シリーズ側へは棚が受けない次元（ごみ箱・並び順・エンコード状況）を落として渡す。
 * シリーズ側の条件はもともと録画一覧の部分集合なので、録画側へはそのまま渡す。
 * 今いる側のリンクも同じ条件を指す（押しても条件が消えない）。
 */
export function RecordingSeriesToggle({
  active,
  search,
}: {
  active: 'recordings' | 'series'
  search: RecordingsPageSearch
}) {
  return (
    <div
      role="group"
      aria-label="録画とシリーズの表示切替"
      className="flex items-center pointer-coarse:gap-0.5 rounded-md border border-border p-0.5"
    >
      <Link
        to="/recordings"
        search={search}
        aria-current={active === 'recordings' ? 'page' : undefined}
        className={cn(
          'inline-flex min-h-6 min-w-6 pointer-coarse:min-h-11 pointer-coarse:min-w-11 items-center rounded px-2 py-1 text-xs transition-colors',
          active === 'recordings'
            ? 'bg-muted font-medium text-foreground'
            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
        )}
      >
        録画
      </Link>
      <Link
        to="/series"
        search={toSeriesSearch(search)}
        aria-current={active === 'series' ? 'page' : undefined}
        className={cn(
          'inline-flex min-h-6 min-w-6 pointer-coarse:min-h-11 pointer-coarse:min-w-11 items-center rounded px-2 py-1 text-xs transition-colors',
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
