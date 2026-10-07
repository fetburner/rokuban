// PoC(H-5) 比較用モック。製品コードではない。
import { ContextMenu } from '@base-ui/react/context-menu'
import type { ComponentProps, ReactElement, ReactNode } from 'react'

import { cn } from '@/lib/utils'

export type MockItem = { label: string; destructive?: boolean; sep?: boolean; onSelect?: () => void }

/** mockFlags は e2e/h5-shots.mjs が addInitScript で差す切替フラグ。 */
export function mockFlags(): { variant?: string; menu?: boolean } {
  return (window as unknown as { __mock?: { variant?: string; menu?: boolean } }).__mock ?? {}
}

export function MockCtx({
  enabled,
  items,
  children,
  render,
  ...props
}: { enabled: boolean; items: MockItem[]; children: ReactNode; render?: ReactElement } & ComponentProps<'div'>) {
  if (!enabled || mockFlags().menu === false) {
    const Tag = render ? 'li' : 'div'
    return <Tag {...(props as object)}>{children}</Tag>
  }
  return (
    <ContextMenu.Root>
      <ContextMenu.Trigger render={render} {...(props as object)}>
        {children}
      </ContextMenu.Trigger>
      <ContextMenu.Portal>
        <ContextMenu.Positioner className="isolate z-50 outline-none">
          <ContextMenu.Popup className="z-50 min-w-48 rounded-lg bg-popover p-1 text-popover-foreground shadow-md ring-1 ring-foreground/10 outline-none">
            {items.map((item, i) => (
              <div key={item.label}>
                {item.sep && i > 0 && <ContextMenu.Separator className="-mx-1 my-1 h-px bg-border" />}
                <ContextMenu.Item
                  onClick={item.onSelect}
                  className={cn(
                    'flex cursor-default items-center gap-1.5 rounded-md px-1.5 py-1 text-sm outline-hidden select-none focus:bg-accent focus:text-accent-foreground',
                    item.destructive && 'text-destructive focus:bg-destructive/10 focus:text-destructive',
                  )}
                >
                  {item.label}
                </ContextMenu.Item>
              </div>
            ))}
          </ContextMenu.Popup>
        </ContextMenu.Positioner>
      </ContextMenu.Portal>
    </ContextMenu.Root>
  )
}
