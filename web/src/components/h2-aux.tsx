import { Link } from '@tanstack/react-router'
import { ChevronRight, Plus } from 'lucide-react'
import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'

type Props = {
  kind: 'nav' | 'action'
  children: ReactNode
  onClick?: () => void
  // oxlint-disable-next-line no-explicit-any
  linkProps?: any
}

function variantName(): string {
  return (typeof window !== 'undefined' && (window as unknown as { __H2?: string }).__H2) || 'current'
}

/** H-2 モック: 補助ボタン。 */
export function AuxButton({ kind, children, onClick, linkProps }: Props) {
  const v = variantName()
  const render = linkProps ? <Link {...linkProps} /> : undefined
  if (v === 'A-outline' || v === 'D-tinted') {
    return (
      <Button variant={v === 'A-outline' ? 'outline' : 'secondary'} size="sm" render={render} onClick={onClick}>
        {children}
      </Button>
    )
  }
  if (v === 'B-chevron') {
    return (
      <Button variant="ghost" size="sm" className="text-muted-foreground" render={render} onClick={onClick}>
        {kind === 'action' && <Plus />}
        {children}
        {kind === 'nav' && <ChevronRight />}
      </Button>
    )
  }
  if (v === 'C-underline') {
    return (
      <Button variant="link" size="sm" className="text-foreground underline underline-offset-4" render={render} onClick={onClick}>
        {children}
      </Button>
    )
  }
  return (
    <Button variant="ghost" size="sm" render={render} onClick={onClick}>
      {children}
    </Button>
  )
}
