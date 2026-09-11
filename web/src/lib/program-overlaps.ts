import type { ProgramOverlaps, Reservation } from '@/api/generated'
import type { SiteProgram } from '@/lib/all-sites-services'

/**
 * 重なり判定に必要な番組の最小形。検索結果は `endAt` を持たないため、指定されて
 * いなければ `startAt + durationMs` を終了時刻として使う。
 */
export type ProgramOverlapTarget = Pick<
  SiteProgram,
  'site' | 'programId' | 'startAt' | 'durationMs'
> & { endAt?: string }

/**
 * ReservationOverlapEntry は `deriveProgramOverlaps` が索引を引くための
 * 事前パース済み予約 1 件分。`useReservationActions` が `reservations` から
 * 1 回だけ作る（`web/src/lib/reservation-actions.ts` 参照）。`startMs` /
 * `endMs` を都度 `Date.parse` しないための形。
 */
export type ReservationOverlapEntry = {
  site: string
  programId: number
  title: string
  startAt: string
  durationMs: number
  startMs: number
  endMs: number
}

/**
 * deriveProgramOverlaps は、事前パース済みの予約索引から番組表の重なり警告を導出する。
 *
 * 区間は半開区間として扱い、同じ site・別の programId で重なる予約だけを
 * 対象にする（`state === 'orphaned'` と `skip !== false` の除外は索引を作る側
 * （`useReservationActions`）が既に済ませている）。`reservations` の順序は
 * そのまま内訳へ引き継ぐ。
 *
 * **サーバーの overlaps API とはズレうる。** サーバー
 * （`internal/db/queries/overlaps.sql` の `ListOverlappingReservations`）は
 * `NOT EXISTS never_scheduled_events` だけで除外するが、クライアントは
 * `never_scheduled_events` を持たず `reservation.state`（`internal/api/handler.go`
 * の `reservationState` が導出）で近似する。`state` が `orphaned` になるのは
 * 「`never_scheduled_events` の行が有りかつ `recordings` の行が無い」ときだけ
 * なので、「never-scheduled と記録されたが後に本物の `recordings` 行ができた
 * 放送イベント」はサーバーでは除外されるがクライアントでは `active` のまま
 * 重なりに数えてしまう。クライアントは `never_scheduled_events` の情報を
 * 受け取っていないため直す手段が無く、`state` が使える最良の近似。
 */
export function deriveProgramOverlaps(
  program: ProgramOverlapTarget,
  reservations: readonly ReservationOverlapEntry[],
): ProgramOverlaps {
  const programStartMs = new Date(program.startAt).getTime()
  const programEndMs =
    program.endAt === undefined ? programStartMs + program.durationMs : Date.parse(program.endAt)
  const overlapping = reservations.filter((reservation) => {
    if (reservation.site !== program.site) return false
    if (reservation.programId === program.programId) return false
    return reservation.startMs < programEndMs && programStartMs < reservation.endMs
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

/**
 * buildReservationOverlapIndex は生の予約一覧から `deriveProgramOverlaps` 用の
 * 索引を作る。`orphaned` と `skip !== false` の予約を除外し、開始・終了時刻を
 * 一度だけパースする。
 */
export function buildReservationOverlapIndex(
  reservations: readonly Reservation[],
): ReservationOverlapEntry[] {
  return reservations
    .filter((r) => r.state !== 'orphaned' && r.skip === false)
    .map((r) => {
      const startMs = new Date(r.startAt).getTime()
      return {
        site: r.site,
        programId: r.programId,
        title: r.title,
        startAt: r.startAt,
        durationMs: r.durationMs,
        startMs,
        endMs: startMs + r.durationMs,
      }
    })
}
