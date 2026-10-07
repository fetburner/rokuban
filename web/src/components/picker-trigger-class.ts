import { cn } from '@/lib/utils'

/**
 * pickerTriggerClassName は絞り込みピッカー（チャンネル・ジャンル）のデスクトップ用トリガーの見た目。
 * 両者は同じ理由（絞り込みパネルの統一）で変わるのでここに 1 つだけ置く。
 */
export const pickerTriggerClassName = cn(
  'flex h-11 max-w-full items-center gap-1.5 rounded-lg border border-border bg-background px-3 text-sm text-foreground transition-colors',
  'hover:bg-muted aria-expanded:bg-muted aria-expanded:text-foreground',
)
