import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronRight, TriangleAlert } from 'lucide-react'
import { useId, useLayoutEffect, useRef, useState } from 'react'

import type { CapacityOverage, RecordingShelf, Reservation } from '@/api/generated'
import { ReservationContextMenu } from '@/components/reservation-context-menu'
import { RecordingThumbnail } from '@/components/recording-thumbnail'
import { ReservationOrigin, ReservationVerdictBadge } from '@/components/reservation-row-parts'
import { formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { reservationGroupCapacityAt, type ReservationGroup } from '@/lib/reservation-groups'
import { shortageMessage } from '@/lib/capacity'
import { reservationRowLabel, unwatchedLabel } from '@/lib/reservation-labels'
import { useMediaQuery } from '@/lib/use-media-query'
import { cn } from '@/lib/utils'

/** 予約の詳細リンクを展開する行。棚は予約一覧とは独立したクエリの成功時だけ渡す。 */
export function ReservationSeriesRow({
  group,
  overages,
  shelvesKnown,
  shelf,
  ruleLabel,
  showSite,
  onCancelReservation,
  isCancelPending,
}: {
  group: ReservationGroup
  overages: readonly CapacityOverage[]
  shelvesKnown: boolean
  shelf?: RecordingShelf
  ruleLabel: (ruleId: number) => string
  onCancelReservation: (reservation: Reservation) => void
  isCancelPending: (reservation: Reservation) => boolean
  /** 複数サイトを構成しているか。構成ベースで決め、行の予約の site 数では決めない。 */
  showSite: boolean
}) {
  const finePointer = useMediaQuery('(pointer: fine)')
  const [expanded, setExpanded] = useState(false)
  const headingRef = useRef<HTMLSpanElement>(null)
  const headerRef = useRef<HTMLDivElement>(null)
  const detailsId = `reservation-series-details-${useId().replaceAll(':', '')}`
  const canExpand = group.reservations.length > 1

  // 行を開いたとき見出しが sticky なページヘッダーの下に隠れていたら、見出しを
  // ヘッダーの下へ戻す。隠れる量は scroll-margin-top（時間順の日付見出しの top と同じ
  // CSS 変数）が持つ。高さの変化後も「何を開いたか」を見失わないようにする。
  useLayoutEffect(() => {
    if (!expanded) return
    headingRef.current?.scrollIntoView({ block: 'nearest' })
  }, [expanded])

  const title = group.title
  const next = group.next
  const summary = `今後 ${group.reservations.length.toLocaleString('ja-JP')} 本`
  const shelfKnownForSeries = shelvesKnown && group.series !== null

  return (
    <li
      data-testid="reservation-series-row"
      data-series-value={group.series ?? undefined}
      className="border-b border-border"
    >
      <ReservationContextMenu
        reservation={next}
        rowRef={headerRef}
        onCancel={() => onCancelReservation(next)}
        cancelPending={isCancelPending(next)}
      >
        <div
          ref={headerRef}
          data-testid="reservation-series-header"
          tabIndex={finePointer ? -1 : undefined}
          className="relative isolate"
        >
          {canExpand ? (
            <button
              type="button"
              aria-label={`${title}の予約を${expanded ? '閉じる' : '開く'}`}
              aria-expanded={expanded}
              aria-controls={detailsId}
              onClick={() => setExpanded((value) => !value)}
              className="absolute inset-0"
            />
          ) : (
            <Link
              to="/reservations/$site/$programId"
              params={{ site: next.site, programId: String(next.programId) }}
              aria-label={reservationRowLabel(next)}
              className="absolute inset-0"
            />
          )}
          <div className="flex min-w-0 items-center gap-3 px-4 py-2.5">
            <div className="min-w-0 flex-1">
              <span
                ref={headingRef}
                data-testid="reservation-series-title"
                className="block truncate text-base font-medium"
                style={{
                  scrollMarginTop: 'calc(var(--sticky-banners-height, 0px) + var(--page-header-height, 0px))',
                }}
              >
                {title}
              </span>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
                <span className="shrink-0">{formatDateTime(next.startAt)}</span>
                <span className="shrink-0">{next.serviceName}</span>
                <span className="shrink-0">{formatDuration(next.durationMs)}</span>
                <ReservationOrigin reservation={next} ruleLabel={ruleLabel} />
                <span data-testid="reservation-series-count" className="shrink-0">
                  {summary}
                </span>
                <ReservationGroupBadges group={group} />
                {shelfKnownForSeries && shelf !== undefined && (
                  <MobileShelfSummary series={group.series!} shelf={shelf} />
                )}
                {shelfKnownForSeries && shelf === undefined && (
                  <span className="shrink-0 lg:hidden">まだ録画なし</span>
                )}
              </div>
            </div>

            {shelfKnownForSeries && <DesktopShelfBox series={group.series!} shelf={shelf} />}
            {/* 開閉できる行は ∨（開くと ∧）、遷移する行は ›。形で両者を見分けさせる。 */}
            {canExpand ? (
              <ChevronDown
                aria-hidden
                className={cn('size-4 shrink-0 text-muted-foreground', expanded && 'rotate-180')}
              />
            ) : (
              <ChevronRight aria-hidden className="size-4 shrink-0 text-muted-foreground" />
            )}
          </div>
        </div>
      </ReservationContextMenu>

      {canExpand && expanded && (
        <ul id={detailsId} aria-label={`${title}の予約`} className="border-t border-border bg-muted/20">
          {group.reservations.map((reservation) => (
            <ReservationEpisodeRow
              key={`${reservation.site}:${reservation.programId}`}
              reservation={reservation}
              series={group.series}
              overages={overages}
              showSite={showSite}
              ruleLabel={ruleLabel}
              onCancelReservation={onCancelReservation}
              isCancelPending={isCancelPending}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

function MobileShelfSummary({ series, shelf }: { series: string; shelf: RecordingShelf }) {
  const unwatched = unwatchedLabel(shelf.unwatchedCount)
  return (
    <Link
      to="/recordings/$id/series"
      params={{ id: String(shelf.representativeId) }}
      aria-label={`${series}の番組ハブ。録画 ${shelf.count} 本、${unwatched}`}
      className="relative z-10 inline-flex min-h-6 pointer-coarse:min-h-11 items-center rounded px-1 text-foreground underline underline-offset-2 lg:hidden"
    >
      録画 {shelf.count.toLocaleString('ja-JP')} 本 · {unwatched} ›
    </Link>
  )
}

function DesktopShelfBox({ series, shelf }: { series: string; shelf?: RecordingShelf }) {
  if (shelf === undefined) {
    return (
      <div
        data-testid="reservation-recording-shelf-empty"
        className="hidden w-56 shrink-0 items-center gap-3 rounded-md border border-dashed border-border px-3 py-2 text-sm text-muted-foreground lg:flex"
      >
        <span aria-hidden className="aspect-video w-20 shrink-0 rounded border border-dashed border-border bg-muted/40" />
        <span>まだ録画なし</span>
      </div>
    )
  }

  const unwatched = unwatchedLabel(shelf.unwatchedCount)
  return (
    <ShelfLink series={series} shelf={shelf} className="hidden lg:flex">
      <RecordingThumbnail
        key={shelf.representativeId}
        recordingId={shelf.representativeId}
        className="w-20"
      />
      <span className="min-w-0">
        <span className="block truncate text-sm font-medium">録画 {shelf.count.toLocaleString('ja-JP')} 本</span>
        <span className="block truncate text-xs text-muted-foreground">{unwatched}</span>
      </span>
    </ShelfLink>
  )
}

function ShelfLink({
  series,
  shelf,
  className,
  children,
}: {
  series: string
  shelf: RecordingShelf
  className: string
  children: React.ReactNode
}) {
  const unwatched = unwatchedLabel(shelf.unwatchedCount)
  return (
    <Link
      to="/recordings/$id/series"
      params={{ id: String(shelf.representativeId) }}
      aria-label={`${series}の番組ハブ。録画 ${shelf.count} 本、${unwatched}`}
      className={cn(
        'relative z-10 min-h-16 w-56 shrink-0 items-center gap-3 rounded-md border border-border bg-card p-2 text-left hover:bg-muted/60',
        className,
      )}
    >
      {children}
    </Link>
  )
}

function ReservationEpisodeRow({
  reservation,
  series,
  overages,
  showSite,
  ruleLabel,
  onCancelReservation,
  isCancelPending,
}: {
  reservation: Reservation
  series: string | null
  overages: readonly CapacityOverage[]
  showSite: boolean
  ruleLabel: (ruleId: number) => string
  onCancelReservation: (reservation: Reservation) => void
  isCancelPending: (reservation: Reservation) => boolean
}) {
  const finePointer = useMediaQuery('(pointer: fine)')
  const title = episodeTitle(reservation.title, series)
  const rowRef = useRef<HTMLLIElement>(null)

  const row = (
    <li
      ref={rowRef}
      data-testid="reservation-episode-row"
      className="relative isolate border-b border-border/70 last:border-b-0"
      tabIndex={finePointer ? -1 : undefined}
    >
      <Link
        to="/reservations/$site/$programId"
        params={{ site: reservation.site, programId: String(reservation.programId) }}
        aria-label={reservationRowLabel(reservation)}
        className="absolute inset-0"
      />
      <div className="flex min-h-11 min-w-0 flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 pl-7">
        <span className="shrink-0 text-sm">{formatDateTime(reservation.startAt)}</span>
        <span className="min-w-0 flex-1 truncate text-sm">{title}</span>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          <span className="shrink-0">{formatDuration(reservation.durationMs)}</span>
          {showSite && <span className="shrink-0 rounded bg-muted px-1.5 py-0.5">{reservation.site}</span>}
          <ReservationOrigin reservation={reservation} ruleLabel={ruleLabel} />
          <ReservationVerdictBadge reservation={reservation} overages={overages} />
        </div>
      </div>
    </li>
  )

  return (
    <ReservationContextMenu
      reservation={reservation}
      rowRef={rowRef}
      onCancel={() => onCancelReservation(reservation)}
      cancelPending={isCancelPending(reservation)}
    >
      {row}
    </ReservationContextMenu>
  )
}

function episodeTitle(title: string, series: string | null): string {
  const displayTitle = programTitle(title)
  if (series === null) return displayTitle
  const index = displayTitle.indexOf(series)
  if (index < 0) return displayTitle
  const remainder = `${displayTitle.slice(0, index)}${displayTitle.slice(index + series.length)}`
    .replace(/^[\s:：・|｜\-–—]+/, '')
    .replace(/[\s:：・|｜\-–—]+$/, '')
    .trim()
  return remainder || displayTitle
}

function ReservationGroupBadges({ group }: { group: ReservationGroup }) {
  const { badges } = group
  const capacityAt = reservationGroupCapacityAt(group)
  const capacityOverage = group.capacityTarget
  return (
    <>
      {badges.notRecorded > 0 && (
        <span className="shrink-0 rounded bg-destructive/10 px-1.5 py-0.5 text-xs text-destructive">
          録画されず {badges.notRecorded}
        </span>
      )}
      {badges.skipDuplicate > 0 && (
        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
          録画しない（重複） {badges.skipDuplicate}
        </span>
      )}
      {badges.skipExcluded > 0 && (
        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
          録画しない（除外） {badges.skipExcluded}
        </span>
      )}
      {badges.capacityShortfall > 0 && capacityAt !== undefined && capacityOverage !== undefined && (
        <Link
          to="/programs"
          search={{ view: 'grid', at: capacityAt }}
          aria-label={`${shortageMessage(capacityOverage)}。該当する予約 ${badges.capacityShortfall} 件`}
          className="relative z-10 inline-flex min-h-6 pointer-coarse:min-h-11 items-center gap-1 rounded bg-warning/10 px-1.5 py-0.5 text-xs text-warning hover:bg-warning/20 focus-visible:outline-2 focus-visible:outline-warning before:absolute before:inset-x-0 before:top-1/2 before:h-8 pointer-coarse:before:h-11 before:-translate-y-1/2"
        >
          <TriangleAlert className="size-3 shrink-0" aria-hidden />
          容量不足 {badges.capacityShortfall}
        </Link>
      )}
    </>
  )
}
