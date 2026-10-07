import { Link } from '@tanstack/react-router'

import { saveHomeModePreference, type HomeMode } from '@/lib/home-mode'
import { cn } from '@/lib/utils'

/** ホームの「見る / 管理」切替。形と aria-current は RecordingSeriesToggle に揃える。 */
export function HomeModeToggle({
  mode,
  warningCount,
}: {
  mode: HomeMode
  /** 警告クエリがすべて解決するまで undefined。0 件ならバッジを描かない。 */
  warningCount: number | undefined
}) {
  return (
    <nav
      aria-label="ホームの表示切替"
      data-testid="home-mode-toggle"
      className="flex shrink-0 items-center gap-3 border-b border-border"
    >
      <Link
        to="/"
        search={{ mode: 'watch' }}
        aria-current={mode === 'watch' ? 'page' : undefined}
        onClick={() => saveHomeModePreference('watch')}
        className={cn(
          'flex min-h-7 pointer-coarse:min-h-11 pointer-coarse:min-w-11 -mb-px items-center gap-1 border-b-2 border-transparent px-1 py-1 text-xs whitespace-nowrap transition-colors',
          mode === 'watch'
            ? 'border-foreground font-medium text-foreground'
            : 'text-muted-foreground hover:text-foreground',
        )}
      >
        見る
      </Link>
      <Link
        to="/"
        search={{ mode: 'ops' }}
        aria-current={mode === 'ops' ? 'page' : undefined}
        onClick={() => saveHomeModePreference('ops')}
        className={cn(
          'flex min-h-7 pointer-coarse:min-h-11 pointer-coarse:min-w-11 -mb-px items-center gap-1 border-b-2 border-transparent px-1 py-1 text-xs whitespace-nowrap transition-colors',
          mode === 'ops'
            ? 'border-foreground font-medium text-foreground'
            : 'text-muted-foreground hover:text-foreground',
        )}
      >
        管理
        {warningCount !== undefined && warningCount > 0 && (
          <span
            data-testid="home-warning-count"
            aria-label={`警告 ${warningCount} 件`}
            className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-sm bg-destructive/10 px-1 text-xs leading-4 text-destructive whitespace-nowrap"
          >
            {warningCount}
          </span>
        )}
      </Link>
    </nav>
  )
}
