import { Link } from '@tanstack/react-router'
import { TriangleAlert } from 'lucide-react'
import { useMemo } from 'react'

import {
  useListCapacityOverages,
  useListCircuitBreakers,
  useListRecordings,
  useListReservations,
  type CapacityOverage,
  type CircuitBreaker,
  type DropSummary,
  type Recording,
  type Reservation,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ListSkeleton, PageHeader } from '@/components/page'
import { ReservationSkipBadge } from '@/components/reservation-skip-reason'
import { describeBreakerName } from '@/lib/breaker'
import { shortageRangeMessage } from '@/lib/capacity'
import { dayOrigin } from '@/lib/day-offset'
import { formatDateTime, formatDuration } from '@/lib/format'
import { cn } from '@/lib/utils'
import { DropBadges, StatusBadge } from '@/pages/recordings'

/**
 * RESERVATION_LIMIT / RECENT_FINISHED_LIMIT は「1 セクションが画面を占有しない」
 * ための恣意的な上限（実測ではない。issue #242 着手宣言コメント）。窓の方
 * （今夜〜明日の予約の時間窓）は既存の日境界（`lib/day-offset.ts`）に揃えたという
 * 根拠があるが、この 2 つの件数はそうではない。
 */
const RESERVATION_LIMIT = 10
const RECENT_FINISHED_LIMIT = 6

/**
 * HomePage はホーム（`/`。M8-3, issue #242）。
 *
 * 起動して最初に見えるのが番組表（「これから録るもの」）だと、運用が安定した
 * 録画サーバーへの再訪の大半が知りたいこと（録れているか・今夜なにが録れるか・
 * 見るものはあるか・異常はないか）に 1 画面で答えられない。部品（録画中の状態・
 * 予約一覧・サーキットブレーカーバナー・容量バッジ・ドロップ統計）は既存のまま、
 * ここでは**新しい API を作らず**それらを集約するだけにする（issue #242 の
 * 着手宣言コメントの決定）。
 *
 * セクションは 4 つ: いま録画中 / 今夜〜明日の予約 / 警告 / 直近の完了。
 * **0 件のセクションは文言も出さずセクションごと消し、全セクションが空のときだけ
 * ホーム全体で 1 つの空状態を出す**（一覧画面の「条件に合う録画がありません」の
 * ような「探した結果の報告」とは意味が違う --- ホームの空は「何も主張しない」
 * ことそのものなので、肯定的な文言（「異常なし」）に転ばないよう沈黙を選ぶ）。
 *
 * データが出揃うまではセクションの出し分けを判断しない（全クエリの解決を待って
 * から可視性を決める）。**個別のクエリが解決する前に「空だから隠す」を判定すると、
 * 読み込み中の一瞬を「セクションが無い」と誤読する**（CLAUDE.md「非同期の空虚な
 * 成功」）。取得が失敗した場合は空扱いにせず、そのセクションだけ取得失敗を表示する
 * （空白のセクションを「異常なし」と取り違えさせないため。ただし警告セクションの
 * 材料であるサーキットブレーカー・容量超過は、他の画面（`CircuitBreakerBanner` /
 * 予約一覧の容量バッジ）と同じ「取得失敗は警告が無いことにする」流儀に揃える ---
 * `docs/data.md` §6.5 が言う「既知の盲点は警告を見逃す方向に偏っている」を
 * 承知のうえで、既存の踏襲先が同じ判断をしている）。
 */
export function HomePage() {
  // nowMs はこのレンダーの間で一貫させる（`pages/programs.tsx` と同じ規律。
  // 起点・上限を別々に Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が
  // 混ざりうる）。
  const nowMs = Date.now()
  // 今夜〜明日の予約セクションの窓の終端。「明日の暦日の終わり」= 明後日の 0 時
  // （`lib/day-offset.ts` の `dayOrigin` が返す「dayOffset 日先の 0 時」を
  // dayOffset=2 で呼ぶと明後日の 0 時になり、これが明日の終わりと一致する。
  // 番組表の日境界（0 時基準）に揃えた窓であり、根拠はここだけ実測ではなく
  // 既存の日境界との整合）。
  const reservationsWindowEndMs = dayOrigin(2, nowMs).getTime()

  const recordingQuery = useListRecordings({ status: 'recording' })
  const finishedQuery = useListRecordings({
    status: 'finished',
    limit: RECENT_FINISHED_LIMIT,
  })
  const reservationsQuery = useListReservations()
  const breakersQuery = useListCircuitBreakers()
  const overagesQuery = useListCapacityOverages({
    start: new Date(nowMs).toISOString(),
    end: new Date(reservationsWindowEndMs).toISOString(),
  })

  // 全クエリが解決する（成功 or 失敗）まではスケルトンのみ。どれか 1 つでも
  // 未解決のうちにセクションの出し分けを決めると、まだ来ていないデータを
  // 「0 件だから隠す」と誤読しかねない。
  const settled =
    !recordingQuery.isPending &&
    !finishedQuery.isPending &&
    !reservationsQuery.isPending &&
    !breakersQuery.isPending &&
    !overagesQuery.isPending

  const recordingsInProgress = unwrap(recordingQuery.data) ?? []
  // `useMemo` で安定させる --- `unwrap(...) ?? []` は解決前/失敗時に毎回新しい
  // 配列を作るため、これを他の useMemo（`warnings`）の依存に生で渡すと
  // 「依存が毎回変わる」形になり oxlint の exhaustive-deps 警告になる
  // （CLAUDE.md「既存 3 件の oxlint warning は増やさない」）。
  const recentFinished = useMemo(() => unwrap(finishedQuery.data) ?? [], [finishedQuery.data])

  const upcomingReservations = useMemo(() => {
    const all = unwrap(reservationsQuery.data) ?? []
    return all
      .filter((r) => {
        const startMs = new Date(r.startAt).getTime()
        return startMs >= nowMs && startMs < reservationsWindowEndMs
      })
      .sort((a, b) => new Date(a.startAt).getTime() - new Date(b.startAt).getTime())
  }, [reservationsQuery.data, nowMs, reservationsWindowEndMs])
  const reservationsOverflow = upcomingReservations.length > RESERVATION_LIMIT
  const shownReservations = upcomingReservations.slice(0, RESERVATION_LIMIT)

  const warnings = useMemo(
    () =>
      buildWarnings({
        breakers: unwrap(breakersQuery.data) ?? [],
        overages: unwrap(overagesQuery.data) ?? [],
        recentFinished,
      }),
    [breakersQuery.data, overagesQuery.data, recentFinished],
  )

  if (!settled) {
    return (
      <>
        <PageHeader title="ホーム" />
        <ListSkeleton />
      </>
    )
  }

  const recordingSectionVisible = recordingQuery.isError || recordingsInProgress.length > 0
  const reservationSectionVisible = reservationsQuery.isError || shownReservations.length > 0
  const warningSectionVisible = warnings.length > 0
  const finishedSectionVisible = finishedQuery.isError || recentFinished.length > 0

  const allEmpty =
    !recordingSectionVisible &&
    !reservationSectionVisible &&
    !warningSectionVisible &&
    !finishedSectionVisible

  return (
    <>
      <PageHeader title="ホーム" />

      {allEmpty ? (
        // 「異常なし」「予約がありません」のような肯定/報告の文言にしない ---
        // これは検索結果の 0 件ではなく、4 セクションすべてが沈黙した結果の
        // 集約なので、何も主張しない言い方に留める。
        <EmptyState>表示できる項目がありません</EmptyState>
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {recordingSectionVisible && (
            <section aria-labelledby="home-recording">
              <h2 id="home-recording" className="px-4 pt-4 pb-2 text-sm font-semibold">
                いま録画中
              </h2>
              {recordingQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">録画中の取得に失敗しました</p>
              ) : (
                <ul>
                  {recordingsInProgress.map((r) => (
                    <RecordingRow key={r.id} recording={r} />
                  ))}
                </ul>
              )}
            </section>
          )}

          {reservationSectionVisible && (
            <section aria-labelledby="home-reservations">
              <h2 id="home-reservations" className="px-4 pt-4 pb-2 text-sm font-semibold">
                今夜〜明日の予約
              </h2>
              {reservationsQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">予約の取得に失敗しました</p>
              ) : (
                <>
                  <ul>
                    {shownReservations.map((r) => (
                      <ReservationRow key={r.id} reservation={r} />
                    ))}
                  </ul>
                  {reservationsOverflow && (
                    <div className="px-4 pb-4">
                      <Link
                        to="/reservations"
                        className="text-sm text-primary underline-offset-2 hover:underline"
                      >
                        予約をすべて見る
                      </Link>
                    </div>
                  )}
                </>
              )}
            </section>
          )}

          {warningSectionVisible && (
            <section aria-labelledby="home-warnings">
              <h2 id="home-warnings" className="px-4 pt-4 pb-2 text-sm font-semibold">
                警告
              </h2>
              <ul className="flex flex-col gap-2 px-4 pb-4">
                {warnings.map((w) => (
                  <WarningRow key={w.key} warning={w} />
                ))}
              </ul>
            </section>
          )}

          {finishedSectionVisible && (
            <section aria-labelledby="home-finished">
              <h2 id="home-finished" className="px-4 pt-4 pb-2 text-sm font-semibold">
                直近の完了
              </h2>
              {finishedQuery.isError ? (
                <p className="px-4 pb-4 text-sm text-destructive">
                  直近の完了録画の取得に失敗しました
                </p>
              ) : (
                <ul>
                  {recentFinished.map((r) => (
                    <RecordingRow key={r.id} recording={r} />
                  ))}
                </ul>
              )}
            </section>
          )}
        </div>
      )}
    </>
  )
}

/** RecordingRow はホームの「いま録画中」「直近の完了」セクションの 1 行。 */
function RecordingRow({ recording }: { recording: Recording }) {
  return (
    <li className="border-b border-border last:border-b-0">
      <Link
        to="/recordings/$id"
        params={{ id: String(recording.id) }}
        className="flex min-h-14 flex-col justify-center gap-0.5 px-4 py-2.5 hover:bg-muted/50"
      >
        <span className="truncate text-sm">{recording.title || '（番組名なし）'}</span>
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          <StatusBadge status={recording.status} />
          <span className="shrink-0">{recording.serviceName}</span>
          <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
          <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
          {recording.dropSummary && <DropBadges summary={recording.dropSummary} />}
        </span>
      </Link>
    </li>
  )
}

/**
 * ReservationRow はホームの「今夜〜明日の予約」セクションの 1 行。
 *
 * 予約一覧（`pages/reservations.tsx`）と違い、容量不足バッジは行に重ねない ---
 * 同じ超過区間は本ページの「警告」セクションで既に一覧化しているので、二重に
 * 主張しない。行自体は 2 つ目の対話要素を持たないので、`pages/reservations.tsx`
 * のような「行本体を絶対配置の面にする」入れ子回避の手当ても不要 --- `<li>` を
 * そのまま 1 つの `Link` にできる。
 */
function ReservationRow({ reservation }: { reservation: Reservation }) {
  return (
    <li className="border-b border-border last:border-b-0">
      <Link
        to="/reservations/$site/$programId"
        params={{ site: reservation.site, programId: String(reservation.programId) }}
        className="flex min-h-14 flex-col justify-center gap-0.5 px-4 py-2.5 hover:bg-muted/50"
      >
        <span className="truncate text-sm">{reservation.title || '（番組名なし）'}</span>
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          <span className="shrink-0">{formatDateTime(reservation.startAt)}</span>
          <span className="shrink-0">{formatDuration(reservation.durationMs)}</span>
          <ReservationSkipBadge reservation={reservation} />
        </span>
      </Link>
    </li>
  )
}

/** WarningItem はホーム「警告」セクションの 1 件（サーキットブレーカー / チューナー不足 / ドロップ）。 */
type WarningItem = {
  key: string
  message: string
  /** 遷移先。サーキットブレーカーは対応する専用画面が無い（`CircuitBreakerBanner` が同じページの上部で扱う）ので省略。 */
  link?: { to: '/programs'; search: { at: number } } | { to: '/recordings/$id'; id: number }
}

/**
 * buildWarnings はサーキットブレーカー・容量超過・直近の完了録画のドロップ統計から
 * ホームの「警告」セクションの項目を組む（issue #242 着手宣言コメントの決定：
 * 新しい API を作らず、既存 3 種の取得結果だけを材料にする）。
 *
 * 容量超過は `overages` に渡された時点で問い合わせ窓（今夜〜明日の予約と同じ窓）
 * で既に絞られているので、ここで追加の時間フィルタはしない --- サーバーが
 * 返した区間はすべて「その窓に関係する超過」である。
 */
function buildWarnings({
  breakers,
  overages,
  recentFinished,
}: {
  breakers: readonly CircuitBreaker[]
  overages: readonly CapacityOverage[]
  recentFinished: readonly Recording[]
}): WarningItem[] {
  const items: WarningItem[] = []

  for (const breaker of breakers) {
    items.push({
      key: `breaker:${breaker.site}:${breaker.name}`,
      message: `${describeBreakerName(breaker.name)}が停止中（保留 ${breaker.pending} 件）`,
    })
  }

  for (const overage of overages) {
    items.push({
      key: `overage:${overage.site}:${overage.startAt}:${overage.endAt}`,
      message: shortageRangeMessage(overage),
      link: { to: '/programs', search: { at: new Date(overage.startAt).getTime() } },
    })
  }

  for (const recording of recentFinished) {
    const summary = recording.dropSummary
    if (summary === undefined) continue
    if (summary.drops === 0 && summary.errors === 0 && summary.scrambled === 0) continue
    items.push({
      key: `drop:${recording.id}`,
      message: `${recording.title || '（番組名なし）'}: ${dropSummaryText(summary)}`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  return items
}

/** dropSummaryText は 0 でない内訳だけを短く並べる（`DropBadges` と同じ「0 は出さない」流儀）。 */
function dropSummaryText(summary: DropSummary): string {
  return [
    { label: 'drop', value: summary.drops },
    { label: 'error', value: summary.errors },
    { label: 'scrambled', value: summary.scrambled },
  ]
    .filter((b) => b.value > 0)
    .map((b) => `${b.label} ${b.value.toLocaleString()}`)
    .join(' / ')
}

/**
 * WarningRow は 1 件の警告。サーキットブレーカーと直近のドロップは「取り返しが
 * つかない/止まっている」意味の destructive、チューナー不足は容量バッジ
 * （`components/capacity-shortfall-badge.tsx`）と同じ warning（琥珀）に揃える
 * （docs/frontend/design.md「色は信号のみ」。同じ事実は同じ色で言う）。
 */
function WarningRow({ warning }: { warning: WarningItem }) {
  const amber = warning.key.startsWith('overage:')
  const content = (
    <>
      <TriangleAlert className="size-4 shrink-0" aria-hidden="true" />
      <span>{warning.message}</span>
    </>
  )
  const rowClassName = cn(
    'flex items-center gap-2 rounded-md px-2 py-1.5 text-sm',
    amber ? 'bg-warning/10 text-warning' : 'text-destructive',
  )

  if (warning.link === undefined) {
    return <li className={rowClassName}>{content}</li>
  }

  if (warning.link.to === '/programs') {
    return (
      <li>
        <Link
          to="/programs"
          search={warning.link.search}
          className={cn(rowClassName, 'hover:underline')}
        >
          {content}
        </Link>
      </li>
    )
  }

  return (
    <li>
      <Link
        to="/recordings/$id"
        params={{ id: String(warning.link.id) }}
        className={cn(rowClassName, 'hover:underline')}
      >
        {content}
      </Link>
    </li>
  )
}
