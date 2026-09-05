import { useMemo } from 'react'

import { type DropSummary, type Recording } from '@/api/generated'
import { encodeJobStatusLabel } from '@/lib/encode-status'
import { useEncodeProgress } from '@/lib/events'
import { formatBytes } from '@/lib/format'
import { ingestDisplay } from '@/lib/ingest'
import { statusLabels } from '@/lib/recording-search'
import { cn } from '@/lib/utils'

/**
 * StatusBadge は録画の状態。**録画中だけがタリーレッドの塗り**になる
 * （docs/frontend/design.md「色は信号のみ」）。
 *
 * 赤は 2 つの意味に使うが、色相ではなく**形で分ける**: タリーは「点灯」なので
 * 塗り（`bg-tally` + 紙白の文字）、destructive は「取り返しがつかない」なので
 * 文字と淡い地（`text-destructive` + `bg-destructive/10`）。同じ赤でも、
 * 塗られているかどうかで「いま電波に乗っている」と「壊れた」を見分けられる。
 *
 * `finished` の文字色は `text-foreground`（bg-muted 小バッジの合成後コントラスト
 * 対策。docs/frontend/design.md「コントラストは毎回測る」）。foreground は色では
 * なく地の無彩 3 値の一部なので「色は信号のみ」は破っていない。
 *
 * この描き分けと `text-foreground` のコントラストは `web/e2e/design.mjs` が
 * 実測している対象（`IngestBadge` と同じフィクスチャで測る）。ここを変えると
 * 理由が分からないまま e2e が落ちる。
 */
export function StatusBadge({ status }: { status: Recording['status'] }) {
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-xs',
        status === 'failed' && 'bg-destructive/10 text-destructive',
        status === 'recording' && 'bg-tally font-medium text-tally-foreground',
        status === 'finished' && 'bg-muted text-foreground',
      )}
    >
      {statusLabels[status]}
    </span>
  )
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
