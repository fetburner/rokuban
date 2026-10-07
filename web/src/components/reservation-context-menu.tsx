import type { ReactElement, RefObject } from 'react'
import { Link } from '@tanstack/react-router'
import { Copy, ExternalLink, FolderOpen, Trash2 } from 'lucide-react'

import type { Reservation } from '@/api/generated'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLinkItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import { useCopyLink } from '@/lib/use-copy-link'
import { useMediaQuery } from '@/lib/use-media-query'

/** ReservationContextMenu は細いポインタの予約行に、既存操作を右クリックからも提供する。 */
export function ReservationContextMenu({
  reservation,
  rowRef,
  onCancel,
  cancelPending = false,
  children,
}: {
  reservation: Reservation
  rowRef: RefObject<HTMLElement | null>
  onCancel: () => void
  /** 取消の処理中。二重に skip を送らないよう項目を無効にする。 */
  cancelPending?: boolean
  children: ReactElement
}) {
  const finePointer = useMediaQuery('(pointer: fine)')
  const detailPath = `/reservations/${encodeURIComponent(reservation.site)}/${reservation.programId}`
  const copyLink = useCopyLink(detailPath)

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
          <FolderOpen />
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
        <ContextMenuItem variant="destructive" disabled={cancelPending} onClick={onCancel}>
          <Trash2 />
          予約を取消
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  )
}
