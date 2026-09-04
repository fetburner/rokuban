import { useMemo } from 'react'

import { type DropSummary, type Recording } from '@/api/generated'
import { encodeJobStatusLabel } from '@/lib/encode-status'
import { useEncodeProgress } from '@/lib/events'
import { formatBytes } from '@/lib/format'
import { ingestDisplay } from '@/lib/ingest'
import { statusLabels } from '@/lib/recording-search'
import { cn } from '@/lib/utils'

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

export function IngestBadge({ recording }: { recording: Recording }) {
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
