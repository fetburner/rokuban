import { DialogTitle } from '@/components/ui/dialog'
import {
  ProgramReservationActions,
  ProgramReservationBody,
  ProgramReservationSummary,
  useProgramReservation,
  type ProgramReservationProgram,
} from '@/components/program-reservation'
import type { ProgramOverlaps, ProgramOverridesInput } from '@/api/generated'
import { cn } from '@/lib/utils'

/**
 * グリッドから開く番組予約パネル。
 *
 * `ProgramRow` はリスト専用の段階的開示と操作列を持つため、ダイアログで再利用しない。
 * 予約の意味・詳細・encode 規則・ボタンは chrome なしの共有部品を使い、ここでは
 * ダイアログの見出し・要約行右側の固定操作列・余白だけを決める。閉じるボタンの
 * 占有領域を避けて操作列を左へ寄せ、タイトルと上端を揃えながら、開いた時点で
 * 1 操作・44px 以上のタップ領域を保つため、通常 80px、放送中 124px の幅を選ぶ。
 */
export function ProgramDialogPanel({
  program,
  serviceName,
  siteName,
  reserved,
  pending,
  reservationStateUnknown,
  overlaps,
  onReserve,
  onCancel,
}: {
  program: ProgramReservationProgram
  serviceName?: string
  siteName?: string
  reserved: boolean
  pending: boolean
  reservationStateUnknown: boolean
  overlaps?: ProgramOverlaps
  onReserve: (overrides?: ProgramOverridesInput) => void
  onCancel: () => void
}) {
  const draft = useProgramReservation({
    program,
    pending,
    reservationStateUnknown,
    onReserve,
  })

  return (
    <div data-testid="program-dialog-panel" className="flex flex-col gap-5">
      <div
        data-testid="program-dialog-summary-row"
        className="flex min-w-0 items-start gap-3"
      >
        <ProgramReservationSummary
          program={program}
          serviceName={serviceName}
          siteName={siteName}
          reserved={reserved}
          overlaps={overlaps}
          title={
            <DialogTitle className="break-words">{program.name}</DialogTitle>
          }
        />

        {/* 閉じるボタンの左側へ寄せた操作列。詳細と一緒に流れ、固定はしない。 */}
        <div
          data-testid="program-dialog-actions"
          className={cn(
            'mr-5 flex shrink-0 items-center justify-center overflow-hidden border-l border-border box-content',
            draft.showLiveLink ? 'w-[7.75rem]' : 'w-20',
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

      <div className="flex flex-col gap-2 text-xs">
        <ProgramReservationBody
          program={program}
          reserved={reserved}
          pending={pending}
          draft={draft}
        />
      </div>
    </div>
  )
}
