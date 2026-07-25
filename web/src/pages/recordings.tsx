import { ChevronDown } from 'lucide-react'
import { useState } from 'react'

import {
  useListRecordingDropStats,
  useListRecordings,
  type DropSummary,
  type Recording,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { cn } from '@/lib/utils'

const statusLabels: Record<Recording['status'], string> = {
  recording: '録画中',
  finished: '完了',
  failed: '失敗',
}

export function RecordingsPage() {
  const query = useListRecordings()
  const recordings = unwrap(query.data) ?? []

  return (
    <>
      <PageHeader title="録画" />

      {query.isError ? (
        <ErrorState>録画の取得に失敗しました</ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : recordings.length === 0 ? (
        <EmptyState>録画がありません</EmptyState>
      ) : (
        <ul>
          {recordings.map((r) => (
            <li key={r.id}>
              <RecordingRow recording={r} />
            </li>
          ))}
        </ul>
      )}
    </>
  )
}

function RecordingRow({ recording }: { recording: Recording }) {
  const [expanded, setExpanded] = useState(false)

  return (
    <div className="border-b border-border">
      <button
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
        className="flex min-h-14 w-full items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/50"
      >
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm">{recording.title || '（番組名なし）'}</div>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
            <StatusBadge status={recording.status} />
            <span className="shrink-0">{recording.serviceName}</span>
            <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
            <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
            {recording.sizeBytes !== undefined && (
              <span className="shrink-0">{formatBytes(recording.sizeBytes)}</span>
            )}
            {recording.dropSummary && <DropBadges summary={recording.dropSummary} />}
          </div>
        </div>
        <ChevronDown
          className={cn(
            'size-4 shrink-0 text-muted-foreground transition-transform',
            expanded && 'rotate-180',
          )}
        />
      </button>

      {expanded && <RecordingDetail recording={recording} />}
    </div>
  )
}

function StatusBadge({ status }: { status: Recording['status'] }) {
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-[0.65rem]',
        status === 'failed' && 'bg-destructive/10 text-destructive',
        status === 'recording' && 'bg-primary/10 text-primary',
        status === 'finished' && 'bg-muted text-muted-foreground',
      )}
    >
      {statusLabels[status]}
    </span>
  )
}

/**
 * DropBadges はドロップ統計をひと目で分かる形で出す。
 * 0 のものは出さないので、正常な録画ではバッジが 1 つも出ない。
 */
function DropBadges({ summary }: { summary: DropSummary }) {
  const badges = [
    { label: 'drop', value: summary.drops },
    { label: 'error', value: summary.errors },
    { label: 'scrambled', value: summary.scrambled },
  ].filter((b) => b.value > 0)

  if (badges.length === 0) return null

  return (
    <>
      {badges.map((b) => (
        <span
          key={b.label}
          className="shrink-0 rounded bg-destructive/10 px-1.5 py-0.5 text-[0.65rem] text-destructive"
        >
          {b.label} {b.value.toLocaleString()}
        </span>
      ))}
    </>
  )
}

function RecordingDetail({ recording }: { recording: Recording }) {
  return (
    <div className="flex flex-col gap-4 bg-muted/30 px-4 py-3 text-xs">
      {recording.description && (
        <p className="whitespace-pre-wrap text-muted-foreground">{recording.description}</p>
      )}

      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
        <dt className="text-muted-foreground">チャンネル</dt>
        <dd>
          {recording.serviceName} ({recording.channelType}/{recording.channel})
        </dd>
        {recording.startedAt && (
          <>
            <dt className="text-muted-foreground">録画開始</dt>
            <dd>{formatDateTime(recording.startedAt)}</dd>
          </>
        )}
        {recording.endedAt && (
          <>
            <dt className="text-muted-foreground">録画終了</dt>
            <dd>{formatDateTime(recording.endedAt)}</dd>
          </>
        )}
        <dt className="text-muted-foreground">種別</dt>
        <dd>{recording.source === 'manual' ? '手動' : 'ルール'}</dd>
      </dl>

      {recording.qualityEvents && recording.qualityEvents.length > 0 && (
        <section>
          <h4 className="mb-1 font-medium">品質イベント</h4>
          <ul className="flex flex-col gap-1 text-muted-foreground">
            {recording.qualityEvents.map((event, i) => (
              <li key={i} className="break-all">
                {String(event.event ?? 'unknown')}
                {event.reason ? `: ${JSON.stringify(event.reason)}` : ''}
              </li>
            ))}
          </ul>
        </section>
      )}

      {/* PID 別の内訳は行数が多いので、モバイルで横スクロールさせずここに畳む */}
      {recording.dropSummary && <DropStatsTable recordingId={recording.id} />}
    </div>
  )
}

function DropStatsTable({ recordingId }: { recordingId: number }) {
  const query = useListRecordingDropStats(recordingId)
  const stats = unwrap(query.data) ?? []

  if (query.isPending) {
    return <p className="text-muted-foreground">ドロップ統計を読み込み中…</p>
  }
  if (query.isError) {
    return <p className="text-destructive">ドロップ統計の取得に失敗しました</p>
  }
  if (stats.length === 0) return null

  return (
    <section>
      <h4 className="mb-1 font-medium">PID 別ドロップ統計</h4>
      <div className="grid grid-cols-[auto_1fr_1fr_1fr_1fr] gap-x-3 gap-y-0.5 tabular-nums">
        <span className="text-muted-foreground">PID</span>
        <span className="text-right text-muted-foreground">packets</span>
        <span className="text-right text-muted-foreground">drop</span>
        <span className="text-right text-muted-foreground">error</span>
        <span className="text-right text-muted-foreground">scrambled</span>
        {stats.map((s) => (
          <div key={s.pid} className="col-span-5 grid grid-cols-subgrid">
            <span>0x{s.pid.toString(16).padStart(4, '0')}</span>
            <span className="text-right">{s.packets.toLocaleString()}</span>
            <span className={cn('text-right', s.drops > 0 && 'text-destructive')}>
              {s.drops.toLocaleString()}
            </span>
            <span className={cn('text-right', s.errors > 0 && 'text-destructive')}>
              {s.errors.toLocaleString()}
            </span>
            <span className={cn('text-right', s.scrambled > 0 && 'text-destructive')}>
              {s.scrambled.toLocaleString()}
            </span>
          </div>
        ))}
      </div>
    </section>
  )
}
