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
  // PoC: N は tabs（録画一覧のライブラリ/ごみ箱と同じ見た目。外枠なし）
  const uni = (window as unknown as { __mock?: { uni?: boolean } }).__mock?.uni === true
  const tab = uni && 'rounded-md border border-transparent px-3 py-1.5'
  return (
    <div
      role="group"
      aria-label="ホームの表示切替"
      data-testid="home-mode-toggle"
      className={cn(
        'flex shrink-0 items-center',
        uni ? 'gap-1' : 'pointer-coarse:gap-0.5 rounded-md border border-border bg-card p-0.5',
      )}
    >
      <Link
        to="/"
        search={{ mode: 'watch' }}
        aria-current={mode === 'watch' ? 'page' : undefined}
        onClick={() => saveHomeModePreference('watch')}
        className={cn(
          'flex min-h-7 pointer-coarse:min-h-11 pointer-coarse:min-w-11 items-center gap-1 rounded px-2 py-1 text-xs whitespace-nowrap transition-colors',
          tab,
          mode === 'watch'
            ? 'bg-muted font-medium text-foreground'
            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
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
          'flex min-h-7 pointer-coarse:min-h-11 pointer-coarse:min-w-11 items-center gap-1 rounded px-2 py-1 text-xs whitespace-nowrap transition-colors',
          tab,
          mode === 'ops'
            ? 'bg-muted font-medium text-foreground'
            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
        )}
      >
        管理
        {warningCount !== undefined && warningCount > 0 && (
          <span
            data-testid="home-warning-count"
            aria-label={`警告 ${warningCount} 件`}
            className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-sm bg-destructive/10 px-1 text-[10px] leading-4 text-destructive whitespace-nowrap"
          >
            {warningCount}
          </span>
        )}
      </Link>
    </div>
  )
}
