import { Dialog as DialogPrimitive } from '@base-ui/react/dialog'
import { createContext, type ReactNode } from 'react'

import { DialogOverlay } from '@/components/ui/dialog'

/** B 案用: ToolbarPanel と ChannelPicker の間で「押し込み表示」を共有する。 */
export const PanelCtx = createContext<{
  slot: HTMLElement | null
  pushed: boolean
  setPushed: (v: boolean) => void
} | null>(null)

/** MockSheet は ToolbarPanel のシートと同じ見た目の下シート（モック用）。 */
export function MockSheet({
  title,
  open,
  onOpenChange,
  children,
  fit = false,
}: {
  title: string
  open: boolean
  onOpenChange: (open: boolean) => void
  children: ReactNode
  fit?: boolean
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogOverlay />
        <DialogPrimitive.Popup
          className={
            'fixed inset-x-0 bottom-0 z-[60] flex flex-col rounded-t-2xl bg-card pt-2 pb-[max(1.5rem,env(safe-area-inset-bottom))] text-foreground shadow-lg outline-none ' +
            (fit ? 'max-h-[85dvh]' : (window as unknown as { __mock?: string }).__mock === 'A' ? 'h-[60dvh]' : 'h-[85dvh]')
          }
        >
          <div aria-hidden className="mx-auto h-1 w-9 shrink-0 rounded-full bg-border" />
          <div className="grid shrink-0 grid-cols-[1fr_auto_1fr] items-center px-2">
            <span />
            <DialogPrimitive.Title className="text-base font-semibold">{title}</DialogPrimitive.Title>
            <DialogPrimitive.Close className="h-11 justify-self-end rounded-lg px-3 text-base font-semibold text-primary hover:bg-muted">
              完了
            </DialogPrimitive.Close>
          </div>
          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4 pb-1">{children}</div>
        </DialogPrimitive.Popup>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
