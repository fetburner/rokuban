import { useMemo } from 'react'

import { type DropSummary, type Recording } from '@/api/generated'
import type { LiveCapability } from '@/lib/capabilities'
import { encodeJobStatusLabel } from '@/lib/encode-status'
import { useEncodeProgress } from '@/lib/events'
import { formatBytes } from '@/lib/format'
import { ingestDisplay } from '@/lib/ingest'
import { recordingVerdict, type RecordingVerdict } from '@/lib/recording-verdict'
import { cn } from '@/lib/utils'

/**
 * RecordingVerdictBadge は「見られるか」の結論を出す。視聴可能な録画は無印。
 *
 * 「要対応」のように結論と処理内訳を混ぜる語は使わない。見られる録画の
 * エンコード失敗やドロップは、後続の内訳バッジが destructive で示す。
 *
 * `pending` / `unknown` capability を `false` に潰さず、原本だけの録画を
 * 誤って「再生不可」としないため、useLiveCapability の 4 値を使う。
 */
export function RecordingVerdictBadge({
  recording,
  liveCapability,
  isTrashed = false,
  onClick,
}: {
  recording: Recording
  liveCapability: LiveCapability
  isTrashed?: boolean
  /** 詳細ヘッダーでは結論から記録タブへ移動する。 */
  onClick?: () => void
}) {
  // IngestBadge と同じくレンダー時点の観測時刻を渡す。結論は transferring の
  // stale 有無で変わらず、再描画時には内訳と同じ観測を使う。
  // oxlint-disable-next-line react/purity -- status snapshot follows IngestBadge's render-time clock
  const verdict = recordingVerdict({ recording, liveCapability, isTrashed, nowMs: Date.now() })
  if (verdict === undefined || verdict === 'viewable') return null

  const badge = (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        verdict === 'failed' && 'bg-destructive/10 text-destructive',
        verdict === 'recording' && 'bg-tally font-medium text-tally-foreground',
        (verdict === 'preparing' || verdict === 'unavailable') && 'bg-muted text-foreground',
      )}
    >
      {verdictLabels[verdict]}
    </span>
  )

  if (onClick === undefined) return badge
  return (
    <button
      type="button"
      className="inline-flex min-h-6 items-center"
      aria-label="録画状態を記録タブで見る"
      onClick={onClick}
    >
      {badge}
    </button>
  )
}

const verdictLabels: Record<Exclude<RecordingVerdict, 'viewable'>, string> = {
  recording: '録画中',
  failed: '録画失敗',
  preparing: '準備中',
  unavailable: '再生不可',
}

/**
 * IngestBadge は「原本をまだ取り込めていない」ことを一覧の行に出す（issue #212）。
 *
 * `status = finished` は mirakc の録画完了であって取り込み完了ではない。原本が
 * コミットされるまでブラウザ再生も事後エンコードもできないが、それを表すものが
 * `sizeBytes` の省略しか無かったため「止まっているのか進んでいるのか」が
 * 分からなかった。
 *
 * **色は使わない**（`bg-muted` のまま）。停滞も含めて状況の説明であって、
 * タリー（いま電波に乗っている）でも destructive（取り返しがつかない）でも
 * ない --- 「色は信号のみ」（docs/frontend/design.md）に従い、停滞は文言で
 * 言う。文字色は `text-foreground`（bg-muted 小バッジの合成後コントラスト対策。
 * 同 doc「コントラストは毎回測る」。foreground は地の無彩 3 値の一部で色では
 * ないので、この方針とは矛盾しない）。
 *
 * `originalDeleted`（取り込み済みだが原本が今は無い）はここには出さない ---
 * 一覧の 1 行に常時出す種類の情報ではなく、詳細ページの「取り込み」欄
 * （`RecordingDetail`）が引き受ける。
 *
 * 停滞判定に使う「今」はレンダリング時の `Date.now()`。時計そのものを刻んでは
 * いないが、取り込み中の録画がある間は一覧が定期再取得され（`refetchInterval`）
 * そのたびに再レンダリングされるので、進捗が止まっていれば数十秒のうちに
 * 「停滞」へ変わる。
 */
export function IngestBadge({ recording }: { recording: Recording }) {
  // 取り込み中の一覧は refetchInterval で再描画される。時刻を state に固定すると
  // 停滞判定が更新されなくなるため、各レンダーの観測時刻を使う意図的な例外。
  // oxlint-disable-next-line react/purity -- refetch ごとの現在時刻スナップショットが必要
  const display = ingestDisplay(recording, Date.now())
  if (display === undefined || display.kind === 'originalDeleted') return null

  const label =
    display.kind === 'pending'
      ? '取り込み待ち'
      : display.percent !== undefined
        ? `取り込み中 ${display.percent}%`
        : `取り込み中 ${formatBytes(display.writtenBytes)}`

  return (
    <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
      {display.kind === 'transferring' && display.stale ? `${label}（停滞）` : label}
    </span>
  )
}

/**
 * DropBadges はドロップ統計をひと目で分かる形で出す。
 * 0 のものは出さないので、正常な録画ではバッジが 1 つも出ない。
 */
export function DropBadges({ summary }: { summary: DropSummary }) {
  const badges = [
    { label: 'ドロップ', value: summary.drops },
    { label: 'エラー', value: summary.errors },
    { label: 'スクランブル', value: summary.scrambled },
  ].filter((b) => b.value > 0)

  if (badges.length === 0) return null

  return (
    <>
      {badges.map((b) => (
        <span
          key={b.label}
          className="shrink-0 rounded bg-destructive/10 px-1.5 py-0.5 text-xs text-destructive"
        >
          {b.label} {b.value.toLocaleString()}
        </span>
      ))}
    </>
  )
}

/**
 * EncodeStatusBadges は完了していないエンコードプロファイルの試行状態を出す
 * （issue #316。`Recording.encodeStatus`）。プロファイルを設定していない録画・
 * 全プロファイルが完了済みの録画では `encodeStatus` が省略され、このコンポーネント
 * は何も出さない --- 機能しないキュー画面や空の進捗バーを出さない判断
 * （docs/frontend/recordings.md）はサーバー側の省略で表現されており、ここは
 * それをそのまま描くだけ。
 *
 * `failed` だけ destructive（`DropBadges` と同じ判断: 実害があるので色で
 * 目立たせる）。`queued` / `running` は `IngestBadge` と同じ `bg-muted`
 * （状況の説明であって信号ではない。docs/frontend/design.md「色は信号のみ」）。
 *
 * プロファイル名を前置するのは、事後追加（issue #133）で複数プロファイルを
 * 依頼した録画では「どのプロファイルが失敗したか」が言えないと運用判断に
 * 使えないため（ドロップ統計の種別列と同じ判断）。
 */
export function EncodeStatusBadges({ recording }: { recording: Recording }) {
  const statuses = recording.encodeStatus ?? []
  const runningProfiles = useMemo(
    () =>
      (recording.encodeStatus ?? [])
        .filter((status) => status.state === 'running')
        .map((status) => status.profile),
    [recording.encodeStatus],
  )
  const progress = useEncodeProgress(recording.id, runningProfiles)

  if (statuses.length === 0) return null

  return (
    <>
      {statuses.map((s) => (
        <span
          key={s.profile}
          className={cn(
            'shrink-0 rounded px-1.5 py-0.5 text-xs',
            s.state === 'failed'
              ? 'bg-destructive/10 text-destructive'
              : 'bg-muted text-foreground',
          )}
        >
          {s.profile}:{' '}
          {encodeJobStatusLabel(s.state, s.state === 'running' ? progress.get(s.profile) : undefined)}
        </span>
      ))}
    </>
  )
}
