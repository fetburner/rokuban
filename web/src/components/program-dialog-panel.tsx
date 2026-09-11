import { DialogTitle } from '@/components/ui/dialog'
import {
  ProgramReservationActions,
  ProgramReservationBody,
  ProgramReservationSummary,
  useProgramReservation,
  type ProgramReservationProgram,
} from '@/components/program-reservation'
import type { ProgramOverlaps, ProgramOverridesInput } from '@/api/generated'

/**
 * グリッドから開く番組予約パネル。
 *
 * `ProgramRow` はリスト専用の段階的開示と操作列を持つため、ダイアログで再利用しない。
 * 予約の意味・詳細・encode 規則・ボタンは chrome なしの共有部品を使い、ここでは
 * ダイアログの見出し・常時表示の操作面・余白だけを決める。
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
      <ProgramReservationSummary
        program={program}
        serviceName={serviceName}
        siteName={siteName}
        reserved={reserved}
        overlaps={overlaps}
        title={
          <DialogTitle className="pr-14 break-words">{program.name}</DialogTitle>
        }
      />

      {/*
       * 操作面は要約の直後に置く。長い番組説明の後までスクロールせずに、開いた時点で
       * 予約 / 取消 / ライブへ届くようにするためである。本文と一緒に流れるので
       * スクロール中は固定しない（本文を読む最中も画面外へ出ないのは閉じるボタンだけ）。
       * リストの 80px / 124px の幅アニメーションはここへ持ち込まない。
       */}
      <div
        data-testid="program-dialog-actions"
        className="flex w-full items-stretch gap-2"
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
