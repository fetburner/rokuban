import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { useMemo, useState } from 'react'

import {
  ListRecordingShelvesKey,
  useListCapacityOverages,
  useListRecordingShelves,
  useListReservations,
  useListRules,
  useListSites,
  type CapacityOverage,
  type RecordingShelf,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { ReservationGroupToggle } from '@/components/reservation-group-toggle'
import { ReservationSeriesRow } from '@/components/reservation-series-row'
import { ReservationOrigin, ReservationVerdictBadge } from '@/components/reservation-row-parts'
import { Chip } from '@/components/ui/chip'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { coveringWindow } from '@/lib/capacity'
import { calendarDayDiff, dayKey, formatDate, formatDuration, formatTime } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import {
  reservationNeedsAttention,
  reservationRowLabel,
  type ReservationsPageSearch,
} from '@/lib/reservation-labels'
import { shouldShowRecordingSite } from '@/lib/recording-search'
import { makeRuleLabel } from '@/lib/rule-label'
import { groupReservations } from '@/lib/reservation-groups'
import {
  loadReservationGrouping,
  saveReservationGrouping,
  type ReservationGrouping,
} from '@/lib/reservation-grouping'

export function ReservationsPage() {
  const search = useRouteSearch({ from: '/reservations' })
  const navigate = useNavigate()
  const [grouping, setGrouping] = useState(loadReservationGrouping)
  const query = useListReservations()
  const rulesQuery = useListRules()
  const sitesQuery = useListSites()
  const shelvesQuery = useListRecordingShelves(
    { key: ListRecordingShelvesKey.series },
    { query: { enabled: grouping === 'series' } },
  )
  const reservations = useMemo(() => unwrap(query.data) ?? [], [query.data])
  const rules = useMemo(() => unwrap(rulesQuery.data) ?? [], [rulesQuery.data])

  // 一覧に出ている予約すべてを覆う窓で超過区間を訊く。窓を固定幅にすると、
  // その外に出た予約のバッジが黙って消える。予約が無ければ問い合わせない
  // （窓が null。パラメータは必須なので値は入れるが enabled で止める）。
  const listedWindow = useMemo(() => coveringWindow(reservations), [reservations])
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(listedWindow?.startMs ?? 0).toISOString(),
      end: new Date(listedWindow?.endMs ?? 0).toISOString(),
    },
    { query: { enabled: listedWindow !== null } },
  )
  // 成功した容量データだけを判定に使う。失敗時は React Query が前回成功した
  // data を残すことがあるが、それを最新の確認結果として表示してはいけない。
  // 容量取得の失敗・未完了は「不足なし」ではなく未確認なので、沈黙を保証にしない
  // 方針（docs/data.md §6.5）を一覧の絞り込みにも適用する。
  const overages = useMemo(
    () => (overagesQuery.isSuccess ? (unwrap(overagesQuery.data) ?? []) : []),
    [overagesQuery.data, overagesQuery.isSuccess],
  )
  // 初回の結果を待つ間だけ true を保留する。失敗（isError）でも「容量抜きの
  // 下界」として要確認を出す --- orphaned の分だけでも導線を消さない
  // ため（容量の判定が不完全なことは capacityUnavailable のバナーが別に言う）。
  const attentionReady = listedWindow === null || !overagesQuery.isPending
  const capacityUnavailable = listedWindow !== null && overagesQuery.isError
  const ruleFilteredReservations = useMemo(
    () =>
      search.ruleId === undefined
        ? reservations
        : reservations.filter((reservation) => reservation.ruleId === search.ruleId),
    [reservations, search.ruleId],
  )
  const attentionReservations = useMemo(
    () => ruleFilteredReservations.filter((reservation) => reservationNeedsAttention(reservation, overages)),
    [overages, ruleFilteredReservations],
  )
  const displayedReservations =
    search.only === 'attention' ? attentionReservations : ruleFilteredReservations
  const groupedReservations = useMemo(() => groupByLocalDay(displayedReservations), [displayedReservations])
  const reservationGroups = useMemo(
    () => groupReservations(displayedReservations, overages),
    [displayedReservations, overages],
  )
  // 棚の存在/不在は、取得が成功しているときだけ主張する。初回の取得中と失敗は
  // 「棚をまだ知らない」であって「棚が無い」ではない。再取得中は成功済みのデータを
  // そのまま使う（箱を消すと周期再取得のたびに行が組み直される）。再取得が失敗すると
  // status が error になり isSuccess は false に戻るので、古い棚を主張し続けない。
  const shelvesKnown = shelvesQuery.isSuccess
  const shelvesByValue = useMemo(() => {
    if (!shelvesKnown) return undefined
    const byValue = new Map<string, RecordingShelf>()
    for (const shelf of unwrap(shelvesQuery.data) ?? []) {
      // value=null の棚は番組ハブを開けない。null series の予約とも突き合わせない。
      if (shelf.value !== undefined && shelf.value !== null) byValue.set(shelf.value, shelf)
    }
    return byValue
  }, [shelvesKnown, shelvesQuery.data])
  const registeredSites = useMemo(() => unwrap(sitesQuery.data) ?? [], [sitesQuery.data])
  const showSite = useMemo(
    () => shouldShowRecordingSite(registeredSites, reservations.map((reservation) => reservation.site)),
    [registeredSites, reservations],
  )
  const ruleLabel = useMemo(() => makeRuleLabel(rules), [rules])
  const rulesWithReservations = useMemo(
    () => rulesWithReservationCounts(reservations, ruleLabel),
    [reservations, ruleLabel],
  )
  const selectSearch = (patch: Partial<ReservationsPageSearch>) => {
    void navigate({
      to: '/reservations',
      search: (previous) => ({ ...(previous as ReservationsPageSearch), ...patch }),
      replace: true,
    })
  }
  const changeGrouping = (next: ReservationGrouping) => {
    setGrouping(next)
    saveReservationGrouping(next)
  }

  return (
    <>
      <PageHeader
        title="予約"
        actions={<ReservationGroupToggle grouping={grouping} onChange={changeGrouping} />}
      >
        {!query.isPending && !query.isError && (
          <div role="group" aria-label="予約の絞り込み" className="flex flex-wrap gap-2 px-4 pb-3">
            <Chip active={search.only === undefined} onClick={() => selectSearch({ only: undefined })}>
              すべて（{ruleFilteredReservations.length}）
            </Chip>
            {attentionReady && (attentionReservations.length > 0 || search.only === 'attention') && (
              <Chip active={search.only === 'attention'} onClick={() => selectSearch({ only: 'attention' })}>
                要確認（{attentionReservations.length}）
              </Chip>
            )}
            {(rulesWithReservations.length > 0 || search.ruleId !== undefined) && (
              search.ruleId === undefined ? (
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <button
                        type="button"
                        aria-label="ルールで絞り込む"
                        className="flex min-h-7 pointer-coarse:min-h-11 max-w-full shrink-0 items-center gap-1 rounded-full border border-border px-3 py-1.5 text-xs text-muted-foreground transition-colors outline-none hover:bg-muted hover:text-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
                      />
                    }
                  >
                    ルール
                    <ChevronDown aria-hidden className="size-3.5" />
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="start" className="min-w-48">
                    {rulesWithReservations.map(({ id, label, count }) => (
                      <DropdownMenuItem
                        key={id}
                        className="min-h-6"
                        onClick={() => selectSearch({ ruleId: id })}
                      >
                        <span className="min-w-0 truncate">{label}</span>
                        <span className="ml-auto shrink-0 text-muted-foreground">{count}</span>
                      </DropdownMenuItem>
                    ))}
                  </DropdownMenuContent>
                </DropdownMenu>
              ) : (
                <>
                  <button
                    type="button"
                    aria-label={`ルール「${ruleLabel(search.ruleId)}」の絞り込みを解除`}
                    onClick={() => selectSearch({ ruleId: undefined })}
                    className="flex min-h-7 pointer-coarse:min-h-11 max-w-full shrink-0 items-center rounded-full border border-primary px-3 py-1.5 text-xs text-foreground transition-colors outline-none hover:bg-muted focus-visible:ring-3 focus-visible:ring-ring/50"
                  >
                    <span className="min-w-0 truncate">ルール「{ruleLabel(search.ruleId)}」 ×</span>
                  </button>
                  <Link
                    to="/search"
                    search={{ ruleId: search.ruleId }}
                    className="flex min-h-6 pointer-coarse:min-h-11 items-center rounded px-1 text-xs text-primary underline underline-offset-2"
                  >
                    ルールの条件を直す
                  </Link>
                </>
              )
            )}
          </div>
        )}
      </PageHeader>

      <PageContent>
        {!query.isError && capacityUnavailable && (
          <ErrorState onRetry={() => void overagesQuery.refetch()}>
            容量の確認に失敗しました。要確認の判定が不完全です
          </ErrorState>
        )}
        {query.isError ? (
          <ErrorState onRetry={() => void query.refetch()}>予約の取得に失敗しました</ErrorState>
        ) : query.isPending || (search.only === 'attention' && !attentionReady) ? (
          <ListSkeleton />
        ) : grouping === 'series' && reservationGroups.length > 0 ? (
          <ul aria-label="シリーズ別の予約">
            {reservationGroups.map((group) => (
              <ReservationSeriesRow
                key={group.key}
                group={group}
                overages={overages}
                shelvesKnown={shelvesByValue !== undefined}
                shelf={group.series === null ? undefined : shelvesByValue?.get(group.series)}
                ruleLabel={ruleLabel}
                showSite={showSite}
              />
            ))}
          </ul>
        ) : grouping === 'time' && displayedReservations.length > 0 ? (
          <ul aria-label="日付別の予約">
            {groupedReservations.map(({ key, date, reservations: dayReservations }) => (
              <li key={key}>
                <h2
                  data-testid="reservation-date-heading"
                  className="sticky z-[5] border-y border-border bg-muted/80 px-4 py-1.5 text-xs font-medium text-foreground backdrop-blur"
                  style={{
                    top: 'calc(var(--sticky-banners-height, 0px) + var(--page-header-height, 0px))',
                  }}
                >
                  {dateHeading(date)}
                  <span className="ml-2 font-normal text-muted-foreground">
                    {dayReservations.length} 件
                  </span>
                </h2>
                <ul>
                  {dayReservations.map((reservation) => (
                    <ReservationRow
                      key={`${reservation.site}:${reservation.programId}`}
                      reservation={reservation}
                      overages={overages}
                      ruleLabel={ruleLabel}
                    />
                  ))}
                </ul>
              </li>
            ))}
          </ul>
        ) : capacityUnavailable && search.only === 'attention' && ruleFilteredReservations.length > 0 ? null : (
          <EmptyState>
            {search.ruleId !== undefined && ruleFilteredReservations.length === 0
              ? 'このルールの予約はありません'
              : search.only === 'attention'
                ? '確認が要る予約はありません'
                : '予約がありません'}
          </EmptyState>
        )}
      </PageContent>
    </>
  )
}

function ReservationRow({
  reservation,
  overages,
  ruleLabel,
}: {
  reservation: Reservation
  overages: CapacityOverage[]
  ruleLabel: (ruleId: number) => string
}) {
  return (
    <li className="relative isolate flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/40">
      {/* 行全面リンクを背面へ置き、対話要素は個別に手前へ積む。 */}
      <Link
        to="/reservations/$site/$programId"
        params={{ site: reservation.site, programId: String(reservation.programId) }}
        aria-label={reservationRowLabel(reservation)}
        className="absolute inset-0"
      />
      <div className="w-[4.5rem] shrink-0 self-center text-sm">
        <span className="block">{formatTime(reservation.startAt)}</span>
        <span className="block text-xs text-muted-foreground">
          {formatDuration(reservation.durationMs)}
        </span>
      </div>
      <div className="min-w-0 flex-1">
        <div className="truncate text-base">{programTitle(reservation.title)}</div>
        <div
          data-testid="reservation-secondary"
          className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground"
        >
          <span className="shrink-0">{reservation.serviceName}</span>
          <ReservationOrigin reservation={reservation} ruleLabel={ruleLabel} />
          {/* 結論バッジは行全面リンクより手前で当たり判定を保つ。容量不足は
              CapacityShortfallBadge をそのまま使い、番組表への導線を維持する。 */}
          <ReservationVerdictBadge reservation={reservation} overages={overages} />
        </div>
      </div>
      <ChevronRight
        data-testid="reservation-chevron"
        className="size-4 shrink-0 text-muted-foreground"
      />
    </li>
  )
}

type ReservationDay = {
  key: string
  date: string
  reservations: Reservation[]
}

/** groupByLocalDay は時刻順の予約をローカル日付ごとにまとめる。 */
function groupByLocalDay(reservations: Reservation[]): ReservationDay[] {
  const groups = new Map<string, ReservationDay>()
  const chronological = [...reservations].sort(
    (a, b) => new Date(a.startAt).getTime() - new Date(b.startAt).getTime(),
  )
  for (const reservation of chronological) {
    const key = dayKey(reservation.startAt)
    const group = groups.get(key)
    if (group) group.reservations.push(reservation)
    else groups.set(key, { key, date: reservation.startAt, reservations: [reservation] })
  }
  return [...groups.values()]
}

/** dateHeading は今日・明日だけを見出しへ足し、残りは日付だけを返す。 */
function dateHeading(iso: string): string {
  const date = new Date(iso)
  const dayDifference = calendarDayDiff(date.getTime(), Date.now())
  const prefix = dayDifference === 0 ? '今日 ' : dayDifference === 1 ? '明日 ' : ''
  return `${prefix}${formatDate(iso)}`
}

/** rulesWithReservationCounts は、どの絞り込みより前の全予約でルールごとの件数を数える。 */
function rulesWithReservationCounts(
  reservations: Reservation[],
  ruleLabel: (ruleId: number) => string,
): { id: number; label: string; count: number }[] {
  const counts = new Map<number, number>()
  for (const reservation of reservations) {
    if (reservation.ruleId !== undefined) {
      counts.set(reservation.ruleId, (counts.get(reservation.ruleId) ?? 0) + 1)
    }
  }
  return [...counts]
    .map(([id, count]) => ({ id, label: ruleLabel(id), count }))
    .sort((a, b) => b.count - a.count || a.label.localeCompare(b.label, 'ja') || a.id - b.id)
}
