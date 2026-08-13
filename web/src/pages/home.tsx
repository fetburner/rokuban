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

/**
 * RESERVATION_LIMIT / RECENT_FINISHED_LIMIT は「1 セクションが画面を占有しない」
 * ための恣意的な上限（実測ではない。issue #242 着手宣言コメント）。窓の方
 * （今夜〜明日の予約の時間窓）は既存の日境界（`lib/day-offset.ts`）に揃えたという
 * 根拠があるが、この 2 つの件数はそうではない。
 */
const RESERVATION_LIMIT = 10
const RECENT_FINISHED_LIMIT = 6

/**
 * DROP_WARNING_SCAN_LIMIT はドロップ警告の材料を取る範囲。**「直近の完了」の
 * 表示件数（`RECENT_FINISHED_LIMIT`）とは意図的に独立させてある。**
 *
 * レビュー指摘: 以前は警告のドロップ検出を表示用の `recentFinished`
 * （`RECENT_FINISHED_LIMIT` 件）にそのまま乗せていたため、表示上限をレイアウト
 * の都合で下げると警告の遡り幅まで黙って縮み、それを検知するテストも無かった。
 * 表示（何行見せるか = レイアウトの都合）と検出（どこまで遡って異常を拾うか =
 * 正しさの都合）は別の関心事なので、別クエリ（`dropScanQuery`）に分離した。
 * この値も上と同じく恣意的な上限（実測ではない）だが、少なくとも表示件数を
 * 変えても警告の遡り幅は変わらない。
 */
const DROP_WARNING_SCAN_LIMIT = 20

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
 * **セクションごとの可視性はそのセクション自身のクエリの解決だけを待つ。**
 * 「全セクションが空」（`allEmpty`）の判定だけが全クエリの解決を待つ ---
 * 6 本のうち最も遅い 1 本（絞り込みを持たない `GET /api/reservations` など）に
 * 「いま録画中」のような最も見たいセクションまで引きずられて隠れる半径を
 * 小さくするため（レビュー指摘）。一方で「まだ解決していないセクションを
 * 0 件として隠す」ことはしない --- 個別のクエリが解決する前に「空だから隠す」を
 * 判定すると、読み込み中の一瞬を「セクションが無い」と誤読する（CLAUDE.md
 * 「非同期の空虚な成功」）。未解決のセクションは「解決するまで存在を主張しない」
 * （消えているのではなく、まだ何も言っていない）。
 *
 * 取得が失敗した場合は空扱いにせず、そのセクションだけ取得失敗を表示する
 * （空白のセクションを「異常なし」と取り違えさせないため）。ただし警告
 * セクションの材料であるサーキットブレーカー・容量超過・ドロップ検出用の
 * 録画一覧は、他の画面（`CircuitBreakerBanner` / 予約一覧の容量バッジ）と
 * 同じ「取得失敗は警告が無いことにする」流儀に揃える --- `docs/data.md` §6.5
 * が言う「既知の盲点は警告を見逃す方向に偏っている」を承知のうえで、既存の
 * 踏襲先が同じ判断をしている。
 */
export function HomePage() {
  // nowMs はこのレンダーの間で一貫させる（`pages/programs.tsx` と同じ規律。
  // 起点・上限を別々に Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が
  // 混ざりうる）。
  const nowMs = Date.now()

  // **容量超過クエリの `start` は生の `nowMs` を渡さない。** レンダーごとの
  // 生ミリ秒をクエリのパラメータ（延いては TanStack Query のキャッシュキー）に
  // 直接載せると、レンダーのたびに新しいキーになり「未解決 → 即解決 → 再描画 →
  // また未解決」が閉じない無限再取得になる（レビューで実測: 4 秒で 37 回、
  // 実サーバー相当の遅延では 4 秒間ずっと全画面スケルトンのまま収束しなかった）。
  //
  // **既存 2 ファイルの前例に倣い、量子化してからキーに渡す**（`useRef`/`useState`
  // で「now を固定する」ような対症療法は採らない --- それは症状を消すだけで
  // 「キーに入る値は答えが変わる粒度まで量子化する」という規律の欠落が残る）。
  // `pages/programs.tsx` は `Date.now()` を `dayOrigin(0, ...)` で時境界へ量子化
  // してからキーに渡している（同ファイルの `dayOrigin` の doc コメント参照。
  // 「今日」の起点を「now を時で切り捨てた時刻」にしているのはこの目的も兼ねる）。
  // ここでも同じ関数で「今」を時境界へ丸めてから `start` に渡す。
  const overagesStartMs = dayOrigin(0, nowMs).getTime()
  // 今夜〜明日の予約セクションの窓の終端。「明日の暦日の終わり」= 明後日の 0 時
  // （`dayOrigin` が返す「dayOffset 日先の 0 時」を dayOffset=2 で呼ぶと明後日の
  // 0 時になり、これが明日の終わりと一致する。番組表の日境界（0 時基準）に
  // 揃えた窓であり、根拠はここだけ実測ではなく既存の日境界との整合）。
  // こちらは元から日単位に量子化済みなので上記の無限再取得は起きない。
  const reservationsWindowEndMs = dayOrigin(2, nowMs).getTime()

  const recordingQuery = useListRecordings({ status: 'recording' })
  const finishedQuery = useListRecordings({
    status: 'finished',
    limit: RECENT_FINISHED_LIMIT,
  })
  // ドロップ警告専用。`finishedQuery`（表示用、上限 6）とは独立に、より広い
  // 範囲（上限 20）を取る（`DROP_WARNING_SCAN_LIMIT` 参照）。
  const dropScanQuery = useListRecordings({
    status: 'finished',
    limit: DROP_WARNING_SCAN_LIMIT,
  })
  const reservationsQuery = useListReservations()
  const breakersQuery = useListCircuitBreakers()
  const overagesQuery = useListCapacityOverages({
    start: new Date(overagesStartMs).toISOString(),
    end: new Date(reservationsWindowEndMs).toISOString(),
  })

  const recordingsInProgress = unwrap(recordingQuery.data) ?? []
  // `useMemo` で安定させる --- `unwrap(...) ?? []` は解決前/失敗時に毎回新しい
  // 配列を作るため、これを他の useMemo（`warnings`）の依存に生で渡すと
  // 「依存が毎回変わる」形になり oxlint の exhaustive-deps 警告になる
  // （CLAUDE.md「既存 3 件の oxlint warning は増やさない」）。
  const recentFinished = useMemo(() => unwrap(finishedQuery.data) ?? [], [finishedQuery.data])
  const dropScanRecordings = useMemo(
    () => unwrap(dropScanQuery.data) ?? [],
    [dropScanQuery.data],
  )

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

  // `overagesStartMs` を時境界へ丸めた分だけ、実際の「今」より前に始まった
  // （が、その時点ではまだ終わっていなかった）区間まで返ってきうる。ここで
  // 「実際の今より後に終わる」区間だけへ絞り、量子化前と同じ主張の強さに戻す
  // （量子化はキャッシュキーの安定のためだけの手段で、表示する内容の正しさを
  // 緩めてよい理由にはしない）。
  const activeOverages = useMemo(() => {
    const all = unwrap(overagesQuery.data) ?? []
    return all.filter((o) => new Date(o.endAt).getTime() > nowMs)
  }, [overagesQuery.data, nowMs])

  const warnings = useMemo(
    () =>
      buildWarnings({
        breakers: unwrap(breakersQuery.data) ?? [],
        overages: activeOverages,
        dropCandidates: dropScanRecordings,
      }),
    [breakersQuery.data, activeOverages, dropScanRecordings],
  )

  // セクションごとの可視性はそのセクション自身のクエリの解決だけを待つ
  // （上記 doc コメント参照）。警告は 3 本のクエリの合成なので、そのいずれかが
  // 未解決なら「まだ言わない」。
  const recordingSectionVisible =
    !recordingQuery.isPending && (recordingQuery.isError || recordingsInProgress.length > 0)
  const reservationSectionVisible =
    !reservationsQuery.isPending && (reservationsQuery.isError || shownReservations.length > 0)
  const warningsPending =
    breakersQuery.isPending || overagesQuery.isPending || dropScanQuery.isPending
  const warningSectionVisible = !warningsPending && warnings.length > 0
  const finishedSectionVisible =
    !finishedQuery.isPending && (finishedQuery.isError || recentFinished.length > 0)

  const anyVisible =
    recordingSectionVisible ||
    reservationSectionVisible ||
    warningSectionVisible ||
    finishedSectionVisible

  // 「全セクションが空」の判定だけは全クエリの解決を待つ ---
  // 一部がまだ未解決のうちは「空である」とまだ言い切れない。
  const allSettled =
    !recordingQuery.isPending &&
    !reservationsQuery.isPending &&
    !warningsPending &&
    !finishedQuery.isPending

  // どのセクションもまだ何も言えることが無い（1 つも解決していない、または
  // 解決済みの分がすべて 0 件）間はスケルトンを出す。何か 1 つでも言えることが
  // 出てくれば、他がまだ解決していなくても表示を始める（最も遅いクエリに
  // 引きずられない）。
  if (!anyVisible && !allSettled) {
    return (
      <>
        <PageHeader title="ホーム" />
        <ListSkeleton />
      </>
    )
  }

  const allEmpty = allSettled && !anyVisible

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

/**
 * RecordingRow はホームの「いま録画中」「直近の完了」セクションの 1 行。
 *
 * **警告に値する事実（ドロップ統計）は行に重ねない。** どちらのセクションも
 * 録画は `status` で既に絞り込まれている（前者は `recording`、後者は
 * `finished`）ため状態バッジ自体が見出しと重複するので出さない。ドロップの
 * 有無は「警告」セクションが該当録画へのリンク付きで一覧化する唯一の場所
 * （`ReservationRow` の doc コメントと同じ「二重に主張しない」規律。以前は
 * ここにも `DropBadges` を出しており、同じ事実が同一画面に 2 回出ていた ---
 * レビュー指摘で統一した）。行の役目はタイトル・時刻等の識別に絞り、詳細は
 * 遷移先（録画単体ページ）か警告セクションに譲る。
 */
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
          <span className="shrink-0">{recording.serviceName}</span>
          <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
          <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
        </span>
      </Link>
    </li>
  )
}

/**
 * ReservationRow はホームの「今夜〜明日の予約」セクションの 1 行。
 *
 * **警告に値する事実（容量不足）は行に重ねない。** 同じ超過区間は本ページの
 * 「警告」セクションで既に一覧化しているので、二重に主張しない（`RecordingRow`
 * の doc コメントと同じ規律 --- 警告セクションが「その事実の一覧化」を一手に
 * 引き受け、他の行は識別情報だけを持つ）。行自体は 2 つ目の対話要素を持たない
 * ので、`pages/reservations.tsx` のような「行本体を絶対配置の面にする」入れ子
 * 回避の手当ても不要 --- `<li>` をそのまま 1 つの `Link` にできる。
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

/** WarningKind はホーム「警告」セクションの項目の種別。表示色と、色を選ぶ判断の両方をこれ 1 つに一本化する。 */
type WarningKind = 'breaker' | 'overage' | 'drop'

/** WarningItem はホーム「警告」セクションの 1 件（サーキットブレーカー / チューナー不足 / ドロップ）。 */
type WarningItem = {
  key: string
  /**
   * 表示色・将来の出し分けの判断はすべてこの値を経由させる（文字列の `key` を
   * 前方一致で覗いて種別を推測する実装は、`key` の書式を変えただけで表示が
   * 黙って壊れる。レビュー指摘で `buildWarnings` が組む時点の種別をそのまま
   * 持たせる形に直した）。
   */
  kind: WarningKind
  message: string
  /** 遷移先。サーキットブレーカーは対応する専用画面が無い（`CircuitBreakerBanner` が同じページの上部で扱う）ので省略。 */
  link?: { to: '/programs'; search: { at: number } } | { to: '/recordings/$id'; id: number }
}

/**
 * buildWarnings はサーキットブレーカー・容量超過・ドロップ統計から
 * ホームの「警告」セクションの項目を組む（issue #242 着手宣言コメントの決定：
 * 新しい API を作らず、既存の取得結果だけを材料にする）。
 *
 * `dropCandidates` は表示用の「直近の完了」一覧とは別に、より広い範囲
 * （`DROP_WARNING_SCAN_LIMIT`）で取った録画一覧 --- 表示件数を絞っても警告の
 * 検出範囲まで連動して狭まらないようにするため（呼び出し元の doc コメント参照）。
 *
 * 容量超過は呼び出し元で「実際の今より後に終わる」ものへ絞り込み済みなので、
 * ここで追加の時間フィルタはしない。
 */
function buildWarnings({
  breakers,
  overages,
  dropCandidates,
}: {
  breakers: readonly CircuitBreaker[]
  overages: readonly CapacityOverage[]
  dropCandidates: readonly Recording[]
}): WarningItem[] {
  const items: WarningItem[] = []

  for (const breaker of breakers) {
    items.push({
      key: `breaker:${breaker.site}:${breaker.name}`,
      kind: 'breaker',
      message: `${describeBreakerName(breaker.name)}が停止中（保留 ${breaker.pending} 件）`,
    })
  }

  for (const overage of overages) {
    items.push({
      key: `overage:${overage.site}:${overage.startAt}:${overage.endAt}`,
      kind: 'overage',
      message: shortageRangeMessage(overage),
      link: { to: '/programs', search: { at: new Date(overage.startAt).getTime() } },
    })
  }

  for (const recording of dropCandidates) {
    const summary = recording.dropSummary
    if (summary === undefined) continue
    if (summary.drops === 0 && summary.errors === 0 && summary.scrambled === 0) continue
    const parts = [
      { label: 'drop', value: summary.drops },
      { label: 'error', value: summary.errors },
      { label: 'scrambled', value: summary.scrambled },
    ]
      .filter((b) => b.value > 0)
      .map((b) => `${b.label} ${b.value.toLocaleString()}`)
      .join(' / ')
    items.push({
      key: `drop:${recording.id}`,
      kind: 'drop',
      message: `${recording.title || '（番組名なし）'}: ${parts}`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  return items
}

/**
 * WarningRow は 1 件の警告。サーキットブレーカーと直近のドロップは「取り返しが
 * つかない/止まっている」意味の destructive、チューナー不足は容量バッジ
 * （`components/capacity-shortfall-badge.tsx`）と同じ warning（琥珀）に揃える
 * （docs/frontend/design.md「色は信号のみ」。同じ事実は同じ色で言う）。
 */
function WarningRow({ warning }: { warning: WarningItem }) {
  const amber = warning.kind === 'overage'
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
