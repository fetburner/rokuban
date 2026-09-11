import { ChevronDown } from 'lucide-react'
import { useState } from 'react'

import type { ProgramOverlaps, ProgramOverridesInput } from '@/api/generated'
import {
  ProgramReservationActions,
  ProgramReservationBody,
  ProgramReservationSummary,
  useProgramReservation,
  type ProgramReservationProgram,
} from '@/components/program-reservation'
import { cn } from '@/lib/utils'

/** 既存の import 先を保ちつつ、予約 UI の共有モデルを公開する。 */
export type { ProgramReservationProgram as ProgramRowProgram } from '@/components/program-reservation'

/**
 * ProgramRow は番組リスト / 検索結果の 1 行。
 *
 * 番組の要約・詳細・encode 下書き・予約可否・操作ボタンは
 * `program-reservation.tsx` と共有するが、ここではリスト固有の chrome だけを持つ。
 * 行本体のタップで詳細を展開し、右端の操作列は hover / フォーカス / 展開時だけ
 * 幅を持つ。ダイアログはこのコンポーネントを使わず、リスト chrome が漏れないようにする。
 */
export function ProgramRow({
  program,
  serviceName,
  siteName,
  reserved,
  pending,
  reservationStateUnknown,
  onReserve,
  onCancel,
  overlaps,
}: {
  program: ProgramReservationProgram
  serviceName?: string
  siteName?: string
  reserved: boolean
  pending: boolean
  /** 予約一覧が未取得・失敗中のとき、未予約側の操作を止める。 */
  reservationStateUnknown: boolean
  onReserve: (overrides?: ProgramOverridesInput) => void
  onCancel: () => void
  /** 予約一覧から導出した重なり。 */
  overlaps?: ProgramOverlaps
}) {
  const [expanded, setExpanded] = useState(false)
  const draft = useProgramReservation({
    program,
    pending,
    reservationStateUnknown,
    onReserve,
  })

  const detailId = `program-row-detail-${program.site}-${program.programId}`
  // リストの操作列は、予約ボタン 80px と放送中のライブボタン 44px を
  // まとめて開く。これはリストだけの幅アニメーションで、共有操作側へ渡さない。
  const reserveColumnOpenClasses = cn(
    'pointer-fine:group-hover:border-l group-has-[:focus-visible]:border-l peer-aria-expanded:border-l',
    draft.showLiveLink
      ? 'pointer-fine:group-hover:w-[7.75rem] group-has-[:focus-visible]:w-[7.75rem] peer-aria-expanded:w-[7.75rem]'
      : 'pointer-fine:group-hover:w-20 group-has-[:focus-visible]:w-20 peer-aria-expanded:w-20',
  )

  return (
    <div className="flex flex-col border-b border-border" data-testid="program-row">
      <div className="group flex items-stretch">
        <button
          type="button"
          aria-expanded={expanded}
          aria-controls={detailId}
          onClick={() => setExpanded((value) => !value)}
          // `peer` はタッチ / 粗いポインタで展開中の操作列を開くためのマーカー。
          // 操作列より先に来る兄弟なので `peer-aria-expanded:` が使える。
          className="peer flex min-h-14 min-w-0 flex-1 items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/40"
        >
          <ProgramReservationSummary
            program={program}
            serviceName={serviceName}
            siteName={siteName}
            reserved={reserved}
            overlaps={overlaps}
          />
          <ChevronDown
            className={cn(
              'size-4 shrink-0 text-muted-foreground transition-transform',
              expanded && 'rotate-180',
            )}
          />
        </button>

        {/*
         * リストの操作列は幅 0 + overflow-hidden で畳む。opacity だけで隠すと
         * 見えない予約ボタンがヒットテストと Tab 順序に残り、スクロール中の誤操作を
         * 防ぐというリストの理由を失う。共有側はボタンそのものだけを描画し、ここで
         * 80px / 124px、hover / focus / 展開の規則を決める。
         */}
        <div
          data-testid="program-row-reserve"
          className={cn(
            'flex w-0 shrink-0 items-center justify-center overflow-hidden border-border box-content',
            'transition-[width] duration-150 motion-reduce:transition-none',
            reserveColumnOpenClasses,
          )}
        >
          <ProgramReservationActions
            program={program}
            reserved={reserved}
            pending={pending}
            reserveBlocked={draft.reserveBlocked}
            showLiveLink={draft.showLiveLink}
            onReserve={draft.handleReserve}
            onCancel={onCancel}
          />
        </div>
      </div>

      {expanded && (
        <div id={detailId} className="px-4 pb-3">
          <ProgramReservationBody
            program={program}
            reserved={reserved}
            pending={pending}
            draft={draft}
          />
        </div>
      )}
    </div>
  )
}
