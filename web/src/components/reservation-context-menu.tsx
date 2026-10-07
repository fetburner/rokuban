import type { ReactElement, RefObject } from 'react'
import { Link } from '@tanstack/react-router'
import { Copy, ExternalLink, Trash2 } from 'lucide-react'

import type { Reservation } from '@/api/generated'
import { useToast } from '@/components/toaster'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLinkItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import { useMediaQuery } from '@/lib/use-media-query'

/** ReservationContextMenu は細いポインタの予約行に、既存操作を右クリックからも提供する。 */
export function ReservationContextMenu({
  reservation,
  rowRef,
  onCancel,
  children,
}: {
  reservation: Reservation
  rowRef: RefObject<HTMLElement | null>
  onCancel: () => void
  children: ReactElement
}) {
  const finePointer = useMediaQuery('(pointer: fine)')
  const toast = useToast()
  const detailPath = `/reservations/${encodeURIComponent(reservation.site)}/${reservation.programId}`
  const copyLink = async () => {
    try {
      await navigator.clipboard.writeText(new URL(detailPath, window.location.origin).href)
      toast({ message: 'リンクをコピーしました' })
    } catch {
      toast({ message: 'リンクをコピーできませんでした', kind: 'error' })
    }
  }

  if (!finePointer) return children

  return (
    <ContextMenu>
      <ContextMenuTrigger render={children} />
      <ContextMenuContent returnFocusRef={rowRef}>
        <ContextMenuLinkItem
          closeOnClick
          render={
            <Link
              to="/reservations/$site/$programId"
              params={{ site: reservation.site, programId: String(reservation.programId) }}
            />
          }
        >
          <ExternalLink />
          開く
        </ContextMenuLinkItem>
        <ContextMenuLinkItem
          closeOnClick
          render={
            <Link
              to="/reservations/$site/$programId"
              params={{ site: reservation.site, programId: String(reservation.programId) }}
              target="_blank"
              rel="noopener noreferrer"
            />
          }
        >
          <ExternalLink />
          新しいタブで開く
        </ContextMenuLinkItem>
        <ContextMenuItem onClick={() => void copyLink()}>
          <Copy />
          リンクをコピー
        </ContextMenuItem>
        <ContextMenuSeparator />
        <ContextMenuItem variant="destructive" onClick={onCancel}>
          <Trash2 />
          予約を取消
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}
