import type { ProgramOverlaps, Reservation } from '@/api/generated'
import type { SiteProgram } from '@/lib/all-sites-services'

/**
 * deriveProgramOverlaps は、予約一覧から番組表の重なり警告を導出する。
 *
 * 予約一覧が返すのは開始時刻と尺なので、予約の終了時刻はそこから求める。
 * 区間は半開区間として扱い、同じ site・別の programId・非 orphaned・非 skip
 * の予約だけを対象にする。`reservations` の順序はそのまま内訳へ引き継ぐ。
 */
export function deriveProgramOverlaps(
  program: SiteProgram,
  reservations: readonly Reservation[],
): ProgramOverlaps {
  const programStartMs = new Date(program.startAt).getTime()
  const programEndMs = new Date(program.endAt).getTime()
  const overlapping = reservations.filter((reservation) => {
    if (reservation.site !== program.site) return false
    if (reservation.programId === program.programId) return false
    if (reservation.state === 'orphaned') return false
    if (reservation.skip !== false) return false

    const reservationStartMs = new Date(reservation.startAt).getTime()
    const reservationEndMs = reservationStartMs + reservation.durationMs
    return reservationStartMs < programEndMs && programStartMs < reservationEndMs
  })

  return {
    count: overlapping.length,
    reservations: overlapping.map(({ programId, title, startAt, durationMs }) => ({
      programId,
      title,
      startAt,
      durationMs,
    })),
  }
}
