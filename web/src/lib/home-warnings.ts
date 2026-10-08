import {
  type CapacityOverage,
  type CircuitBreaker,
  type Recording,
  type Reservation,
} from '@/api/generated'
import { describeBreakerName } from '@/lib/breaker'
import { isStationFixableCMStage } from '@/lib/cm-detect-stage'
import { calendarDayDiff, formatDuration, formatTime, formatTimeRange } from '@/lib/format'
import { homeTimelineChannelLabel, homeTimelineDayLabel } from '@/lib/home-timeline'
import { programTitle } from '@/lib/program-labels'

/** WarningKind は「要対応」の項目の種別。表示色と、色を選ぶ判断の両方をこれ 1 つに一本化する。 */
type WarningKind = 'breaker' | 'overage' | 'drop' | 'failed' | 'not-recorded' | 'cm-detection'

/**
 * WarningItem は「要対応」の 1 件（サーキットブレーカー / 失敗録画 / 録画されず /
 * チューナー不足 / ドロップ / CM 検出失敗）。行は種別チップ + 太字のタイトル + 副行の形で描く。
 */
export type WarningItem = {
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
    | { to: '/cm-logos/$networkId/$serviceId'; networkId: number; serviceId: number }
    | { to: '/reservations/$site/$programId'; site: string; programId: number }
}

/**
 * buildWarnings はサーキットブレーカー・失敗録画・orphaned 予約・容量超過・完了録画の結果から
 * 「要対応」の項目を組む。新しい API は作らず、既存の取得結果だけを材料にする。
 *
 * `finishedCandidates` は `limit=DROP_WARNING_SCAN_LIMIT` で取った完了録画の全件。
 * ドロップと CM 検出失敗に同じ応答を使う。`failedRecordings` は呼び出し元が
 * `FAILED_RECORDING_WARNING_WINDOW_MS` の recency 窓へ絞った失敗録画。どちらも時間軸の
 * 窓とは独立（窓の外の失敗・ドロップも出る）。
 * 容量超過も呼び出し元で `endAt > now` に絞り済みなので、ここでは時間フィルタをしない。
 */
export function buildWarnings({
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

  // orphaned は放送イベント中に schedule も録画試行も観測されなかった予約。
  // 失敗録画と同じ「既に失われた」段に置き、予約詳細へ案内する。
  for (const reservation of reservations ?? []) {
    if (reservation.state !== 'orphaned') continue
    const startMs = Date.parse(reservation.startAt)
    items.push({
      key: `not-recorded:${reservation.site}:${reservation.programId}`,
      kind: 'not-recorded',
      chip: '録画されず',
      title: programTitle(reservation.title),
      detail: `${homeTimelineDayLabel(startMs, nowMs)} ${formatTime(reservation.startAt)} · ${reservation.serviceName}`,
      link: {
        to: '/reservations/$site/$programId',
        site: reservation.site,
        programId: reservation.programId,
      },
    })
  }

  for (const overage of overages) {
    const startMs = new Date(overage.startAt).getTime()
    const endMs = new Date(overage.endAt).getTime()
    const startsAt = new Date(startMs)
    const dayDifference = calendarDayDiff(startMs, nowMs)
    const scope = dayDifference === 0
      ? startsAt.getHours() >= 18 ? '今夜 ' : '今日 '
      : dayDifference === 1
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
      title: `${scope}${formatTimeRange(formatTime(overage.startAt), formatTime(overage.endAt))} ${types}が ${overage.shortfall} 本不足しています`,
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

  // 局の CM ロゴ画面で直せる段階は局単位にまとめる。それ以外の段階は個別の録画詳細が
  // 操作先なので録画ごとに出す。エラー詳細は原因を推測させないため警告には載せない。
  const stationFailures = new Map<
    string,
    { networkId: number; serviceId: number; serviceName: string; count: number }
  >()
  const recordingFailures: WarningItem[] = []
  for (const recording of finishedCandidates) {
    const detection = recording.cmDetection
    if (detection.state !== 'failed') continue

    if (isStationFixableCMStage(detection.stage)) {
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
      detail: warningStartText(recording, nowMs),
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
        to: '/cm-logos/$networkId/$serviceId',
        networkId: station.networkId,
        serviceId: station.serviceId,
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
 * いま `qualityEvents` を書くのは `recording.failed` / `recording.record-broken`
 * だけで、スクランブル数は `drop_stats.scrambled` にある。それでも末尾の要素では
 * なく、失敗系イベントの最後の要素を `findLast` で読む。ほかの種類のイベントが
 * 増えても、また版を混在させて動かしている間に古い版が別種のイベントを書き足し
 * ても、失敗理由を読み飛ばさないようにするためである。
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
 * 期待した形（`type` / `reason` フィールドが無い）でなければ、
 * `components/recording-detail-panel.tsx` の「品質イベント」欄と同じ流儀（`JSON.stringify`）で読める形にフォールバックする。
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
    const value = record[failureEvent['event'] === 'recording.failed' ? 'type' : 'reason']
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
