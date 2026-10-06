import { Dialog as DialogPrimitive } from '@base-ui/react/dialog'
import { Popover as PopoverPrimitive } from '@base-ui/react/popover'

import { useMediaQuery } from '@/lib/use-media-query'

/**
 * ResponsivePanel は広い幅ではポップオーバー、狭い幅では下から出るシートで中身を出す
 * （HIG「狭い画面ではポップオーバーを避け、シートで出す」）。ラフ用。
 */
export function ResponsivePanel({
  open,
  onOpenChange,
  title,
  triggerClassName,
  trigger,
  popupClassName,
  children,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  triggerClassName: string
  trigger: React.ReactNode
  popupClassName: string
  children: React.ReactNode
}) {
  const wide = useMediaQuery('(min-width: 48rem)')

  if (wide) {
    return (
      <PopoverPrimitive.Root open={open} onOpenChange={onOpenChange}>
        <PopoverPrimitive.Trigger className={triggerClassName}>
          {trigger}
        </PopoverPrimitive.Trigger>
        <PopoverPrimitive.Portal>
          <PopoverPrimitive.Positioner className="z-50 outline-none" positionMethod="fixed" side="bottom" align="start" sideOffset={6}>
            <PopoverPrimitive.Popup aria-label={title} className={popupClassName}>
              {children}
            </PopoverPrimitive.Popup>
          </PopoverPrimitive.Positioner>
        </PopoverPrimitive.Portal>
      </PopoverPrimitive.Root>
    )
  }

  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Trigger className={triggerClassName}>
        {trigger}
      </DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Backdrop className="fixed inset-0 z-50 bg-black/30" />
        <DialogPrimitive.Popup
          aria-label={title}
          className="fixed inset-x-0 bottom-0 z-50 flex max-h-[85dvh] flex-col rounded-t-2xl bg-popover pb-[env(safe-area-inset-bottom,0px)] text-popover-foreground shadow-lg outline-none"
        >
          <div className="mx-auto mt-2 h-1 w-9 rounded-full bg-muted-foreground/30" aria-hidden />
          <div className="grid grid-cols-[1fr_auto_1fr] items-center px-4 py-2">
            <span />
            <DialogPrimitive.Title className="text-base font-semibold">{title}</DialogPrimitive.Title>
            <DialogPrimitive.Close className="justify-self-end rounded-md px-2 py-2 text-base font-semibold text-primary">完了</DialogPrimitive.Close>
          </div>
          <div className="flex flex-col gap-4 overflow-y-auto px-4 pb-4">{children}</div>
        </DialogPrimitive.Popup>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
