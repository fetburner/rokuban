import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useSearch as useRouteSearch, useNavigate } from '@tanstack/react-router'
import { ChevronRight, LayoutGrid, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import {
  deleteRecording as deleteRecordingRequest,
  getListRecordingsQueryKey,
  ListRecordingsEncodeState,
  listRecordings,
  purgeRecording as purgeRecordingRequest,
  restoreRecording as restoreRecordingRequest,
  useGetEncodeQueue,
  useListSites,
  type Recording,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { DropBadges, EncodeStatusBadges, IngestBadge, StatusBadge } from '@/components/recording-badges'
import { RecordingFilters } from '@/components/recording-filters'
import { StorageBalance } from '@/components/storage-balance'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
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
import { Chip } from '@/components/ui/chip'
import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { hasLiveIngestProgress, ingestRefetchIntervalMs } from '@/lib/ingest'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import {
  buildListRecordingsParams,
  clearRecordingsFilters,
  hasAnyRecordingsCondition,
  type RecordingsPageSearch,
} from '@/lib/recording-search'
import { cn } from '@/lib/utils'

const pageSize = 50

type RecordingsView = 'list' | 'card'

const VIEW_KEY = 'rokuban:recordings:view'

function shouldShowRecordingSite(
  registeredSites: readonly string[],
  recordingSites: readonly string[],
): boolean {
  return new Set([...registeredSites, ...recordingSites]).size > 1
}
function loadRecordingsView(): RecordingsView {
  try {
    return localStorage.getItem(VIEW_KEY) === 'card' ? 'card' : 'list'
  } catch {
    return 'list'
  }
}
type RecordingsPageParam = { before?: string; beforeId?: number }
type BulkFailure = { id: number; error: unknown }
async function runBulk(
  ids: number[],
  op: (id: number) => Promise<unknown>,
): Promise<{ ok: number[]; failed: BulkFailure[] }> {
  const settled = await Promise.allSettled(ids.map((id) => op(id)))
  const ok: number[] = []
  const failed: BulkFailure[] = []
  settled.forEach((result, i) => {
    if (result.status === 'fulfilled') ok.push(ids[i])
    else failed.push({ id: ids[i], error: result.reason })
  })
  return { ok, failed }
}
function bulkFailureMessage(label: string, failed: BulkFailure[]): string {
  const details = [
    ...new Set(failed.map(({ error }) => apiErrorMessage(error)).filter((detail) => detail !== undefined)),
  ]
  return details.length > 0 ? `${label}: ${details.join(' / ')}` : label
}

/** 録画一覧ページ。表示形式以外の状態は URL とサーバーを正とする。 */
export function RecordingsPage() {
  const search = useRouteSearch({ from: '/recordings' })
  const navigate = useNavigate()
  const trash = search.tab === 'trash'
  const sitesQuery = useListSites()
  const registeredSites = useMemo(() => unwrap(sitesQuery.data) ?? [], [sitesQuery.data])
  const encodeQueue = unwrap(useGetEncodeQueue().data)
  const updateSearch = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => {
    void navigate({
      to: '/recordings',
      search: (prev) => updater(prev as RecordingsPageSearch),
      replace: true,
    })
  }
  const listParams = useMemo(
    () => ({ ...buildListRecordingsParams(search, trash), limit: pageSize }),
    [search, trash],
  )
  const hasConditions = hasAnyRecordingsCondition(search)
  const query = useInfiniteQuery({
    queryKey: getListRecordingsQueryKey(listParams),
    queryFn: ({ pageParam }: { pageParam: RecordingsPageParam }) =>
      listRecordings({ ...listParams, ...pageParam }),
    initialPageParam: {} as RecordingsPageParam,
    refetchInterval: (q) => {
      const now = Date.now()
      const live = (q.state.data?.pages ?? []).some((page) =>
        (unwrap(page) ?? []).some((r) => hasLiveIngestProgress(r, now)),
      )
      return live ? ingestRefetchIntervalMs : false
    },
    getNextPageParam: (lastPage) => {
      const data = unwrap(lastPage) ?? []
      if (data.length < pageSize) return undefined
      const last = data[data.length - 1]
      return { before: last.startAt, beforeId: last.id }
    },
  })
  const recordings = useMemo(
    () => query.data?.pages.flatMap((page) => unwrap(page) ?? []) ?? [],
    [query.data],
  )
  const showSite = useMemo(
    () => shouldShowRecordingSite(registeredSites, recordings.map((recording) => recording.site)),
    [registeredSites, recordings],
  )
  const queryClient = useQueryClient()
  const toast = useToast()
  const [view, setView] = useState<RecordingsView>(loadRecordingsView)
  const [selecting, setSelecting] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(() => new Set())
  const [bulkBusy, setBulkBusy] = useState(false)
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const selectedIds = [...selected]
  const allLoadedSelected = recordings.length > 0 && recordings.every((r) => selected.has(r.id))
  const toggleView = () => {
    const next: RecordingsView = view === 'card' ? 'list' : 'card'
    setView(next)
    try {
      localStorage.setItem(VIEW_KEY, next)
    } catch {
    }
  }
  const toggleSelected = (id: number) => {
    setSelected((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  const invalidateRecordings = () =>
    queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  const reportBulkFailures = (label: string, failed: BulkFailure[]) => {
    if (failed.length === 0) return
    toast({ message: bulkFailureMessage(label, failed), kind: 'error' })
  }
  const restoreIds = async (ids: number[]) => {
    const result = await runBulk(ids, (id) => restoreRecordingRequest(id))
    await invalidateRecordings()
    return result
  }
  const finishSelection = (failed: BulkFailure[]) => {
    setSelected(new Set(failed.map(({ id }) => id)))
    if (failed.length === 0) setSelecting(false)
  }
  const moveSelectedToTrash = async () => {
    setBulkBusy(true)
    try {
      const result = await runBulk(selectedIds, (id) => deleteRecordingRequest(id))
      await invalidateRecordings()
      finishSelection(result.failed)
      if (result.ok.length > 0) {
        const undoIds = result.ok
        toast({
          message: `${undoIds.length} 件をごみ箱へ移動`,
          actions: [
            {
              label: '元に戻す',
              onClick: () => {
                void restoreIds(undoIds).then((undo) =>
                  reportBulkFailures(
                    `${undo.failed.length} 件を元に戻せませんでした`,
                    undo.failed,
                  ),
                )
              },
            },
          ],
        })
      }
      reportBulkFailures(
        `${result.failed.length} 件をごみ箱へ移動できませんでした`,
        result.failed,
      )
    } finally {
      setBulkBusy(false)
    }
  }
  const restoreSelected = async () => {
    setBulkBusy(true)
    try {
      const result = await restoreIds(selectedIds)
      finishSelection(result.failed)
      reportBulkFailures(`${result.failed.length} 件を復元できませんでした`, result.failed)
    } finally {
      setBulkBusy(false)
    }
  }
  const purgeSelected = async () => {
    setBulkBusy(true)
    try {
      const result = await runBulk(selectedIds, (id) => purgeRecordingRequest(id))
      await invalidateRecordings()
      finishSelection(result.failed)
      if (result.ok.length > 0) toast({ message: `${result.ok.length} 件の完全削除を予約しました` })
      reportBulkFailures(
        `${result.failed.length} 件の完全削除を予約できませんでした`,
        result.failed,
      )
    } finally {
      setBulkBusy(false)
    }
  }
  const cancelSelection = () => {
    setSelecting(false)
    setSelected(new Set())
    setPurgeConfirmOpen(false)
  }
  const [autoLoadFailed, setAutoLoadFailed] = useState(false)
  const paramsKey = JSON.stringify(listParams)
  useEffect(() => {
    setAutoLoadFailed(false)
    setSelecting(false)
    setSelected(new Set())
    setPurgeConfirmOpen(false)
  }, [paramsKey])
  useEffect(() => {
    if (query.isFetchNextPageError) setAutoLoadFailed(true)
  }, [query.isFetchNextPageError])
  const autoLoadStateRef = useRef({
    hasNextPage: query.hasNextPage,
    isFetchingNextPage: query.isFetchingNextPage,
    autoLoadFailed,
    fetchNextPage: query.fetchNextPage,
  })
  useEffect(() => {
    autoLoadStateRef.current = {
      hasNextPage: query.hasNextPage,
      isFetchingNextPage: query.isFetchingNextPage,
      autoLoadFailed,
      fetchNextPage: query.fetchNextPage,
    }
  })
  const sentinelRef = useRef<HTMLDivElement>(null)
  const sentinelMounted = !query.isPending && recordings.length > 0
  useEffect(() => {
    if (!sentinelMounted) return
    if (!domLayoutMeasurable()) return
    const node = sentinelRef.current
    if (!node) return
    const observer = new IntersectionObserver((entries) => {
      const isIntersecting = entries.some((entry) => entry.isIntersecting)
      const state = autoLoadStateRef.current
      if (
        shouldAutoLoadNextPage({
          isIntersecting,
          autoLoadAvailable: true,
          autoLoadFailed: state.autoLoadFailed,
          hasNextPage: state.hasNextPage,
          isFetchingNextPage: state.isFetchingNextPage,
        })
      ) {
        void state.fetchNextPage()
      }
    })
    observer.observe(node)
    return () => observer.disconnect()
  }, [sentinelMounted])
  const showLoadMoreButton = shouldShowLoadMoreButton({
    hasNextPage: query.hasNextPage,
    autoLoadAvailable: domLayoutMeasurable(),
    autoLoadFailed,
  })
  return (
    <>
      <PageHeader
        title="録画"
        actions={
          !selecting && (recordings.length > 0 || view === 'card') ? (
            <div className="flex items-center gap-1">
              <Button
                type="button"
                variant={view === 'card' ? 'secondary' : 'ghost'}
                size="sm"
                aria-pressed={view === 'card'}
                aria-label="カード表示"
                onClick={toggleView}
              >
                <LayoutGrid className="size-4" />
              </Button>
              {recordings.length > 0 && (
                <Button type="button" variant="ghost" size="sm" onClick={() => setSelecting(true)}>
                  選択
                </Button>
              )}
            </div>
          ) : undefined
        }
      >
        <div className="flex gap-1 border-t border-border px-4 py-2">
          <ViewTab
            active={!trash}
            onClick={() => updateSearch((s) => ({ ...s, tab: undefined }))}
            label="ライブラリ"
          />
          <ViewTab
            active={trash}
            onClick={() => updateSearch((s) => ({ ...s, tab: 'trash' }))}
            label="ごみ箱"
          />
        </div>
        <RecordingFilters search={search} onChange={updateSearch} />
        {!trash && encodeQueue !== undefined && (
          <div
            aria-label="エンコード待機列"
            className="flex flex-wrap items-center gap-2 border-t border-border px-4 py-2 text-xs text-muted-foreground"
          >
            <span>エンコード</span>
            <Chip
              active={search.encodeState === ListRecordingsEncodeState.queued}
              onClick={() =>
                updateSearch((s) => ({
                  ...s,
                  encodeState:
                    s.encodeState === ListRecordingsEncodeState.queued
                      ? undefined
                      : ListRecordingsEncodeState.queued,
                }))
              }
            >
              待機中 {encodeQueue.queued}件
            </Chip>
            <Chip
              active={search.encodeState === ListRecordingsEncodeState.running}
              onClick={() =>
                updateSearch((s) => ({
                  ...s,
                  encodeState:
                    s.encodeState === ListRecordingsEncodeState.running
                      ? undefined
                      : ListRecordingsEncodeState.running,
                }))
              }
            >
              実行中 {encodeQueue.running}件
            </Chip>
          </div>
        )}
        <StorageBalance />
      </PageHeader>
      <PageContent className={selecting ? 'pb-32 md:pb-20' : undefined}>
        {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()}>
          {apiErrorMessage(query.error) ??
            (trash ? 'ごみ箱の取得に失敗しました' : '録画の取得に失敗しました')}
        </ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : recordings.length === 0 ? (
        <EmptyState>
          {hasConditions ? (
            <div className="flex flex-col items-center gap-3">
              <p>条件に一致する録画がありません</p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => updateSearch(clearRecordingsFilters)}
              >
                条件をクリア
              </Button>
            </div>
          ) : trash ? (
            'ごみ箱は空です'
          ) : (
            '録画がありません'
          )}
        </EmptyState>
      ) : (
        <>
          <ul
            role={selecting ? 'listbox' : undefined}
            aria-multiselectable={selecting || undefined}
            aria-label={selecting ? '録画を選択' : undefined}
            className={
              view === 'card' ? 'grid grid-cols-2 gap-3 p-4 sm:grid-cols-3 lg:grid-cols-4' : undefined
            }
          >
            {recordings.map((r) => (
              <li key={r.id}>
                <RecordingRow
                  recording={r}
                  trash={trash}
                  showSite={showSite}
                  view={view}
                  selecting={selecting}
                  selected={selected.has(r.id)}
                  onToggle={() => toggleSelected(r.id)}
                />
              </li>
            ))}
          </ul>
          <div ref={sentinelRef} aria-hidden className="h-px" />
          {query.isFetchingNextPage && (
            <p className="px-4 py-3 text-center text-xs text-muted-foreground">読み込み中…</p>
          )}
          {showLoadMoreButton && (
            <div className="px-4 py-4">
              {autoLoadFailed && (
                <p role="alert" className="mb-2 text-center text-xs text-destructive">
                  {apiErrorMessage(query.error) ?? '続きの読み込みに失敗しました'}
                </p>
              )}
              <Button
                type="button"
                variant="outline"
                size="lg"
                className="w-full"
                onClick={() => void query.fetchNextPage()}
              >
                さらに読み込む
              </Button>
            </div>
          )}
        </>
      )}
      </PageContent>
      {selecting && (
        <div className="fixed inset-x-0 bottom-[var(--bottom-nav-height)] z-20 flex justify-center px-4 pb-2 md:bottom-0 md:pb-4">
          <div
            role="toolbar"
            aria-label="選択した録画の操作"
            className="flex w-full max-w-3xl flex-wrap items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 shadow-lg"
          >
            <span className="mr-auto text-sm font-medium">{selected.size} 件を選択中</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={bulkBusy}
              onClick={() =>
                setSelected(
                  allLoadedSelected ? new Set() : new Set(recordings.map(({ id }) => id)),
                )
              }
            >
              {allLoadedSelected
                ? `読み込み済みの ${recordings.length} 件の選択を解除`
                : `読み込み済みの ${recordings.length} 件を選択`}
            </Button>
            {trash ? (
              <>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  disabled={bulkBusy || selected.size === 0}
                  onClick={() => void restoreSelected()}
                >
                  復元
                </Button>
                <AlertDialog open={purgeConfirmOpen} onOpenChange={setPurgeConfirmOpen}>
                  <AlertDialogTrigger
                    render={
                      <Button
                        type="button"
                        variant="destructive"
                        size="sm"
                        disabled={bulkBusy || selected.size === 0}
                      >
                        完全削除
                      </Button>
                    }
                  />
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>{selected.size} 件を完全削除しますか？</AlertDialogTitle>
                      <AlertDialogDescription>
                        選択した録画の原本・変換後のファイル・サムネイルを削除します。取り消せません。
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>キャンセル</AlertDialogCancel>
                      <AlertDialogAction variant="destructive" onClick={() => void purgeSelected()}>
                        完全削除を予約する
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </>
            ) : (
              <Button
                type="button"
                variant="destructive"
                size="sm"
                disabled={bulkBusy || selected.size === 0}
                onClick={() => void moveSelectedToTrash()}
              >
                <Trash2 data-icon="inline-start" />
                ごみ箱へ
              </Button>
            )}
            <Button type="button" variant="ghost" size="sm" disabled={bulkBusy} onClick={cancelSelection}>
              キャンセル
            </Button>
          </div>
        </div>
      )}
    </>
  )
}

/** 録画一覧のライブラリ／ごみ箱切り替えタブ。 */
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
        'rounded-md border border-transparent px-3 py-1.5 text-xs transition-[color,background-color] outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50',
        active
          ? 'bg-muted font-medium text-foreground'
          : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
      )}
    >
      {label}
    </button>
  )
}

/** 録画一覧の 1 行。詳細リンクと選択モードを兼ねる。 */
function RecordingRow({
  recording,
  trash,
  showSite,
  view,
  selecting,
  selected,
  onToggle,
}: {
  recording: Recording
  trash: boolean
  showSite: boolean
  view: RecordingsView
  selecting: boolean
  selected: boolean
  onToggle: () => void
}) {
  const [thumbFailed, setThumbFailed] = useState(false)
  const card = view === 'card'
  return (
    <div
      role={selecting ? 'option' : undefined}
      aria-selected={selecting ? selected : undefined}
      onClick={selecting ? onToggle : undefined}
      className={cn(
        'relative hover:bg-muted/50',
        card
          ? 'flex h-full flex-col gap-2 rounded border border-border p-2'
          : 'flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5',
        selecting && 'cursor-pointer',
        selected && 'bg-muted/50',
      )}
    >
      {!selecting && (
        <Link
          to="/recordings/$id"
          params={{ id: String(recording.id) }}
          aria-label={recording.title || '（番組名なし）'}
          className="absolute inset-0"
        />
      )}
      {selecting && (
        <input
          type="checkbox"
          aria-label={`${recording.title || '（番組名なし）'}を選択`}
          className="size-4 shrink-0 accent-primary"
          checked={selected}
          onClick={(event) => event.stopPropagation()}
          onChange={onToggle}
        />
      )}
      <div
        className={cn(
          'aspect-video shrink-0 overflow-hidden rounded bg-muted',
          card ? 'w-full' : 'h-12',
        )}
      >
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
        <div className={cn('text-base', card ? 'line-clamp-2' : 'truncate')}>
          {recording.title || '（番組名なし）'}
        </div>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          <StatusBadge status={recording.status} />
          <IngestBadge recording={recording} />
          <EncodeStatusBadges recording={recording} />
          {showSite && (
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
              {recording.site}
            </span>
          )}
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
      {!selecting && !card && <ChevronRight className="size-4 shrink-0 text-muted-foreground" />}
    </div>
  )
}
