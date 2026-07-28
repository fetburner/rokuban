import { cn } from '@/lib/utils'

/**
 * Chip はトグル可能な小さな選択肢。日付・サービス・表示形式の切り替えに使う。
 *
 * `aria-pressed` を持つトグルボタンであってリンクではない（選択は URL に出ない）。
 * 番組表（M2-9）と検索（M2-11）が並行実装で同じものを各ページに持ってしまったため、
 * マージ後にここへ引き上げた。
 */
export function Chip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        'shrink-0 rounded-full border px-3 py-1.5 text-xs transition-colors',
        active
          ? 'border-primary bg-primary text-primary-foreground'
          : 'border-border text-muted-foreground hover:bg-muted',
      )}
    >
      {children}
    </button>
  )
}
