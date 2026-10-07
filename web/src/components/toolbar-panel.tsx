import { Dialog as DialogPrimitive } from '@base-ui/react/dialog'
import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { useState, type ReactNode } from 'react'
import { ChevronLeft } from 'lucide-react'
import { PanelCtx } from '@/components/mock-sheet'

import { DialogOverlay } from '@/components/ui/dialog'

import { mdMediaQuery, useMediaQuery } from '@/lib/use-media-query'
import { cn } from '@/lib/utils'

/**
 * toolbarButtonClass は録画検索のツールバーに並ぶボタン（期間・絞り込み・並び順）の外枠。
 * md 未満は枠なしの 44×44px アイコン、md 以上は文字とシェブロンを持つ枠付きのポップアップ
 * ボタン（HIG の押せる領域 44pt と pop-up button）。3 つが同じ理由で変わるので 1 つにする。
 */
export const toolbarButtonClass =
  'relative flex size-11 shrink-0 items-center justify-center rounded-lg text-foreground transition-colors hover:bg-muted aria-expanded:bg-muted md:w-auto md:gap-1.5 md:border md:border-border md:bg-background md:px-3 md:text-sm'

/** ToolbarDot はアイコンだけのボタンに条件がかかっていることを示す右上の点（md 未満だけ）。 */
export function ToolbarDot() {
  return <span aria-hidden className="absolute top-2 right-2 size-2 rounded-full bg-primary md:hidden" />
}

/**
 * ToolbarPanel は録画検索のツールバーから開くパネル。md 以上はトリガーの下のポップオーバー、
 * md 未満は画面下から出るシートにする（HIG「狭い画面ではポップオーバーを避け、シートで出す」）。
 *
 * 呼び出し元は期間と絞り込みの 2 つで、どちらも「ツールバーのパネルを狭い幅でどう出すか」で
 * 変わる。片方だけシートの寸法を変える理由が無いので 1 つにする。録画詳細の再生設定・チャプター
 * 一覧のシート（`recording-playback-controls.tsx` の `popoverClass`）とは共有しない ---
 * あちらは動画の上に重なるメニュー（role="menu"・md 以上は動画内の小窓）で、プレイヤーの都合で
 * 変わる。見た目（角丸・セーフエリア・つまみ）だけ揃える。幕はアプリのダイアログと同じ
 * `DialogOverlay` を使う（色をトークン外で持たない）。
 *
 * `bodyClassName` は中身の並べ方（両方の形で同じ）。`popupWidthClassName` はポップオーバーの幅。
 */
export function ToolbarPanel({
  title,
  open,
  onOpenChange,
  trigger,
  triggerClassName,
  popupWidthClassName,
  bodyClassName,
  children,
}: {
  title: string
  open: boolean
  onOpenChange: (open: boolean) => void
  trigger: ReactNode
  triggerClassName: string
  popupWidthClassName: string
  bodyClassName: string
  children: ReactNode
}) {
  const wide = useMediaQuery(mdMediaQuery)
  const [pushed, setPushed] = useState(false)
  const [slot, setSlot] = useState<HTMLElement | null>(null)

  if (wide) {
    return (
      <PopoverPrimitive.Root open={open} onOpenChange={onOpenChange}>
        <PopoverPrimitive.Trigger className={triggerClassName}>{trigger}</PopoverPrimitive.Trigger>
        <PopoverPrimitive.Portal>
          {/* positionMethod は 'fixed'。理由は components/channel-picker.tsx と同じ
              （sticky なトリガーと 'absolute' ポップアップのスクロール追従のずれ）。 */}
          <PopoverPrimitive.Positioner
            className="z-50 outline-none"
            positionMethod="fixed"
            side="bottom"
            align="start"
            sideOffset={6}
          >
            <PopoverPrimitive.Popup
              aria-label={title}
              className={cn(
                'max-h-[min(34rem,80vh)] overflow-y-auto rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-md outline-none',
                popupWidthClassName,
                bodyClassName,
              )}
            >
              {children}
            </PopoverPrimitive.Popup>
          </PopoverPrimitive.Positioner>
        </PopoverPrimitive.Portal>
      </PopoverPrimitive.Root>
    )
  }

  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Trigger className={triggerClassName}>{trigger}</DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogOverlay />
        {/* 角丸・セーフエリア・つまみは録画詳細の再生設定シート（popoverClass）に揃える。
            高さは中身が多い（絞り込みは 6 節）ので 85dvh まで許し、中身だけをスクロールさせる。 */}
        <DialogPrimitive.Popup className="fixed inset-x-0 bottom-0 z-50 flex max-h-[85dvh] flex-col rounded-t-2xl data-[nested-dialog-open]:scale-[0.96] data-[nested-dialog-open]:brightness-95 origin-bottom transition-transform bg-card pt-2 pb-[max(1.5rem,env(safe-area-inset-bottom))] text-foreground shadow-lg outline-none">
          <div aria-hidden className="mx-auto h-1 w-9 shrink-0 rounded-full bg-border" />
          <div className="grid shrink-0 grid-cols-[1fr_auto_1fr] items-center px-2">
            {pushed ? (
              <button type="button" onClick={() => setPushed(false)} className="flex h-11 items-center gap-0.5 justify-self-start rounded-lg pr-3 pl-1 text-base text-primary hover:bg-muted">
                <ChevronLeft className="size-5" />
                {title}
              </button>
            ) : (
              <span />
            )}
            <DialogPrimitive.Title className="text-base font-semibold">{pushed ? 'チャンネル' : title}</DialogPrimitive.Title>
            <DialogPrimitive.Close className="h-11 justify-self-end rounded-lg px-3 text-base font-semibold text-primary hover:bg-muted">
              完了
            </DialogPrimitive.Close>
          </div>
          <div data-testid="toolbar-sheet-body" className={cn('min-h-0 overflow-y-auto px-4 pb-1', bodyClassName)}>
            <PanelCtx.Provider value={{ slot, pushed, setPushed }}>
              <div className={cn('contents', pushed && 'hidden')}>{children}</div>
            </PanelCtx.Provider>
            <div ref={setSlot} className={cn('flex min-h-0 flex-col', !pushed && 'hidden')} />
          </div>
        </DialogPrimitive.Popup>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}
