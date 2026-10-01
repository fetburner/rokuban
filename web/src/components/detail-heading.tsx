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
