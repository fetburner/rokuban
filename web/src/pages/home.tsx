import { keepPreviousData } from '@tanstack/react-query'
import { Link, useRouterState } from '@tanstack/react-router'
import { Fragment, useEffect, useLayoutEffect, useRef, useState } from 'react'

import {
  useGetEncodeQueue,
  useGetStorage,
  useListCapacityOverages,
  useListCircuitBreakers,
  useListContinueWatching,
  useListRecordings,
  useListReservations,
  type CapacityOverage,
  type CircuitBreaker,
  type EncodeQueueSummary,
  type Recording,
  type Reservation,
  type StorageRoot,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { ThumbnailOverlay } from '@/components/thumbnail-overlay'
import { HomeModeToggle } from '@/components/home-mode-toggle'
import { describeBreakerName, describeBreakerReason } from '@/lib/breaker'
import { dayOrigin } from '@/lib/day-offset'
import { formatBytes, formatDate, formatDateTime, formatDuration, formatTime } from '@/lib/format'
import {
  readHomeModePreference,
  resolveHomeMode,
  saveHomeModePreference,
} from '@/lib/home-mode'
import { chooseHomeHero, homeNewArrivals, type HomeHeroChoice } from '@/lib/home-selection'
import {
  buildHomeTimelineRows,
  homeTimelineChannelLabel,
  homeTimelineDayLabel,
  homeTimelineKindLabel,
  homeTimelineTickLabel,
  sortHomeTimelineEvents,
  type HomeTimelineEvent,
  type HomeTimelineKind,
} from '@/lib/home-timeline'
import { programTitle } from '@/lib/program-labels'
import {
  estimateAverageBitrate,
  estimateStorageForecast,
  findMediaRoot,
  isObservationStale,
  forecastWindowDays,
  recentBitrateSamples,
  recentRecordingSampleLimit,
  upcomingReservationSchedule,
} from '@/lib/storage-forecast'
import { cn } from '@/lib/utils'

/**
 * DROP_WARNING_SCAN_LIMIT はドロップ警告の材料を取る範囲（完了録画の直近 20 件）。
 * **時間軸の窓とは独立した定数にしてある** --- 表示の都合で窓を狭めたとき、警告の
 * 遡り幅まで黙って縮まないようにするため（検出は正しさの都合、窓は見た目の都合）。
 * 値は実測ではない恣意的な上限。判定: `pages/home.test.tsx`「今日 0 時より前の
 * finished drop も 20 件の警告 scan から拾う」。
 */
const DROP_WARNING_SCAN_LIMIT = 20

/**
 * FAILED_RECORDING_SCAN_LIMIT は「要対応」に出す失敗録画（`status=failed`）を取る
 * 範囲。`DROP_WARNING_SCAN_LIMIT` とは別の定数にする（値だけ共有すると、一方を
 * 変えたときもう一方の遡り幅まで連動する）。値は実測ではない恣意的な上限。
 */
const FAILED_RECORDING_SCAN_LIMIT = 20

/**
 * FAILED_RECORDING_WARNING_WINDOW_MS は警告に出す失敗録画の recency 窓。他の警告材料（ブレーカーは発動中のみ・容量超過は今日〜明日の窓・
 * ドロップは直近 20 件の完了で実質 recency がある）はどれも自然に消えるが、
 * 失敗だけは `FAILED_RECORDING_SCAN_LIMIT` 件に収まる限り**いつの失敗でも
 * 出続けてしまう**。稼働の長いサーバーでは警告セクションが古い失敗で常時
 * 埋まり、警告全体の情報価値が下がる（issue #301 の受け入れ基準も「直近の」
 * 失敗録画と言っている）。窓は録画の `startAt`（番組の放送開始。失敗の場合も
 * 必ず持つ --- `startedAt` と違い欠けることが無い）で判定する。値（7 日）は
 * 実測ではなく「1〜2 週間動かして異常が無いか確認する」運用サイクルに対して
 * 「今日気付くべき失敗」を残す側に振った恣意的な上限（他の上限と同じ性質）。
 */
const FAILED_RECORDING_WARNING_WINDOW_MS = 7 * 24 * 3_600_000

/** 時間軸は API の `limit` 上限である 200 件までを既存 API から取得する。 */
const HOME_TIMELINE_RECORDING_LIMIT = 200
// 完了・失敗録画を窓の始点より前から取る幅。API の `from` は開始時刻で絞るので、
// 0 時より前に始まって窓に食い込む録画を拾うにはこの分さかのぼる。
// ponytail: 24 時間を超えて窓に食い込む録画は落ちる（番組の最長尺は測っていない）。
// 落ちる例が出たら幅を広げる。
const HOME_TIMELINE_LOOKBACK_MS = 24 * 3_600_000
const HOME_TIMELINE_HOUR_PX_DESKTOP = 64
const HOME_TIMELINE_HOUR_PX_PHONE = 30
const HOME_TIMELINE_TRACK_HEIGHT_PX = 20
const HOME_TIMELINE_TRACK_GAP_PX = 3
const HOME_TIMELINE_PAST_CONTEXT_MS = 3 * 3_600_000
/** 新着カードの幅。本文内で 1 行を保てる最低幅から列数を計算する。 */
const HOME_ARRIVAL_CARD_MIN_WIDTH_PX = 176
const HOME_ARRIVAL_CARD_GAP_PX = 12

/**
 * HomePage はホーム（`/`）。右上の「見る / 管理」で 2 つのモードを切り替える
 * （`lib/home-mode.ts`。設計の判断は docs/frontend/home.md）。
 *
 * 見るモードは「次に見る 1 本」・ほかの新着・録画中の細い行。管理モードは
 * 時間軸（今日 0 時〜明日の終わり）・要対応の一覧・ストレージの 1 行。
 * どちらのモードでも使わないクエリは発行しない（`enabled`）。
 *
 * **0 件のものは文言も出さず消え、材料が全部確定して表示対象も無いときだけ
 * 単一の空状態を出す**。ホームの空は「何も主張しない」ことそのもので、肯定的な
 * 文言（「異常なし」）には転ばない。未解決の材料を 0 件として隠さない（読み込み中の
 * 一瞬を「無い」と誤読させない。CLAUDE.md「非同期の空虚な成功」）。
 *
 * 要対応の材料（ブレーカー・容量超過・完了録画のドロップと CM 検出失敗・失敗録画）の
 * 取得失敗は「警告が無い」へ縮退する（`CircuitBreakerBanner` / 予約一覧の容量バッジと同じ流儀。
 * docs/data.md §6.5 の既知の盲点）。時間軸の取得失敗は空にせずエラーを出す。
 * 失敗録画（`status=failed`）は専用の一覧を持たず、時間軸のブロックと要対応の
 * 追加項目としてだけ出す。行では予定尺（`durationMs`）と実際に録れた尺
 * （`startedAt`〜`endedAt`）を区別する（`failedDurationText`）。
 */
export function HomePage() {
  const locationSearch = useRouterState({ select: (state) => state.location.search }) as Record<
    string,
    unknown
  >
  const mode = resolveHomeMode(locationSearch.mode, readHomeModePreference())

  // nowMs はこのレンダーの間で一貫させる（`pages/programs.tsx` と同じ規律。
  // 起点・上限を別々に Date.now() を呼んで求めると、ミリ秒単位でずれた「今」が
  // 混ざりうる）。
  // 予約・容量の窓を同じ瞬間の観測で組み立てる。時刻を state 初期値にすると、
  // クエリ再取得後の「いま」と予測窓が古いままになる。
  // oxlint-disable-next-line react/purity -- 各レンダーで一貫した現在時刻スナップショットが必要
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

  // 時間軸は常に今日の 0 時から明日の暦日の終わり（予約窓と同じ終端）までの 48 時間。
  // 始点を現在時刻に依存させない（正午固定だと午前に開いたとき午前の録画が窓から
  // 落ち、「いま」の線が窓の外に張り付く。測定: home.test.tsx「午前に開いても…」）。
  const timelineWindowStartDate = new Date(nowMs)
  timelineWindowStartDate.setHours(0, 0, 0, 0)
  const timelineWindowStartMs = timelineWindowStartDate.getTime()
  const timelineWindowEndMs = reservationsWindowEndMs
  // 完了・失敗は窓の始点より `HOME_TIMELINE_LOOKBACK_MS` 前から取り、下で窓と
  // 重なるものだけに絞る。
  const timelineLookbackFrom = new Date(timelineWindowStartMs - HOME_TIMELINE_LOOKBACK_MS).toISOString()
  const timelineTo = new Date(timelineWindowEndMs).toISOString()

  // 見るモードだけの材料。管理モードでは取らない。
  const recordingQuery = useListRecordings(
    { status: 'recording' },
    { query: { enabled: mode === 'watch' } },
  )
  const continueWatchingQuery = useListContinueWatching({ query: { enabled: mode === 'watch' } })
  // ドロップ警告の検出（`DROP_WARNING_SCAN_LIMIT` 件）と、見るモードの主役・
  // ほかの新着の材料を兼ねる 1 本。時間軸の窓とは独立（時間軸は下の 3 本）。
  const finishedQuery = useListRecordings({
    status: 'finished',
    limit: DROP_WARNING_SCAN_LIMIT,
  })
  // 失敗録画。取る範囲がそのまま「要対応」に出す範囲になる。
  const failedQuery = useListRecordings({
    status: 'failed',
    limit: FAILED_RECORDING_SCAN_LIMIT,
  })
  // 時間軸の録画は status 別に取り、窓と**重なる**ものを描く（予約と同じ規則）。
  // API の `from` / `to` は `program_start_at` の範囲なので、`from` に窓の始点を
  // 渡すと 0 時より前に始まった日またぎの録画が録画中でも落ちる（深夜の映画を
  // 0:00 から終わるまで消していた。測定: home.test.tsx「0 時をまたぐ録画は…」）。
  // 録画中は `from` を付けない（件数はチューナー数で抑えられる）。
  const timelineRecordingQuery = useListRecordings(
    {
      status: 'recording',
      limit: HOME_TIMELINE_RECORDING_LIMIT,
    },
    { query: { enabled: mode === 'ops' } },
  )
  const timelineFinishedQuery = useListRecordings(
    {
      status: 'finished',
      from: timelineLookbackFrom,
      to: timelineTo,
      limit: HOME_TIMELINE_RECORDING_LIMIT,
    },
    { query: { enabled: mode === 'ops' } },
  )
  // `status=failed` を必須にする: API が supersede 済みの擬似 failed 行を除外する。
  // 時間軸用の範囲は警告用の 7 日窓とは独立させる。
  const timelineFailedQuery = useListRecordings(
    {
      status: 'failed',
      from: timelineLookbackFrom,
      to: timelineTo,
      limit: HOME_TIMELINE_RECORDING_LIMIT,
    },
    { query: { enabled: mode === 'ops' } },
  )
  const reservationsQuery = useListReservations()
  const breakersQuery = useListCircuitBreakers()
  const storageQuery = useGetStorage({ query: { enabled: mode === 'ops' } })
  const encodeQueueQuery = useGetEncodeQueue({ query: { enabled: mode === 'ops' } })
  // StorageBalance と同じ finished sample limit を明示する。今は drop scan と同じ
  // 20 件なので React Query の同一 query key を共有するが、母数の規則は独立させる。
  const storageRecordingsQuery = useListRecordings(
    { status: 'finished', limit: recentRecordingSampleLimit },
    { query: { enabled: mode === 'ops' } },
  )
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(overagesStartMs).toISOString(),
      end: new Date(reservationsWindowEndMs).toISOString(),
    },
    {
      // **時境界を越えた瞬間に警告セクションを消さない。** 上の量子化により
      // キーは毎時 0 分に 1 回変わる。新しいキーにはまだデータが無いので、
      // 素のままだと `isPending` → `warningsPending` → 警告セクションが 1 RTT
      // だけ消える（警告だけが可視だった場合はページ全体がスケルトンに戻る）。
      // この画面の主題は「セクションが理由なく消えないこと」なので、キーが
      // 進んでいる間は前のキーのデータを見せ続ける（`isPending` は false のまま
      // になる）。判定はテスト「時境界を越えてキーが変わっても警告は消えない」。
      query: { placeholderData: keepPreviousData },
    },
  )

  const recordingsInProgress = unwrap(recordingQuery.data) ?? []
  const continueWatching = unwrap(continueWatchingQuery.data) ?? []
  // 以下の導出は `useMemo` を使わない。**この関数の中で最も頻繁に変わる依存は
  // `nowMs`（= 生の `Date.now()`）で、レンダーごとに必ず変わる。** それを deps に
  // 持つ `useMemo` は毎レンダー再計算されるので何も買っていない（レビュー指摘。
  // 以前は `activeOverages` / `upcomingReservations` / `warnings` を `useMemo` で
  // 包んでおり、後者 2 つは前者が毎レンダー新しい配列になることで連鎖して
  // 再計算されていた）。録画は API の `limit`、超過区間は窓幅で上界があるが、
  // **予約だけは上界が無い**（`GET /api/reservations` は絞り込みパラメータを
  // 持たない全件取得で `limit` も無い）。それでも `useMemo` は上記のとおり
  // `nowMs` 依存で効かないので、素朴に毎レンダー計算する。
  const finishedRecordings = unwrap(finishedQuery.data) ?? []
  const failedRecordings = unwrap(failedQuery.data) ?? []

  // `overagesStartMs` を時境界へ丸めた分だけ、実際の「今」より前に始まって
  // **既に終わった**区間まで返ってきうる（`openapi.yaml` の `start` は「この時刻
  // より後に終わる区間が対象」なので、`start` を時頭まで後退させて増えるのは
  // ちょうど「[時頭, now] に終わった区間」だけ）。ここで「実際の今より後に
  // 終わる」区間だけへ絞り、量子化前と同じ主張の強さに戻す（量子化はキャッシュ
  // キーの安定のためだけの手段で、表示する内容の正しさを緩めてよい理由には
  // しない）。これが無いと「もう終わったチューナー不足」が最大 59 分ぶん警告に
  // 出続ける。判定はテスト「既に終わった超過区間は警告に出さない」/「時境界より
  // 前に始まって進行中の超過区間は警告に出す」の両方向。
  const activeOverages = (unwrap(overagesQuery.data) ?? []).filter(
    (o) => new Date(o.endAt).getTime() > nowMs,
  )

  // 失敗録画は `FAILED_RECORDING_SCAN_LIMIT` 件に収まる限りいつの失敗でも警告に
  // 出続けてしまうので、ここで recency 窓へ絞る
  // （`FAILED_RECORDING_WARNING_WINDOW_MS` の doc コメント参照）。
  const recentFailedRecordings = failedRecordings.filter(
    (r) => new Date(r.startAt).getTime() >= nowMs - FAILED_RECORDING_WARNING_WINDOW_MS,
  )

  const breakers = unwrap(breakersQuery.data) ?? []
  const warnings = buildWarnings({
    breakers,
    overages: activeOverages,
    finishedCandidates: finishedRecordings,
    failedRecordings: recentFailedRecordings,
    reservations: reservationsQuery.isError ? undefined : unwrap(reservationsQuery.data),
    nowMs,
  })

  const warningsPending =
    breakersQuery.isPending ||
    overagesQuery.isPending ||
    finishedQuery.isPending ||
    failedQuery.isPending
  const warningCount = warningsPending ? undefined : warnings.length
  const watchBreakerBand =
    !breakersQuery.isPending && breakers.length > 0 ? <WatchBreakerBand breakers={breakers} /> : null

  // Watch では両方の一覧が解決した後にだけ主役を決める。続きからが空である
  // ことを確認できなければ完了録画へフォールバックしない。
  const homeHeroChoice = chooseHomeHero(
    {
      pending: continueWatchingQuery.isPending,
      error: continueWatchingQuery.isError,
      items: continueWatching,
    },
    { pending: finishedQuery.isPending, error: finishedQuery.isError, items: finishedRecordings },
  )
  const homeHeroError =
    homeHeroChoice === null && (continueWatchingQuery.isError || finishedQuery.isError)
  const watchArrivals =
    homeHeroChoice !== undefined &&
    !continueWatchingQuery.isError &&
    !finishedQuery.isError &&
    homeHeroChoice !== null
      ? homeNewArrivals(
          continueWatching,
          finishedRecordings,
          homeHeroChoice.recording.id,
          20,
        )
      : []
  const headerActions = (
    <HomeModeToggle mode={mode} warningCount={warningCount} />
  )

  if (mode === 'watch') {
    return (
      <>
        <PageHeader title="ホーム" actions={headerActions}>
          {watchBreakerBand}
        </PageHeader>
        <PageContent>
          <div className="flex flex-col gap-5 px-4 py-4">
            {homeHeroChoice === undefined ? (
              <div aria-label="次に見る録画を読み込み中" role="status">
                <ListSkeleton rows={3} />
              </div>
            ) : homeHeroError ? (
              <p role="alert" className="text-sm text-destructive">
                次に見る録画の取得に失敗しました
              </p>
            ) : homeHeroChoice !== null ? (
              <WatchHero choice={homeHeroChoice} />
            ) : null}

            {homeHeroChoice !== undefined && watchArrivals.length > 0 && (
              <HomeNewArrivals recordings={watchArrivals} />
            )}

            {!recordingQuery.isPending && recordingsInProgress.length > 0 && (
              <RecordingStrip recordings={recordingsInProgress} />
            )}
            {recordingQuery.isError && (
              <p role="alert" className="text-sm text-destructive">
                録画中の取得に失敗しました
              </p>
            )}
          </div>
        </PageContent>
      </>
    )
  }

  const reservations = unwrap(reservationsQuery.data) ?? []
  const timelineRecordingEvents = [
    ...(unwrap(timelineRecordingQuery.data) ?? []).map((recording) =>
      recordingTimelineEvent(recording, 'recording'),
    ),
    ...(unwrap(timelineFinishedQuery.data) ?? []).map((recording) =>
      recordingTimelineEvent(recording, 'finished'),
    ),
    ...(unwrap(timelineFailedQuery.data) ?? []).map((recording) =>
      recordingTimelineEvent(recording, 'failed'),
    ),
  ]
  const timelineReservationEvents = reservations.map(reservationTimelineEvent)
  // 録画・予約とも、窓と重なるものだけを置く（左右の端で切って描く）。
  const timelineEvents = [...timelineRecordingEvents, ...timelineReservationEvents].filter(
    (event): event is HomeTimelineEvent =>
      event !== null && event.endMs > timelineWindowStartMs && event.startMs < timelineWindowEndMs,
  )
  const timelineRows = buildHomeTimelineRows(timelineEvents, activeOverages)
  const timelinePending =
    timelineRecordingQuery.isPending ||
    timelineFinishedQuery.isPending ||
    timelineFailedQuery.isPending ||
    reservationsQuery.isPending ||
    overagesQuery.isPending
  const timelineError =
    timelineRecordingQuery.isError ||
    timelineFinishedQuery.isError ||
    timelineFailedQuery.isError ||
    reservationsQuery.isError ||
    overagesQuery.isError
  const storageRoots = unwrap(storageQuery.data)
  const mediaRoot = storageRoots === undefined ? undefined : findMediaRoot(storageRoots)
  const hasVisibleOpsData =
    timelineEvents.length > 0 ||
    timelineRows.some((row) => row.overages.length > 0) ||
    (!warningsPending && warnings.length > 0) ||
    mediaRoot !== undefined
  const allOpsSettled =
    !timelinePending &&
    !warningsPending &&
    !storageQuery.isPending &&
    !storageRecordingsQuery.isPending &&
    !encodeQueueQuery.isPending

  return (
    <>
      <PageHeader title="ホーム" actions={headerActions} />
      <PageContent className="min-w-0 overflow-hidden">
        {allOpsSettled && !timelineError && !storageQuery.isError && !hasVisibleOpsData ? (
          <EmptyState>表示できる項目がありません</EmptyState>
        ) : (
          <div className="flex min-w-0 flex-col gap-5 px-4 py-4">
            <HomeOpsTimeline
              nowMs={nowMs}
              startMs={timelineWindowStartMs}
              endMs={timelineWindowEndMs}
              rows={timelineRows}
              events={timelineEvents}
              isPending={timelinePending}
              isError={timelineError}
            />
            {!warningsPending && warnings.length > 0 && (
              <section aria-labelledby="home-action-required" className="flex min-w-0 flex-col gap-2">
                <div className="flex items-baseline justify-between gap-2">
                  <h2 id="home-action-required" className="text-sm font-semibold">
                    要対応
                  </h2>
                  <span className="text-xs text-muted-foreground">
                    {warnings.length} 件
                  </span>
                </div>
                <ul className="flex min-w-0 flex-col overflow-hidden rounded-md border border-border bg-card">
                  {warnings.map((warning, index) => (
                    <WarningRow key={warning.key} warning={warning} divider={index > 0} />
                  ))}
                </ul>
              </section>
            )}
            {!storageQuery.isPending && !storageQuery.isError && (
              <OpsStorageLine
                roots={storageRoots}
                finishedRecordings={storageRecordingsQuery.isError ? undefined : unwrap(storageRecordingsQuery.data)}
                reservations={reservationsQuery.isError ? undefined : unwrap(reservationsQuery.data)}
                encodeQueue={encodeQueueQuery.isError ? undefined : unwrap(encodeQueueQuery.data)}
                nowMs={nowMs}
              />
            )}
          </div>
        )}
      </PageContent>
    </>
  )

}

function recordingTimelineEvent(
  recording: Recording,
  kind: Extract<HomeTimelineKind, 'recording' | 'finished' | 'failed'>,
): HomeTimelineEvent | null {
  if (
    (kind === 'recording' && recording.status !== 'recording') ||
    (kind === 'finished' && recording.status !== 'finished') ||
    (kind === 'failed' && recording.status !== 'failed')
  ) {
    return null
  }
  const startMs = new Date(recording.startAt).getTime()
  const endMs = startMs + recording.durationMs
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) return null
  const dropSummary = recording.dropSummary
  return {
    key: `recording:${recording.id}`,
    site: recording.site,
    channelType: recording.channelType,
    startMs,
    endMs,
    kind,
    title: recording.title,
    hasDrop:
      kind === 'finished' &&
      dropSummary !== undefined &&
      (dropSummary.drops > 0 || dropSummary.errors > 0 || dropSummary.scrambled > 0),
    href: { to: '/recordings/$id', id: recording.id },
  }
}

function reservationTimelineEvent(reservation: Reservation): HomeTimelineEvent | null {
  // A skipped reservation is retained as user intent but is not synchronized to mirakc.
  if (reservation.skip) return null
  const startMs = new Date(reservation.startAt).getTime()
  const endMs = startMs + reservation.durationMs
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) return null
  return {
    key: `reservation:${reservation.site}:${reservation.programId}`,
    site: reservation.site,
    channelType: reservation.channelType,
    startMs,
    endMs,
    kind: 'reservation',
    title: reservation.title,
    hasDrop: false,
    href: {
      to: '/reservations/$site/$programId',
      site: reservation.site,
      programId: reservation.programId,
    },
  }
}

function HomeOpsTimeline({
  nowMs,
  startMs,
  endMs,
  rows,
  events,
  isPending,
  isError,
}: {
  nowMs: number
  startMs: number
  endMs: number
  rows: ReturnType<typeof buildHomeTimelineRows>
  events: HomeTimelineEvent[]
  isPending: boolean
  isError: boolean
}) {
  const frameRef = useRef<HTMLDivElement>(null)
  const initialScrollLayoutRef = useRef<string | null>(null)
  // 縮尺・ラベル幅の切替は window 幅だけで決める。frame の幅から決めると、切替が
  // ラベル幅を変え、ラベル幅が frame の幅を変える帰還ループになる（実測: 単一 site
  // で window 592–598px、複数 site で 660–676px で 2 つのモードを往復した）。
  const [viewportWidth, setViewportWidth] = useState(() => window.innerWidth)
  const [frameViewport, setFrameViewport] = useState({ scrollLeft: 0, clientWidth: 0 })
  // 目盛りラベルと「いま」の pill の実幅（px）。隠す判定に使う。0 は未計測。
  const tickLabelRefs = useRef<(HTMLSpanElement | null)[]>([])
  const nowPillRef = useRef<HTMLSpanElement>(null)
  const [labelWidths, setLabelWidths] = useState<{ ticks: number[]; pill: number }>({ ticks: [], pill: 0 })
  const hourPx = viewportWidth <= 480 ? HOME_TIMELINE_HOUR_PX_PHONE : HOME_TIMELINE_HOUR_PX_DESKTOP
  const durationMs = Math.max(0, endMs - startMs)
  const axisWidth = (durationMs / 3_600_000) * hourPx
  const contentWidth = axisWidth + 64
  const nowX = Math.max(0, Math.min(axisWidth, ((nowMs - startMs) / 3_600_000) * hourPx))
  const siteCount = new Set(rows.map((row) => row.site)).size
  const labelWidth = siteCount > 1 ? (viewportWidth <= 480 ? 112 : 132) : viewportWidth <= 480 ? 44 : 52
  const rowTypes = [...new Set(rows.map((row) => homeTimelineChannelLabel(row.channelType)))]
  const tickHours = viewportWidth <= 480 ? 2 : 3
  const ticks: number[] = []
  for (let elapsedHour = 0; elapsedHour < durationMs / 3_600_000; elapsedHour += tickHours) {
    ticks.push(elapsedHour)
  }

  useEffect(() => {
    const onResize = () => setViewportWidth(window.innerWidth)
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  useLayoutEffect(() => {
    const frame = frameRef.current
    if (frame === null) return
    const resize = () => {
      setFrameViewport({ scrollLeft: frame.scrollLeft, clientWidth: frame.clientWidth })
    }
    resize()
    const observer = new ResizeObserver(resize)
    observer.observe(frame)
    return () => observer.disconnect()
  }, [isError, isPending, rows.length])

  // ラベルの文言（翌 / 時刻）と縮尺で幅が変わるので毎レンダー測り、変わったときだけ更新する。
  useLayoutEffect(() => {
    const measure = () => {
      const widths = tickLabelRefs.current.slice(0, ticks.length).map((element) => element?.offsetWidth ?? 0)
      const pill = nowPillRef.current?.offsetWidth ?? 0
      setLabelWidths((current) =>
        current.pill === pill &&
        current.ticks.length === widths.length &&
        current.ticks.every((width, index) => width === widths[index])
          ? current
          : { ticks: widths, pill },
      )
    }
    measure()
  })

  useLayoutEffect(() => {
    const frame = frameRef.current
    if (frame === null) {
      initialScrollLayoutRef.current = null
      return
    }
    // nowX can move when HomePage rerenders after an SSE/query update. Keep following
    // the current marker without taking control back from a user who scrolled manually.
    const layoutKey = `${hourPx}:${contentWidth}`
    if (initialScrollLayoutRef.current === layoutKey) return
    initialScrollLayoutRef.current = layoutKey
    const target = Math.max(0, nowX - HOME_TIMELINE_PAST_CONTEXT_MS / 3_600_000 * hourPx)
    frame.scrollLeft = Math.max(0, Math.min(target, frame.scrollWidth - frame.clientWidth))
    setFrameViewport({ scrollLeft: frame.scrollLeft, clientWidth: frame.clientWidth })
  }, [contentWidth, hourPx, isError, isPending, nowX, rows.length])

  const timelineContent = (
    <div
      className="relative flex min-w-0 flex-col gap-2"
      style={{ width: `${contentWidth}px` }}
      data-testid="home-ops-timeline-content"
    >
      <div className="relative h-[14px]" aria-hidden="true">
        {ticks.map((elapsedHour, index) => {
          const tickMs = startMs + elapsedHour * 3_600_000
          const tickX = elapsedHour * hourPx
          // 見える範囲の境界で半端に切れるラベルと、「いま」の pill（z-20）の下に
          // 隠れるラベルは隠す。ラベルの箱は実幅で測る（始端の目盛りだけは左揃え）。
          // 未計測（jsdom 等）のあいだは中心 ± 20px の箱で枠の境界だけを判定する。
          const labelWidth = labelWidths.ticks[index] || 40
          const labelStart = elapsedHour === 0 ? tickX : tickX - labelWidth / 2
          const labelEnd = labelStart + labelWidth
          const visibleStart = frameViewport.scrollLeft
          const visibleEnd = visibleStart + frameViewport.clientWidth
          const tickLabelIntersectsFrame = labelEnd > visibleStart && labelStart < visibleEnd
          const tickLabelFits = labelStart >= visibleStart && labelEnd <= visibleEnd
          // pill は nowX - 1px から実幅ぶん。左右 2px の余白を取って重なりを見る。
          const pillStart = nowX - 1
          const tickLabelUnderPill = labelWidths.pill > 0 &&
            labelEnd + 2 > pillStart && labelStart - 2 < pillStart + labelWidths.pill
          return (
            <span
              key={elapsedHour}
              ref={(element) => {
                tickLabelRefs.current[index] = element
              }}
              className={cn(
                'absolute top-0 whitespace-nowrap text-[10px] leading-[14px] text-muted-foreground',
                elapsedHour !== 0 && '-translate-x-1/2',
              )}
              style={{
                left: `${tickX}px`,
                visibility: tickLabelUnderPill ||
                  (frameViewport.clientWidth > 0 && tickLabelIntersectsFrame && !tickLabelFits)
                  ? 'hidden'
                  : undefined,
              }}
              data-testid="home-timeline-tick"
            >
              {homeTimelineTickLabel(tickMs, startMs)}
            </span>
          )
        })}
        <span
          ref={nowPillRef}
          data-testid="home-timeline-now-label"
          className="absolute top-0 z-20 -translate-x-px rounded-sm bg-tally px-1 text-[10px] font-semibold leading-[14px] text-tally-foreground"
          style={{ left: `${nowX}px` }}
        >
          いま {formatTime(new Date(nowMs).toISOString())}
        </span>
      </div>
      {rows.map((row) => {
        const height = row.trackCount * HOME_TIMELINE_TRACK_HEIGHT_PX +
          Math.max(0, row.trackCount - 1) * HOME_TIMELINE_TRACK_GAP_PX
        return (
          <div
            key={row.key}
            className="relative"
            style={{ height: `${height}px`, width: `${axisWidth}px` }}
            data-testid="home-timeline-row"
          >
            {Array.from({ length: row.trackCount }, (_, track) => (
              <div
                key={track}
                className="absolute left-0 h-5 rounded-sm bg-foreground/[0.03]"
                style={{
                  top: `${track * (HOME_TIMELINE_TRACK_HEIGHT_PX + HOME_TIMELINE_TRACK_GAP_PX)}px`,
                  width: `${axisWidth}px`,
                }}
                aria-hidden="true"
              />
            ))}
            {row.events.map((event) => {
              const left = Math.max(0, ((event.startMs - startMs) / 3_600_000) * hourPx)
              const right = Math.min(axisWidth, ((event.endMs - startMs) / 3_600_000) * hourPx)
              const width = Math.max(0, right - left)
              const kindClass = event.kind === 'recording'
                ? 'bg-tally font-semibold text-tally-foreground'
                : event.kind === 'failed'
                  ? 'border border-destructive bg-destructive/10 text-destructive'
                  : event.kind === 'reservation'
                    ? 'border border-foreground/40 bg-card'
                    : event.hasDrop
                      ? 'bg-foreground/20 shadow-[inset_0_-3px_0_var(--destructive)]'
                      : 'bg-foreground/20'
              return width > 0 ? (
                <div
                  key={event.key}
                  className={cn(
                    'absolute z-[1] box-border h-4 overflow-hidden rounded-sm px-[3px] text-[10px] leading-4 whitespace-nowrap',
                    kindClass,
                  )}
                  style={{
                    left: `${left}px`,
                    top: `${event.track * (HOME_TIMELINE_TRACK_HEIGHT_PX + HOME_TIMELINE_TRACK_GAP_PX) + 2}px`,
                    width: `${width}px`,
                  }}
                  title={programTitle(event.title)}
                  data-testid="home-timeline-block"
                  data-kind={event.kind}
                  data-duration-ms={event.endMs - event.startMs}
                  aria-hidden="true"
                >
                  {width >= 42 ? programTitle(event.title) : null}
                </div>
              ) : null
            })}
            {row.overages.map((overage) => {
              const left = Math.max(0, ((overage.startMs - startMs) / 3_600_000) * hourPx)
              const right = Math.min(axisWidth, ((overage.endMs - startMs) / 3_600_000) * hourPx)
              if (right <= left) return null
              const labelLeft = Math.max(0, Math.min(axisWidth - 92, right + 4))
              return (
                <div
                  key={overage.key}
                  className="pointer-events-none absolute inset-y-[-3px] z-10 border border-dashed border-warning bg-[repeating-linear-gradient(45deg,color-mix(in_oklch,var(--warning)_22%,transparent)_0_3px,transparent_3px_7px)]"
                  style={{ left: `${left}px`, width: `${right - left}px` }}
                  aria-hidden="true"
                >
                  <span
                    className="absolute top-0 whitespace-nowrap bg-card px-[3px] text-[10px] font-semibold leading-4 text-warning"
                    style={{ left: `${labelLeft - left}px` }}
                    data-testid="home-overage-label"
                  >
                    {overage.shortfall} 本不足
                  </span>
                </div>
              )
            })}
            <div
              className="pointer-events-none absolute inset-y-[-12px] z-[15] w-0.5 bg-tally"
              style={{ left: `${nowX}px` }}
              data-testid="home-timeline-now"
              aria-hidden="true"
            />
          </div>
        )
      })}
    </div>
  )

  return (
    <>
      <section aria-labelledby="home-ops-timeline-title" className="flex min-w-0 flex-col gap-2" data-testid="home-ops-timeline" data-hour-px={hourPx} data-viewport-width={viewportWidth}>
        <div className="flex items-baseline justify-between gap-2">
          <h2 id="home-ops-timeline-title" className="text-sm font-semibold">今日 0 時 → 明日の終わり</h2>
          {rowTypes.length > 0 && (
            <span className="text-xs text-muted-foreground">
              {siteCount > 1 ? 'サイト × ' : ''}{rowTypes.join(' / ')} ごと
            </span>
          )}
        </div>
        {isPending ? (
          <div role="status" aria-label="時間軸を読み込み中"><ListSkeleton rows={3} /></div>
        ) : isError ? (
          <p role="alert" className="text-sm text-destructive">時間軸の取得に失敗しました</p>
        ) : rows.length === 0 ? null : (
          <div className="flex min-w-0 flex-col gap-2 overflow-hidden rounded-md border border-border bg-card p-3">
            <div className="grid min-w-0 gap-2" style={{ gridTemplateColumns: `${labelWidth}px minmax(0, 1fr)` }}>
              <div className="flex flex-col gap-2" aria-hidden="true">
                <div className="h-[14px]" />
                {rows.map((row) => {
                  const height = row.trackCount * HOME_TIMELINE_TRACK_HEIGHT_PX +
                    Math.max(0, row.trackCount - 1) * HOME_TIMELINE_TRACK_GAP_PX
                  return (
                    <div
                      key={row.key}
                      className="flex items-center truncate text-xs font-semibold"
                      style={{ height: `${height}px` }}
                      data-testid="home-timeline-row-label"
                    >
                      {siteCount > 1 ? `${row.site} · ` : ''}{homeTimelineChannelLabel(row.channelType)}
                    </div>
                  )
                })}
              </div>
              <div
                ref={frameRef}
                className="min-w-0 overflow-x-auto overflow-y-hidden pb-3 [&::-webkit-scrollbar]:h-2 [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-foreground/30"
                data-testid="home-ops-timeline-frame"
                role="region"
                aria-label="時間軸。横にスクロールできます"
                tabIndex={0}
                onScroll={(event) => setFrameViewport({
                  scrollLeft: event.currentTarget.scrollLeft,
                  clientWidth: event.currentTarget.clientWidth,
                })}
              >
                {timelineContent}
              </div>
            </div>
            <div className="flex flex-wrap gap-x-3 gap-y-1 border-t border-border pt-2 text-[11px] text-muted-foreground">
              <TimelineLegend color="bg-foreground/20">録れた</TimelineLegend>
              <TimelineLegend color="bg-tally">録画中</TimelineLegend>
              <TimelineLegend color="border border-foreground/40 bg-card">予約</TimelineLegend>
              <TimelineLegend color="border border-destructive bg-destructive/10">失敗</TimelineLegend>
              <TimelineLegend color="border-b-[3px] border-destructive bg-foreground/20">ドロップあり</TimelineLegend>
              <TimelineLegend color="border border-dashed border-warning bg-warning/20">チューナー不足の区間</TimelineLegend>
              <span className="basis-full">段は重なりの表示で、チューナーの番号ではありません。枠の中を横にスクロールできます</span>
            </div>
          </div>
        )}
      </section>
      {!isPending && !isError && events.length > 0 && (
        <details className="min-w-0 rounded-md border border-border bg-card text-sm" data-testid="home-timeline-details">
          <summary className="min-h-6 cursor-pointer px-3 py-1 text-primary underline-offset-2 hover:underline">
            録画・予約の詳細
          </summary>
          <ul className="flex min-w-0 flex-col border-t border-border">
            {sortHomeTimelineEvents(events).map((event) => {
              const body = (
                <>
                  <span className="w-20 shrink-0 text-xs text-muted-foreground">
                    {homeTimelineDayLabel(event.startMs, nowMs)} {formatTime(new Date(event.startMs).toISOString())}
                  </span>
                  <span className="w-20 shrink-0 truncate text-xs text-muted-foreground sm:w-28">
                    {siteCount > 1 ? `${event.site} · ` : ''}{homeTimelineChannelLabel(event.channelType)}
                  </span>
                  <span className="min-w-0 flex-1 truncate">{programTitle(event.title)}</span>
                  <span
                    className={cn(
                      'shrink-0 text-xs',
                      event.kind === 'failed' ? 'text-destructive' : 'text-muted-foreground',
                    )}
                    data-testid="home-timeline-detail-kind"
                  >
                    {homeTimelineKindLabel(event.kind)}
                  </span>
                </>
              )
              const className = 'flex min-h-6 items-center gap-2 px-3 py-1 hover:bg-muted/40'
              return (
                <li key={event.key} className="border-b border-border last:border-b-0" data-testid="home-timeline-detail-row">
                  {event.href.to === '/recordings/$id' ? (
                    <Link to="/recordings/$id" params={{ id: String(event.href.id) }} className={className}>
                      {body}
                    </Link>
                  ) : (
                    <Link
                      to="/reservations/$site/$programId"
                      params={{ site: event.href.site, programId: String(event.href.programId) }}
                      className={className}
                    >
                      {body}
                    </Link>
                  )}
                </li>
              )
            })}
          </ul>
        </details>
      )}
    </>
  )
}

function TimelineLegend({ color, children }: { color: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center whitespace-nowrap">
      <i className={cn('mr-1 inline-block h-[9px] w-[14px] rounded-sm', color)} aria-hidden="true" />
      {children}
    </span>
  )
}

function OpsStorageLine({
  roots,
  finishedRecordings,
  reservations,
  encodeQueue,
  nowMs,
}: {
  roots: StorageRoot[] | undefined
  finishedRecordings: Recording[] | undefined
  reservations: Reservation[] | undefined
  encodeQueue: EncodeQueueSummary | undefined
  nowMs: number
}) {
  const media = roots === undefined ? undefined : findMediaRoot(roots)
  if (media === undefined) return null

  const averageBitrate = finishedRecordings === undefined
    ? undefined
    : estimateAverageBitrate(recentBitrateSamples(finishedRecordings))
  const upcomingSchedule = reservations === undefined
    ? undefined
    : upcomingReservationSchedule(
        reservations,
        nowMs,
        nowMs + forecastWindowDays * 24 * 60 * 60 * 1000,
      )
  const forecast = estimateStorageForecast({
    availableBytes: media.availableBytes,
    averageBitrate,
    upcomingSchedule,
    nowMs,
  })
  const projected = forecast.projectedConsumptionBytes
  const stale = isObservationStale(media.observedAt, nowMs)

  return (
    <section className="flex min-w-0 flex-wrap gap-x-4 gap-y-1 border-t border-border pt-2 text-xs text-muted-foreground" data-testid="home-ops-storage">
      <span>残り <strong className="font-semibold text-foreground">{formatBytes(media.availableBytes)}</strong></span>
      {forecast.hasEstimate && projected !== undefined && projected > 0 && (
        <span>今後 7 日の予約で <strong className="font-semibold text-foreground">約 {formatBytes(projected)}</strong></span>
      )}
      {forecast.exceedsAvailable && forecast.fullAtMs !== undefined && (
        <span>満杯まで <strong className="font-semibold text-foreground">約 {Math.max(1, Math.ceil((forecast.fullAtMs - nowMs) / (24 * 60 * 60 * 1000)))} 日</strong></span>
      )}
      {encodeQueue !== undefined && (
        <span>エンコード待ち <strong className="font-semibold text-foreground">{encodeQueue.queued}</strong></span>
      )}
      <span className={cn(stale && 'text-warning')} title={stale ? '観測ループが止まっている可能性があります' : undefined}>
        {stale ? '観測が古い' : `観測 ${formatDateTime(media.observedAt)}`}
      </span>
    </section>
  )
}

/** 「次に見る 1 本」。幅が広い画面ではサムネイルの高さを抑え、下の棚も初期画面に入れる。 */
function WatchHero({ choice }: { choice: HomeHeroChoice }) {
  const { recording, kind } = choice
  const detail = {
    to: '/recordings/$id' as const,
    params: { id: String(recording.id) },
    hash: recording.status === 'recording' ? ('chase' as const) : undefined,
  }
  const resumePosition = recording.resumePositionMs
  const progress =
    resumePosition !== undefined && recording.durationMs > 0
      ? Math.max(0, Math.min(100, (resumePosition / recording.durationMs) * 100))
      : undefined

  return (
    <section aria-label="次に見る 1 本" className="flex min-w-0 flex-col items-start gap-3 md:flex-row md:gap-5">
      <HomeThumbnail recording={recording} hero progress={progress} />
      <div className="flex w-full min-w-0 flex-col gap-1 md:flex-1">
        <p className="text-xs text-muted-foreground">次に見る · {kind === 'continue' ? '続きから' : '新着'}</p>
        <h2 className="text-lg leading-snug font-semibold text-balance md:text-xl">
          {programTitle(recording.title)}
        </h2>
        <p className="text-xs text-muted-foreground">
          {formatDate(recording.startAt)} {formatTime(recording.startAt)} · {recording.serviceName}
          {resumePosition !== undefined &&
            ` · ${formatPlaybackPosition(resumePosition)} / ${formatPlaybackPosition(recording.durationMs)}`}
        </p>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Link
            {...detail}
            data-testid="home-primary-action"
            className="inline-flex min-h-10 items-center justify-center rounded border border-primary bg-primary px-3 text-sm font-medium text-primary-foreground"
          >
            <span aria-hidden="true" className="mr-1">
              ▶
            </span>
            {kind === 'continue' ? '続きから再生' : '再生'}
          </Link>
          {kind === 'continue' && (
            <Link
              {...detail}
              search={{ fromBeginning: true }}
              className="inline-flex min-h-9 items-center justify-center rounded border border-border bg-card px-3 text-sm hover:bg-muted"
            >
              最初から
            </Link>
          )}
        </div>
      </div>
    </section>
  )
}

/** 画面幅に入る列数を観測し、「ほかの新着」を 1 行分だけ表示する。 */
function HomeNewArrivals({ recordings }: { recordings: Recording[] }) {
  const [columns, setColumns] = useState(3)
  const listRef = useRef<HTMLUListElement>(null)

  useLayoutEffect(() => {
    const list = listRef.current
    if (!list) return

    const measure = () => {
      const width = list.getBoundingClientRect().width
      const gap = Number.parseFloat(getComputedStyle(list).columnGap) || HOME_ARRIVAL_CARD_GAP_PX
      // phone は 3 枚。広い画面では 176px カードの列数に合わせる。
      const next = width < 640
        ? 3
        : Math.max(
            3,
            Math.floor((width + gap) / (HOME_ARRIVAL_CARD_MIN_WIDTH_PX + gap)),
          )
      setColumns((current) => (current === next ? current : next))
    }

    const observer = new ResizeObserver(measure)
    observer.observe(list)
    measure()
    return () => observer.disconnect()
  }, [])

  return (
    <section aria-labelledby="home-new-arrivals" className="flex flex-col gap-2">
      <div className="flex items-baseline justify-between gap-2">
        <h2 id="home-new-arrivals" className="text-sm font-semibold">
          ほかの新着
        </h2>
        <Link
          to="/series"
          className="shrink-0 text-xs text-muted-foreground underline-offset-2 hover:underline"
        >
          すべてのシリーズ →
        </Link>
      </div>
      <ul
        ref={listRef}
        data-testid="home-new-arrivals-grid"
        data-column-count={columns}
        style={{ gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }}
        className="grid gap-2 sm:gap-3"
      >
        {recordings.slice(0, columns).map((recording) => (
          <li key={recording.id} className="min-w-0">
            <Link
              to="/recordings/$id"
              params={{ id: String(recording.id) }}
              hash={recording.status === 'recording' ? 'chase' : undefined}
              className="flex min-w-0 flex-col gap-1.5"
            >
              <HomeThumbnail recording={recording} />
              <span className="truncate text-xs text-muted-foreground">
                {programTitle(recording.title)}
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  )
}

function formatPlaybackPosition(milliseconds: number): string {
  const seconds = Math.max(0, Math.floor(milliseconds / 1000))
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  const paddedMinutes = String(minutes).padStart(2, '0')
  const paddedSeconds = String(remainder).padStart(2, '0')
  return hours > 0
    ? `${hours}:${paddedMinutes}:${paddedSeconds}`
    : `${paddedMinutes}:${paddedSeconds}`
}

function HomeThumbnail({
  recording,
  hero = false,
  progress,
}: {
  recording: Recording
  hero?: boolean
  progress?: number
}) {
  const [failed, setFailed] = useState(false)
  return (
    <div
      data-testid={hero ? 'home-next-watch-thumbnail' : undefined}
      className={cn(
        'relative aspect-video w-full min-w-0 overflow-hidden rounded border border-border bg-muted',
        // calc の 25rem は映像の外に積む縦の予算: ページ見出し・本文の上下余白・局名/番組名/時刻の 3 行。
        // 映像と 3 行が初期 viewport に収まることは e2e/design.mjs が測る
        hero &&
          'md:flex-[1.7_1_0%] md:max-w-[clamp(24rem,calc((100dvh-25rem)*16/9),64rem)]',
      )}
    >
      {!failed ? (
        <img
          src={`/api/media/recordings/${recording.id}/thumbnail`}
          alt=""
          loading={hero ? 'eager' : 'lazy'}
          className="size-full object-cover"
          onError={() => setFailed(true)}
        />
      ) : (
        <div className="size-full bg-muted" aria-hidden />
      )}
      {hero && <ThumbnailOverlay serviceName={recording.serviceName} progress={progress} />}
    </div>
  )
}

/** ブレーカーは見る側にも影響するため、警告一覧を複製せず帯で知らせる。 */
function WatchBreakerBand({ breakers }: { breakers: readonly CircuitBreaker[] }) {
  return (
    <div
      role="alert"
      data-testid="home-watch-breaker-band"
      className="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-t border-destructive/30 bg-destructive/10 px-4 py-2 text-xs"
    >
      {breakers.map((breaker) => (
        <Fragment key={`${breaker.site}:${breaker.name}`}>
          <span className="font-semibold text-destructive max-md:basis-full">
            {describeBreakerName(breaker.name)}が停止中
          </span>
          {describeBreakerReason(breaker.name) && (
            <span className="min-w-0 flex-1 text-foreground">
              {describeBreakerReason(breaker.name)}
            </span>
          )}
        </Fragment>
      ))}
      <Link
        to="/"
        search={{ mode: 'ops' }}
        onClick={() => saveHomeModePreference('ops')}
        className="ml-auto shrink-0 text-foreground underline underline-offset-2"
      >
        管理で見る
      </Link>
    </div>
  )
}

/** 録画中の件数と追っかけ導線だけを一行に畳む。 */
function RecordingStrip({ recordings }: { recordings: readonly Recording[] }) {
  const first = recordings[0]
  if (first === undefined) return null
  const titles = recordings.map((recording) => programTitle(recording.title)).join(' · ')
  return (
    <section
      aria-label="録画中"
      data-testid="home-recording-strip"
      className="flex min-w-0 items-center gap-2 border-t border-border pt-3 text-xs"
    >
      <span className="inline-flex shrink-0 items-center gap-1 rounded bg-tally px-1.5 py-0.5 font-medium text-tally-foreground whitespace-nowrap">
        <span aria-hidden="true">●</span> 録画中 {recordings.length}
      </span>
      <span className="min-w-0 flex-1 truncate text-muted-foreground">{titles}</span>
      <Link
        to="/recordings/$id"
        params={{ id: String(first.id) }}
        hash="chase"
        className="shrink-0 text-primary underline-offset-2 hover:underline"
      >
        追っかけ再生 →
      </Link>
    </section>
  )
}

/** WarningKind は「要対応」の項目の種別。表示色と、色を選ぶ判断の両方をこれ 1 つに一本化する。 */
type WarningKind = 'breaker' | 'overage' | 'drop' | 'failed' | 'cm-detection'

/**
 * WarningItem は「要対応」の 1 件（サーキットブレーカー / 失敗録画 / チューナー不足 /
 * ドロップ / CM 検出失敗）。行は種別チップ + 太字のタイトル + 副行の形で描く。
 */
type WarningItem = {
  key: string
  /**
   * 表示色・将来の出し分けの判断はすべてこの値を経由させる（文字列の `key` を
   * 前方一致で覗いて種別を推測する実装は、`key` の書式を変えただけで表示が
   * 黙って壊れる。レビュー指摘で `buildWarnings` が組む時点の種別をそのまま
   * 持たせる形に直した）。
   */
  kind: WarningKind
  /** 種別チップの語（録画失敗 / チューナー不足 / ドロップ N など）。 */
  chip: string
  /** 太字のタイトル。 */
  title: string
  /** 副行。チューナー不足では、種別が詰まった区間に重なる予約名（予約取得が未解決/失敗なら省略）。 */
  detail?: string
  /** 遷移先。サーキットブレーカーは対応する専用画面が無い（`CircuitBreakerBanner` が同じページの上部で扱う）ので省略。 */
  link?:
    | { to: '/programs'; search: { at: number } }
    | { to: '/recordings/$id'; id: number }
    | { to: '/cm-logos'; search: { network: number; service: number } }
}

/**
 * buildWarnings はサーキットブレーカー・失敗録画・容量超過・完了録画の結果から
 * 「要対応」の項目を組む。新しい API は作らず、既存の取得結果だけを材料にする。
 *
 * `finishedCandidates` は `limit=DROP_WARNING_SCAN_LIMIT` で取った完了録画の全件。
 * ドロップと CM 検出失敗に同じ応答を使う。`failedRecordings` は呼び出し元が
 * `FAILED_RECORDING_WARNING_WINDOW_MS` の recency 窓へ絞った失敗録画。どちらも時間軸の
 * 窓とは独立（窓の外の失敗・ドロップも出る）。
 * 容量超過も呼び出し元で `endAt > now` に絞り済みなので、ここでは時間フィルタをしない。
 */
function buildWarnings({
  breakers,
  overages,
  finishedCandidates,
  failedRecordings,
  reservations,
  nowMs,
}: {
  breakers: readonly CircuitBreaker[]
  overages: readonly CapacityOverage[]
  finishedCandidates: readonly Recording[]
  failedRecordings: readonly Recording[]
  reservations: readonly Reservation[] | undefined
  nowMs: number
}): WarningItem[] {
  const items: WarningItem[] = []

  for (const breaker of breakers) {
    items.push({
      key: `breaker:${breaker.site}:${breaker.name}`,
      kind: 'breaker',
      chip: '停止中',
      title: describeBreakerName(breaker.name),
      detail: `保留 ${breaker.pending} 件`,
    })
  }

  for (const recording of failedRecordings) {
    const reason = failureReasonText(recording)
    // 材料が無い（undefined）ときは理由そのものが言えていないので、「理由:」
    // というラベルを付けない（付けると「理由: 理由不明」という二重表現になる）。
    const reasonSegment = reason === undefined ? '理由不明' : `理由: ${reason}`
    items.push({
      key: `failed:${recording.id}`,
      kind: 'failed',
      chip: '録画失敗',
      title: programTitle(recording.title),
      detail: `${warningStartText(recording, nowMs)} · ${failedDurationText(recording)} / ${reasonSegment}`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  for (const overage of overages) {
    const startMs = new Date(overage.startAt).getTime()
    const endMs = new Date(overage.endAt).getTime()
    const startsAt = new Date(startMs)
    const today = new Date(nowMs)
    const tomorrow = new Date(nowMs)
    tomorrow.setDate(tomorrow.getDate() + 1)
    const scope = startsAt.toDateString() === today.toDateString()
      ? startsAt.getHours() >= 18 ? '今夜 ' : '今日 '
      : startsAt.toDateString() === tomorrow.toDateString()
        ? '明日 '
        : `${startsAt.getMonth() + 1}/${startsAt.getDate()} `
    const types = overage.jammedTypes.map(homeTimelineChannelLabel).join('・')
    const overlappingReservations = reservations?.filter((reservation) => {
      // 詰まっていない種別（GR だけの超過に重なる BS の予約など）は不足と無関係。
      if (
        reservation.skip ||
        reservation.site !== overage.site ||
        !overage.jammedTypes.includes(reservation.channelType)
      ) {
        return false
      }
      const reservationStart = new Date(reservation.startAt).getTime()
      const reservationEnd = reservationStart + reservation.durationMs
      return reservationStart < endMs && reservationEnd > startMs
    })
    items.push({
      key: `overage:${overage.site}:${overage.startAt}:${overage.endAt}`,
      kind: 'overage',
      chip: 'チューナー不足',
      title: `${scope}${formatTime(overage.startAt)}–${formatTime(overage.endAt)} ${types}が ${overage.shortfall} 本不足しています`,
      detail: overlappingReservations === undefined
        ? undefined
        : `この時間帯の予約: ${overlappingReservations.length > 0
            ? overlappingReservations.map((reservation) => programTitle(reservation.title)).join(' · ')
            : '該当なし'}`,
      link: { to: '/programs', search: { at: new Date(overage.startAt).getTime() } },
    })
  }

  for (const recording of finishedCandidates) {
    const summary = recording.dropSummary
    if (summary === undefined) continue
    if (summary.drops === 0 && summary.errors === 0 && summary.scrambled === 0) continue
    const chip = [
      { label: 'ドロップ', value: summary.drops },
      { label: 'エラー', value: summary.errors },
      { label: 'スクランブル', value: summary.scrambled },
    ]
      .filter((b) => b.value > 0)
      .map((b) => `${b.label} ${b.value.toLocaleString()}`)
      .join(' / ')
    items.push({
      key: `drop:${recording.id}`,
      kind: 'drop',
      chip,
      title: programTitle(recording.title),
      detail: warningStartText(recording, nowMs),
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  // CM 検出失敗のうち logo / area は局のロゴ設定を直すと解消するため、局単位にまとめる。
  // それ以外の段階は個別の録画詳細が操作先なので録画ごとに出す。エラー詳細は原因を
  // 推測させないため警告には載せない。
  const stationFailures = new Map<
    string,
    { networkId: number; serviceId: number; serviceName: string; count: number }
  >()
  const recordingFailures: WarningItem[] = []
  for (const recording of finishedCandidates) {
    const detection = recording.cmDetection
    if (detection.state !== 'failed') continue

    if (detection.stage === 'logo' || detection.stage === 'area') {
      const stationKey = `${recording.networkId}:${recording.serviceId}`
      const station = stationFailures.get(stationKey)
      if (station === undefined) {
        stationFailures.set(stationKey, {
          networkId: recording.networkId,
          serviceId: recording.serviceId,
          serviceName: recording.serviceName,
          count: 1,
        })
      } else {
        station.count += 1
      }
      continue
    }

    recordingFailures.push({
      key: `cm-detection:recording:${recording.id}`,
      kind: 'cm-detection',
      chip: 'CM 検出失敗',
      title: programTitle(recording.title),
      detail: `1 件 · ${warningStartText(recording, nowMs)}`,
      link: { to: '/recordings/$id', id: recording.id },
    })
  }

  for (const [stationKey, station] of stationFailures) {
    items.push({
      key: `cm-detection:station:${stationKey}`,
      kind: 'cm-detection',
      chip: 'CM 検出失敗',
      title: `${station.serviceName} ${station.count} 件`,
      link: {
        to: '/cm-logos',
        search: { network: station.networkId, service: station.serviceId },
      },
    })
  }
  items.push(...recordingFailures)

  return items
}

/** warningStartText は録画の警告の副行に載せる「今日 17:00 · Eテレ」。 */
function warningStartText(recording: Recording, nowMs: number): string {
  const startMs = new Date(recording.startAt).getTime()
  return `${homeTimelineDayLabel(startMs, nowMs)} ${formatTime(recording.startAt)} · ${recording.serviceName}`
}

/**
 * failedDurationText は失敗録画の警告メッセージに載せる尺の文言。
 *
 * **予定尺（`durationMs`。番組の放送尺のスナップショット）と実際に録れた尺
 * （`startedAt`〜`endedAt`）を区別する**（issue #301）。区別しないと、録画が
 * 開始した直後に終わった失敗（実際は 0 分に近いのに `durationMs` は番組の
 * 予定尺のまま）が「ほぼ予定通り録れた」ように見えてしまう。
 *
 * `startedAt` と `endedAt` は独立に書かれるので、`startedAt` だけが立って
 * `endedAt` が無い行がある（レビューで発覚。以前のコメントは「両方揃ってから
 * 書く」と逆を断言していた）。`UpdateRecordingStatus`
 * （`internal/db/queries/recordings.sql`）は `started_at` を無条件に
 * `COALESCE` で埋め、`ended_at` は渡された値が非 NULL のときだけ書く。呼び
 * 出し元の `Watcher.updateRecordingStatus`（`internal/watcher/watcher.go`）は
 * `record.Recording.EndTime`（`*mirakc.Milliseconds` で nil を取りうる）を
 * そのまま渡すので、mirakc の failed record に `endTime` が無ければ failed
 * 行でも `startedAt` だけが立つ。したがって 3 通りを区別する: 両方あり
 * （実際尺が定義できる）/ `startedAt` のみ（開始した事実はあるが終了時刻が無く、
 * 実際尺は主張できない）/ 両方無し（**rokuban が録画の開始を観測していない**。
 * 「未開始」と出す）。
 *
 * 3 つ目で mirakc 側の事実（「mirakc が録画を開始しなかった」）は主張しない
 * （レビュー指摘）。`started_at` を書くのは record を観測した
 * `Watcher.updateRecordingStatus` だけで、`CreateFailedRecording`
 * （`internal/db/queries/recordings.sql`）は書かないので、**record の観測より
 * 先に `recording.failed` の SSE が届いた**窓でも両方無しの形になる。データが
 * 支えているのは「rokuban が開始を観測していない」までで、その先は測っていない。
 *
 * 3 通りの中の区切りはどれも `・` に揃える（レビュー指摘）。警告メッセージ全体が
 * `（尺 / 理由: …）` の形で ` / ` を「尺と理由の境目」に使っているので、尺の中でも
 * ` / ` を使うと 1 行に 2 種類の意味のスラッシュが並ぶ。
 */
function failedDurationText(recording: Recording): string {
  if (recording.startedAt && recording.endedAt) {
    const actualMs = new Date(recording.endedAt).getTime() - new Date(recording.startedAt).getTime()
    return `実際 ${formatDuration(actualMs)}・予定 ${formatDuration(recording.durationMs)}`
  }
  if (recording.startedAt) {
    return `予定 ${formatDuration(recording.durationMs)}・開始のみ記録（終了未記録）`
  }
  return `予定 ${formatDuration(recording.durationMs)}・未開始`
}

/**
 * failureReasonText は失敗録画の理由を `qualityEvents` から取り出す。
 *
 * **材料が無ければ `undefined` を返し、沈黙（理由が言えない）と実際の理由文を
 * 型で区別する**（issue #301 / #454）。呼び出し側はこれを見て「理由:」という
 * ラベルを付けるかどうかを決める --- 文字列の中身（`'理由不明'`）で判定すると、
 * 将来この語を言い換えたときに呼び出し側の分岐が黙って外れる。
 * `qualityEvents` は追記専用の履歴（`recordings.quality_events`。
 * `docs/schema/recordings.md` §5）で `recording.failed` /
 * `recording.record-broken` / `bcas_anomaly` が混ざるので、**最後の要素では
 * なく失敗系イベントの最後の要素**を見る（末尾が `bcas_anomaly` だと最後の
 * 失敗理由を読み飛ばす）。
 *
 * `reason` の形は書き手（`event` の値）で決まり、いずれもオブジェクトで
 * 素の文字列を書く経路は無い（以前のコメントは「`recording.failed` は文字列」
 * と書いていたが、書き手を辿ると逆でどちらもオブジェクト）:
 * - `recording.failed`: `internal/watcher/watcher.go` の
 *   `handleRecordingFailed` が `json.Marshal(data.Reason)` で書く。
 *   `data.Reason` は `mirakc.FailedReason`
 *   （`internal/mirakc/types.go`。discriminated union で `type` フィールドを
 *   持つ）なので `reason.type` を読む。
 * - `recording.record-broken`: 同ファイルの `handleRecordBroken` が
 *   `map[string]string{"reason": data.Reason}` で書くので `reason.reason`
 *   を読む。
 *
 * 上記どちらでもない `event`、または期待した形（`type` / `reason` フィールド
 * が無い）は `components/recording-detail-panel.tsx` の「品質イベント」欄と
 * 同じ流儀（`JSON.stringify`）で読める形にフォールバックする。
 *
 * **読んだフィールドが空文字なら `undefined` に寄せる**（レビュー指摘）。
 * `mirakc.FailedReason.Type` に `omitempty` は無いので `{"type":""}` は
 * あり得る形だが、それを `JSON.stringify` でそのまま出すと「理由: {"type":""}」に
 * なり、材料が無い（沈黙）ことと区別できる文言にならない。
 */
function failureReasonText(recording: Recording): string | undefined {
  const events = recording.qualityEvents
  if (events === undefined || events.length === 0) return undefined
  const failureEvent = events.findLast(
    (e) => e['event'] === 'recording.failed' || e['event'] === 'recording.record-broken',
  )
  if (failureEvent === undefined) return undefined
  const reason = failureEvent['reason']
  if (reason === undefined || reason === null) return undefined

  if (typeof reason === 'object' && !Array.isArray(reason)) {
    const record = reason as Record<string, unknown>
    // 上の findLast が `event` を 2 種類に絞っているので、`recording.failed`
    // でなければ `recording.record-broken`。
    const field = failureEvent['event'] === 'recording.failed' ? 'type' : 'reason'
    const value = record[field]
    if (typeof value === 'string') return value === '' ? undefined : value
  }
  return JSON.stringify(reason)
}

/**
 * WarningRow は「要対応」の 1 件。種別チップ + 太字のタイトル + 副行。
 * 色はチップだけが持つ: サーキットブレーカー・直近のドロップ・失敗録画・CM 検出失敗は
 * destructive、チューナー不足は容量バッジ（`components/capacity-shortfall-badge.tsx`）と同じ
 * warning（琥珀）
 * （docs/frontend/design.md「色は信号のみ」。同じ事実は同じ色で言う）。
 * 種別 × 色は `pages/home.test.tsx`「警告項目は種別ごとに固定の色クラスを持つ」と
 * `e2e/design.mjs` ①'' が固定する。
 */
function WarningRow({ warning, divider }: { warning: WarningItem; divider: boolean }) {
  const content = (
    <span className="grid min-w-0 grid-cols-[6.5rem_minmax(0,1fr)] items-start gap-x-3">
      <span
        className={cn(
          'w-fit max-w-full rounded-sm px-2 py-0.5 text-xs font-semibold',
          warning.kind === 'overage'
            ? 'bg-warning/15 text-warning'
            : 'bg-destructive/10 text-destructive',
        )}
        data-testid="warning-chip"
      >
        {warning.chip}
      </span>
      <span className="min-w-0">
        <span className="block font-semibold" data-testid="warning-title">{warning.title}</span>
        {warning.detail !== undefined && (
          <span className="mt-0.5 block text-xs text-muted-foreground">{warning.detail}</span>
        )}
      </span>
    </span>
  )
  const rowClassName = 'flex min-h-10 items-center px-3 py-2.5 text-sm text-foreground'
  const itemClassName = cn('min-w-0', divider && 'border-t border-border')

  if (warning.link === undefined) {
    return <li data-warning-kind={warning.kind} className={cn(itemClassName, rowClassName)}>{content}</li>
  }

  if (warning.link.to === '/programs') {
    return (
      <li data-warning-kind={warning.kind} className={itemClassName}>
        <Link
          to="/programs"
          search={warning.link.search}
          className={cn(rowClassName, 'hover:bg-muted/40')}
        >
          {content}
        </Link>
      </li>
    )
  }

  if (warning.link.to === '/cm-logos') {
    return (
      <li data-warning-kind={warning.kind} className={itemClassName}>
        <Link
          to="/cm-logos"
          search={warning.link.search}
          className={cn(rowClassName, 'hover:bg-muted/40')}
        >
          {content}
        </Link>
      </li>
    )
  }

  return (
    <li data-warning-kind={warning.kind} className={itemClassName}>
      <Link
        to="/recordings/$id"
        params={{ id: String(warning.link.id) }}
        className={cn(rowClassName, 'hover:bg-muted/40')}
      >
        {content}
      </Link>
    </li>
  )
}
