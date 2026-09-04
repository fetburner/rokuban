import { Link } from '@tanstack/react-router'

import { useListRules, useListSites, type Recording } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { DropStatsTable } from '@/components/drop-stats-table'
import { RecordingActions } from '@/components/recording-actions'
import { RecordingPlayer } from '@/components/recording-player'
import { formatBytes, formatDateTime } from '@/lib/format'
import { ingestDisplay, type IngestDisplay } from '@/lib/ingest'
import { sourceLabels } from '@/lib/recording-search'

function ingestDetailText(display: IngestDisplay): string {
  switch (display.kind) {
    case 'pending':
      return '待機中（まだ原本を取り込んでいません）'
    case 'originalDeleted':
      return '完了（原本は削除済み）'
    case 'transferring': {
      const size =
        display.expectedBytes !== undefined
          ? `${formatBytes(display.writtenBytes)} / ${formatBytes(display.expectedBytes)}`
          : formatBytes(display.writtenBytes)
      const percent = display.percent !== undefined ? `（${display.percent}%）` : ''
      return `${display.stale ? '転送中・停滞' : '転送中'} ${size}${percent}`
    }
  }
}

function shouldShowRecordingSite(
  registeredSites: readonly string[],
  recordingSites: readonly string[],
) {
  return new Set([...registeredSites, ...recordingSites]).size > 1
}

export function RecordingDetail({ recording, trash }: { recording: Recording; trash: boolean }) {
  const encodedAssets = recording.encodedAssets ?? []
  const hasOriginal = recording.sizeBytes !== undefined
  const ingestState = ingestDisplay(recording, Date.now())
  const registeredSites = unwrap(useListSites().data) ?? []
  const showSite = shouldShowRecordingSite(registeredSites, [recording.site])

  return (
    <div className="flex flex-col gap-4 bg-muted/30 px-4 py-3 text-xs">
      {!trash && (encodedAssets.length > 0 || hasOriginal) && (
        <RecordingPlayer
          recordingId={recording.id}
          encodedAssets={encodedAssets}
          hasOriginal={hasOriginal}
          originalSizeBytes={recording.sizeBytes}
        />
      )}
      {recording.description && (
        <p className="whitespace-pre-wrap text-muted-foreground">{recording.description}</p>
      )}
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
        <dt className="text-muted-foreground">チャンネル</dt>
        <dd>
          {recording.serviceName} ({recording.channelType}/{recording.channel})
          {showSite ? ` · ${recording.site}` : ''}
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
        <dd>{sourceLabels[recording.source]}</dd>
        {ingestState !== undefined && (
          <>
            <dt className="text-muted-foreground">取り込み</dt>
            <dd>{ingestDetailText(ingestState)}</dd>
          </>
        )}
        {trash && recording.deletedAt && (
          <>
            <dt className="text-muted-foreground">削除日時</dt>
            <dd>{formatDateTime(recording.deletedAt)}</dd>
          </>
        )}
      </dl>
      {recording.ruleId !== undefined && <RuleSection ruleId={recording.ruleId} />}
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
      {recording.dropSummary && <DropStatsTable recordingId={recording.id} />}
      <RecordingActions recording={recording} trash={trash} />
    </div>
  )
}

function RuleSection({ ruleId }: { ruleId: number }) {
  const query = useListRules()
  const rules = unwrap(query.data) ?? []
  const rule = rules.find((r) => r.id === ruleId)
  const label = rule?.name ?? `#${ruleId}`

  return (
    <section>
      <h4 className="mb-1 font-medium">ルール</h4>
      <div className="flex flex-wrap items-center gap-3">
        <Link
          to="/search"
          search={{ ruleId }}
          className="text-primary underline-offset-2 hover:underline"
        >
          {label}
        </Link>
        <Link
          to="/recordings"
          search={{ ruleId }}
          className="text-muted-foreground underline-offset-2 hover:underline"
        >
          このルールの録画で絞る
        </Link>
      </div>
    </section>
  )
}
