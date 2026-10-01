import { ChevronRight } from 'lucide-react'
import type { ReactNode } from 'react'

/** Detail headings share the same level and spacing across recording sections. */
export function DetailHeading({
  children,
  compact = false,
}: {
  children: ReactNode
  compact?: boolean
}) {
  return <h3 className={compact ? 'font-medium' : 'mb-2 font-medium'}>{children}</h3>
}

/**
 * DetailSummary は `<details className="group">` の要約行。Chrome は summary に
 * display:flex を当てると開閉三角（::marker）を消すので、自前の矢印を付けて開けると
 * 分かるようにし、min-h-11 で 44px を確保する。
 */
export function DetailSummary({ children }: { children: ReactNode }) {
  return (
    <summary className="flex min-h-11 cursor-pointer items-center gap-1 font-medium">
      <ChevronRight aria-hidden className="size-4 shrink-0 transition-transform group-open:rotate-90" />
      {children}
    </summary>
  )
}
