import type { CapacityOverage, Reservation } from '@/api/generated'
import { intersectingOverages, overageWindow, worstOverage } from '@/lib/capacity'
import { programTitle } from '@/lib/program-labels'

export type ReservationGroup = {
  /** シリーズは値、series=null は予約ごとの site/programId で識別する。 */
  key: string
  series: string | null
  title: string
  reservations: Reservation[]
  /** スキップを飛ばした次回。すべてスキップなら最初の予約。 */
  next: Reservation
  /** 状態・容量不足・重複スキップを予約ごとに数える。 */
  badges: {
    orphaned: number
    detached: number
    capacityShortfall: number
    duplicateSkipped: number
  }
  /** 集約した容量バッジの遷移先と説明に使う、最も不足の大きい区間。 */
  capacityTarget?: CapacityOverage
}

/**
 * groupReservations はすでに絞り込まれた予約を実効シリーズごとにまとめる。
 *
 * null は共有可能なシリーズ値ではないため、同じ null 同士もまとめない。
 * グループの並びはスキップを除いた次回の開始時刻で決め、次回候補が全てスキップなら
 * 最初の予約を使う。「今後 N 本」と各バッジは、入力に残った予約行の件数である。
 */
export function groupReservations(
  reservations: readonly Reservation[],
  overages: readonly CapacityOverage[],
): ReservationGroup[] {
  const grouped = new Map<string, { series: string | null; reservations: Reservation[] }>()

  for (const reservation of reservations) {
    const series = reservation.series ?? null
    const key = series === null
      ? `reservation:${reservation.site}:${reservation.programId}`
      : `series:${series}`
    const group = grouped.get(key)
    if (group) group.reservations.push(reservation)
    else grouped.set(key, { series, reservations: [reservation] })
  }

  const rows = [...grouped].map(([key, group]): ReservationGroup => {
    const episodes = [...group.reservations].sort(compareReservations)
    const next = episodes.find((reservation) => !reservation.skip) ?? episodes[0]
    let capacityShortfall = 0
    const relatedOverages: CapacityOverage[] = []

    for (const reservation of episodes) {
      const startMs = Date.parse(reservation.startAt)
      const intersecting = intersectingOverages(
        overages,
        reservation.site,
        startMs,
        startMs + reservation.durationMs,
      )
      if (intersecting.length === 0) continue
      capacityShortfall += 1
      relatedOverages.push(...intersecting)
    }

    return {
      key,
      series: group.series,
      title: group.series ?? programTitle(episodes[0].title),
      reservations: episodes,
      next,
      badges: {
        orphaned: episodes.filter((reservation) => reservation.state === 'orphaned').length,
        detached: episodes.filter((reservation) => reservation.state === 'detached').length,
        capacityShortfall,
        duplicateSkipped: episodes.filter(
          (reservation) => reservation.skip && reservation.dedupMatchRecordingId !== undefined,
        ).length,
      },
      capacityTarget: worstOverage(relatedOverages) ?? undefined,
    }
  })

  return rows.sort((a, b) => {
    const byNext = Date.parse(a.next.startAt) - Date.parse(b.next.startAt)
    return byNext || a.title.localeCompare(b.title, 'ja') || a.key.localeCompare(b.key, 'ja')
  })
}

function compareReservations(a: Reservation, b: Reservation): number {
  return (
    Date.parse(a.startAt) - Date.parse(b.startAt) ||
    a.site.localeCompare(b.site, 'ja') ||
    a.programId - b.programId
  )
}

/** 代表する容量不足区間の時刻。バッジから番組表の該当時間へ進む。 */
export function reservationGroupCapacityAt(group: ReservationGroup): number | undefined {
  const target = group.capacityTarget
  return target === undefined ? undefined : overageWindow(target).startMs
}
