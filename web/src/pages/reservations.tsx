import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { useMemo } from 'react'

import {
  useListCapacityOverages,
  useListReservations,
  useListRules,
  type CapacityOverage,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { CapacityShortfallBadge } from '@/components/capacity-shortfall-badge'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { ReservationSkipBadge } from '@/components/reservation-skip-reason'
import { Chip } from '@/components/ui/chip'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { coveringWindow } from '@/lib/capacity'
import { dayKey, formatDate, formatDateTime, formatDuration, formatTime } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import {
  reservationNeedsAttention,
  stateLabels,
  type ReservationsPageSearch,
} from '@/lib/reservation-labels'
import { makeRuleLabel } from '@/lib/rule-label'
import { cn } from '@/lib/utils'

export function ReservationsPage() {
  const search = useRouteSearch({ from: '/reservations' })
  const navigate = useNavigate()
  const query = useListReservations()
  const rulesQuery = useListRules()
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
  // 下界」として要確認を出す --- state !== 'active' の分だけでも導線を消さない
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

  return (
    <>
      <PageHeader title="予約">
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
                        className="flex min-h-7 max-w-full shrink-0 items-center gap-1 rounded-full border border-border px-3 py-1.5 text-xs text-muted-foreground transition-colors outline-none hover:bg-muted hover:text-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
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
                    className="flex min-h-7 max-w-full shrink-0 items-center rounded-full border border-primary px-3 py-1.5 text-xs text-foreground transition-colors outline-none hover:bg-muted focus-visible:ring-3 focus-visible:ring-ring/50"
                  >
                    <span className="min-w-0 truncate">ルール「{ruleLabel(search.ruleId)}」 ×</span>
                  </button>
                  <Link
                    to="/search"
                    search={{ ruleId: search.ruleId }}
                    className="flex min-h-6 items-center rounded px-1 text-xs text-primary underline underline-offset-2"
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
        ) : displayedReservations.length > 0 ? (
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
  // 行本体のリンクは子要素を持たない絶対配置なので、children から組めない
  // accessible name を明示する。採否は行を一意に識別できる情報（タイトル・局・
  // 日時・尺・state）だけにする。毎日放送の番組は時刻だけでは同名の行が並ぶ。
  // 見た目の行は日付見出しの下にあるので日付を省くが、名前には日付を残す。
  // 出自や容量バッジの文言は混ぜない。
  const rowLabel = [
    programTitle(reservation.title),
    reservation.serviceName,
    formatDateTime(reservation.startAt),
    formatDuration(reservation.durationMs),
    reservation.state === 'active' ? null : stateLabels[reservation.state],
  ]
    // 空文字も落とす（`serviceName` は API required でも空文字を禁じていない）。
    .filter((part): part is string => part !== null && part !== '')
    .join(' ')

  return (
    <li className="relative isolate flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/40">
      {/* 行全面リンクを背面へ置き、対話要素は個別に手前へ積む。 */}
      <Link
        to="/reservations/$site/$programId"
        params={{ site: reservation.site, programId: String(reservation.programId) }}
        aria-label={rowLabel}
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
          <StateBadge state={reservation.state} />
          <ReservationSkipBadge reservation={reservation} />
          {/* CapacityShortfallBadge は番組表への独立した Link。自分の site の
              不足だけを見て、行全面リンクより手前で当たり判定を保つ。 */}
          <CapacityShortfallBadge
            overages={overages}
            site={reservation.site}
            startMs={new Date(reservation.startAt).getTime()}
            endMs={new Date(reservation.startAt).getTime() + reservation.durationMs}
          />
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
  const today = new Date()
  const todayStart = new Date(today.getFullYear(), today.getMonth(), today.getDate())
  const targetStart = new Date(date.getFullYear(), date.getMonth(), date.getDate())
  const tomorrowStart = new Date(todayStart)
  tomorrowStart.setDate(tomorrowStart.getDate() + 1)
  const prefix =
    dayKey(targetStart.toISOString()) === dayKey(todayStart.toISOString())
      ? '今日 '
      : dayKey(targetStart.toISOString()) === dayKey(tomorrowStart.toISOString())
        ? '明日 '
        : ''
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

/** ReservationOrigin は、ルールの編集先リンクを行全面リンクより手前に置く。 */
function ReservationOrigin({
  reservation,
  ruleLabel,
}: {
  reservation: Reservation
  ruleLabel: (ruleId: number) => string
}) {
  if (reservation.source === 'manual') return <span className="shrink-0">手動</span>
  // source は ruleId と独立した出自。ルールが現在の予約に base を供給して
  // いない場合は ruleId が無いこともあるので、手動と誤表示しない。
  if (reservation.ruleId === undefined) return <span className="shrink-0">ルール</span>
  return (
    <Link
      to="/search"
      search={{ ruleId: reservation.ruleId }}
      className="relative z-10 inline-flex min-h-6 items-center px-1 text-foreground underline underline-offset-2"
      aria-label={`ルール「${ruleLabel(reservation.ruleId)}」`}
    >
      ルール「{ruleLabel(reservation.ruleId)}」
    </Link>
  )
}

/**
 * StateBadge の `detached` の文字色は `text-foreground`（bg-muted 小バッジの
 * 合成後コントラスト対策。docs/frontend/design.md「コントラストは毎回測る」）。
 */
function StateBadge({ state }: { state: Reservation['state'] }) {
  if (state === 'active') return null
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        state === 'orphaned' ? 'bg-destructive/10 text-destructive' : 'bg-muted text-foreground',
      )}
    >
      {stateLabels[state]}
    </span>
  )
}
