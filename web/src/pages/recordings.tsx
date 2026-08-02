import { useQueryClient } from '@tanstack/react-query'
import { ChevronDown, Trash2 } from 'lucide-react'
import { useState } from 'react'

import {
  useDeleteRecording,
  useListRecordingDropStats,
  useListRecordings,
  usePurgeRecording,
  useRestoreRecording,
  type DropSummary,
  type Recording,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { RecordingPlayer } from '@/components/recording-player'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { cn } from '@/lib/utils'

const statusLabels: Record<Recording['status'], string> = {
  recording: '録画中',
  finished: '完了',
  failed: '失敗',
}

type ViewMode = 'library' | 'trash'

export function RecordingsPage() {
  const [mode, setMode] = useState<ViewMode>('library')
  const trash = mode === 'trash'
  // params を渡すと queryKey に trash が入り、ライブラリとごみ箱が別キャッシュになる
  const query = useListRecordings({ trash })
  const recordings = unwrap(query.data) ?? []

  return (
    <>
      <PageHeader title="録画">
        <div className="flex gap-1 border-t border-border px-4 py-2">
          <ViewTab
            active={mode === 'library'}
            onClick={() => setMode('library')}
            label="ライブラリ"
          />
          <ViewTab
            active={mode === 'trash'}
            onClick={() => setMode('trash')}
            label="ごみ箱"
          />
        </div>
      </PageHeader>

      {query.isError ? (
        <ErrorState>
          {trash ? 'ごみ箱の取得に失敗しました' : '録画の取得に失敗しました'}
        </ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : recordings.length === 0 ? (
        <EmptyState>{trash ? 'ごみ箱は空です' : '録画がありません'}</EmptyState>
      ) : (
        <ul>
          {recordings.map((r) => (
            <li key={r.id}>
              <RecordingRow recording={r} trash={trash} />
            </li>
          ))}
        </ul>
      )}
    </>
  )
}

function ViewTab({
  active,
  onClick,
  label,
}: {
  active: boolean
  onClick: () => void
  label: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'rounded-md px-3 py-1.5 text-xs transition-colors',
        active
          ? 'bg-muted font-medium text-foreground'
          : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
      )}
    >
      {label}
    </button>
  )
}

function RecordingRow({ recording, trash }: { recording: Recording; trash: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const [thumbFailed, setThumbFailed] = useState(false)

  return (
    <div className="border-b border-border">
      <button
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
        className="flex min-h-14 w-full items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/50"
      >
        {/*
          サムネイルは openapi 外の streamer 経路（/api/recordings/{id}/thumbnail）。
          未生成時は 404 → onError でプレースホルダ。hasThumbnail 列は持たない（M3-4）。
          ごみ箱の録画は配信側が deleted_at IS NOT NULL を 404 にする契約（docs/api.md
          §メディア配信）なので、そもそもリクエストを出さずプレースホルダ固定にする
          （M3-18: 未生成と 404 で区別が付かない曖昧さもこれで消える）。
        */}
        <div className="size-12 shrink-0 overflow-hidden rounded bg-muted">
          {!trash && !thumbFailed ? (
            <img
              src={`/api/recordings/${recording.id}/thumbnail`}
              alt=""
              className="size-full object-cover"
              loading="lazy"
              onError={() => setThumbFailed(true)}
            />
          ) : (
            <div className="size-full bg-muted" aria-hidden />
          )}
        </div>
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
            {trash && recording.deletedAt && (
              <span className="shrink-0">削除 {formatDateTime(recording.deletedAt)}</span>
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

      {expanded && <RecordingDetail recording={recording} trash={trash} />}
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

function RecordingDetail({ recording, trash }: { recording: Recording; trash: boolean }) {
  const encodedProfiles = recording.encodedProfiles ?? []
  const hasOriginal = recording.sizeBytes !== undefined

  return (
    <div className="flex flex-col gap-4 bg-muted/30 px-4 py-3 text-xs">
      {/*
        ごみ箱の録画は配信 3 クエリ（GetOriginalMediaAssetForServing 等）が
        deleted_at IS NOT NULL を 404 にする（docs/api.md §メディア配信）。
        再生・サムネイル・原本リンクはどれも配信経路を叩くので、ごみ箱では
        そもそも出さない（M3-18）。復元してから見る。
        ListTrashRecordings が available_encoded_profiles を射影しないままなのも
        この理由による（プレイヤーを出さないので揃える必要がない）。
      */}
      {!trash && (encodedProfiles.length > 0 || hasOriginal) && (
        <RecordingPlayer
          recordingId={recording.id}
          encodedProfiles={encodedProfiles}
          hasOriginal={hasOriginal}
        />
      )}

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
        {trash && recording.deletedAt && (
          <>
            <dt className="text-muted-foreground">削除日時</dt>
            <dd>{formatDateTime(recording.deletedAt)}</dd>
          </>
        )}
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

      <RecordingActions recordingId={recording.id} trash={trash} />
    </div>
  )
}

/**
 * RecordingActions は論理削除 / 復元 / 即時 purge 印。
 * いずれも DB だけを触り、ファイルは消さない（M3-7）。
 */
function RecordingActions({ recordingId, trash }: { recordingId: number; trash: boolean }) {
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const queryClient = useQueryClient()
  const toast = useToast()
  const deleteRecording = useDeleteRecording()
  const restoreRecording = useRestoreRecording()
  const purgeRecording = usePurgeRecording()

  const invalidate = () => {
    // ライブラリとごみ箱の両方を捨てる（片側の操作がもう片側の集合を変える）
    void queryClient.invalidateQueries({ queryKey: ['/api/recordings'] })
  }

  const busy =
    deleteRecording.isPending || restoreRecording.isPending || purgeRecording.isPending

  if (!trash) {
    return (
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={busy}
          onClick={() => {
            deleteRecording.mutate(
              { id: recordingId },
              {
                onSuccess: () => {
                  invalidate()
                  toast({ message: 'ごみ箱に移しました' })
                },
                onError: () => toast({ message: '削除に失敗しました' }),
              },
            )
          }}
        >
          <Trash2 data-icon="inline-start" />
          ごみ箱へ
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-wrap gap-2">
      <Button
        type="button"
        variant="secondary"
        size="sm"
        disabled={busy}
        onClick={() => {
          restoreRecording.mutate(
            { id: recordingId },
            {
              onSuccess: () => {
                invalidate()
                toast({ message: '復元しました' })
              },
              onError: () => toast({ message: '復元に失敗しました' }),
            },
          )
        }}
      >
        復元
      </Button>
      <AlertDialog open={purgeConfirmOpen} onOpenChange={setPurgeConfirmOpen}>
        <AlertDialogTrigger
          render={
            <Button type="button" variant="destructive" size="sm" disabled={busy}>
              今すぐ完全削除
            </Button>
          }
        />
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>今すぐ完全削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              削除 reconcile がこの録画の原本・派生物・サムネイルを削除します。取り消せません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                // ダイアログは AlertDialogAction（AlertDialogPrimitive.Close ラップ）が
                // クリックで自動的に閉じるので、ここでは実行の確定のみ行う。
                purgeRecording.mutate(
                  { id: recordingId },
                  {
                    onSuccess: () => {
                      invalidate()
                      toast({ message: '完全削除を予約しました' })
                    },
                    onError: () => toast({ message: '完全削除の予約に失敗しました' }),
                  },
                )
              }}
            >
              完全削除を予約する
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

// pidTypeLabels は PID 種別（M2-13）の表示名。
// 値の権威は Go 側（internal/tsstat）にあり、ここに無い値はそのまま表示する。
// 字幕と文字スーパーは stream_type だけでは区別できないので other にまとまる。
const pidTypeLabels: Record<string, string> = {
  video: '映像',
  audio: '音声',
  other: 'その他',
  pat: 'PAT',
  pmt: 'PMT',
  cat: 'CAT',
  nit: 'NIT',
  sdt: 'SDT',
  eit: 'EIT',
  tot: 'TOT',
}

export function DropStatsTable({ recordingId }: { recordingId: number }) {
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
      <div className="grid grid-cols-[auto_auto_1fr_1fr_1fr_1fr] gap-x-3 gap-y-0.5 tabular-nums">
        <span className="text-muted-foreground">PID</span>
        <span className="text-muted-foreground">種別</span>
        <span className="text-right text-muted-foreground">packets</span>
        <span className="text-right text-muted-foreground">drop</span>
        <span className="text-right text-muted-foreground">error</span>
        <span className="text-right text-muted-foreground">scrambled</span>
        {stats.map((s) => (
          <div key={s.pid} className="col-span-6 grid grid-cols-subgrid">
            <span>0x{s.pid.toString(16).padStart(4, '0')}</span>
            {/* 分類できなかった PID は種別なし（PID 番号だけで統計は成立する） */}
            <span className="text-muted-foreground">
              {s.pidType ? (pidTypeLabels[s.pidType] ?? s.pidType) : '—'}
            </span>
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
