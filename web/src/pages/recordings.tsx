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
  shouldShowRecordingSite,
  sourceLabels,
  type RecordingsPageSearch,
} from '@/lib/recording-search'
import { cn } from '@/lib/utils'

/** pageSize は 1 回のフェッチで取る件数（API の既定と同じ）。 */
const pageSize = 50

/** RecordingsView は一覧の表示形式。`card` はサムネイルを大きく並べる。 */
type RecordingsView = 'list' | 'card'

/**
 * VIEW_KEY は表示形式を持続させる localStorage キー。
 *
 * **URL ではなく端末に持つ**（`tab` や絞り込みと違う扱い）。表示形式は共有
 * リンクの宛先ではなく、その端末で見やすい形の好みだから
 * （docs/frontend/design.md §個人化）。`components/app-shell.tsx` の
 * サイドバー畳みと同じ `rokuban:<関心事>:...` の命名。
 */
const VIEW_KEY = 'rokuban:recordings:view'

function loadRecordingsView(): RecordingsView {
  try {
    return localStorage.getItem(VIEW_KEY) === 'card' ? 'card' : 'list'
  } catch {
    // private mode 等で localStorage が使えない場合はリスト
    return 'list'
  }
}

type RecordingsPageParam = { before?: string; beforeId?: number }

type BulkFailure = { id: number; error: unknown }

/** runBulk は既存の 1 件 API を全 ID へ並列送信し、部分成功を分けて返す。 */
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
  // 検索条件・表示タブはどちらも URL に載せる（リロード・共有・戻るで同じ結果に
  // なる。docs/frontend.md「録画検索は /recordings に同居する」/「ごみ箱タブと
  // 検索条件は直交させる」）。タブは条件と直交する別の軸なので `tab` として
  // 別に持ち、既定のライブラリは URL に書かない（履歴を汚さない・共有 URL を短く）。
  const search = useRouteSearch({ from: '/recordings' })
  const navigate = useNavigate()
  const trash = search.tab === 'trash'

  const sitesQuery = useListSites()
  const registeredSites = useMemo(() => unwrap(sitesQuery.data) ?? [], [sitesQuery.data])
  const encodeQueue = unwrap(useGetEncodeQueue().data)
  const updateSearch = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => {
    // debounce（キーワード）・チップの個別解除のどちらも history を汚さないよう
    // 常に replace で書く（docs/frontend.md「debounce と URL 同期で履歴を汚さない」）。
    //
    // updater の引数を絞る理由は `pages/programs.tsx` の `updateSearch` と同じ
    // （`/live` が同じ名前の `service` を単数で持つため合成型が `number |
    // number[]` になる）。
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
    // getListRecordingsQueryKey は先頭要素が recordingsQueryKeyPrefix になる
    // キーを返すので、RecordingActions の invalidateQueries({ queryKey:
    // [recordingsQueryKeyPrefix] })（前方一致）がここにも効く。カーソル
    // （before/beforeId）はキーに含めない ---
    // 同じ絞り込みの中でページを積んでいくのが useInfiniteQuery の前提であり、
    // カーソルをキーに入れるとページごとに別クエリになってしまう。
    queryKey: getListRecordingsQueryKey(listParams),
    queryFn: ({ pageParam }: { pageParam: RecordingsPageParam }) =>
      listRecordings({ ...listParams, ...pageParam }),
    initialPageParam: {} as RecordingsPageParam,
    // **進捗の数字が動いている録画が読み込み済みのページにある間だけ**定期
    // 再取得する（issue #212）。SSE はヒントで真実ではない（不変条件 5）ので、
    // 進捗は REST の再取得で収束させる。止まったら止める --- 一覧は無限リストで、
    // 常時ポーリングすると積んだページ全部を取り直す。止めた後の収束は
    // lib/events.ts の 60 秒 invalidate が担う（hasLiveIngestProgress 参照）。
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
      // 保存できなくても表示は切り替わる（次に開くとリストに戻るだけ）
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

  // autoLoadFailed: 直近の自動読み込みが失敗したか。番兵が可視のままでも自動
  // では再試行しない（さもないと失敗したまま無限にリクエストを投げ続ける）。
  // このページではエラー文言は持たない --- 失敗すると `query.isError` が立ち、
  // 一覧ごと外側の `<ErrorState>` に差し替わるため、番兵の傍にインラインで
  // 出す余地が無い（pages/programs.tsx は同じブロックが三項の外側にあるので
  // そちらには文言がある）。
  const autoLoadFailed = query.isFetchNextPageError
  const paramsKey = JSON.stringify(listParams)
  useEffect(() => {
    // URL の検索条件変更に合わせて選択状態を無効化する外部入力同期。
    // oxlint-disable-next-line react/set-state-in-effect -- URL 条件変更で一覧選択をリセットする
    setSelecting(false)
    setSelected(new Set())
    setPurgeConfirmOpen(false)
  }, [paramsKey])
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
    // 計測できない環境（jsdom 等）では番兵が常時可視と判定されるおそれがあるので
    // IntersectionObserver 自体を作らない。この環境ではボタンだけが受け皿になる
    // （lib/list-virtualization.ts の domLayoutMeasurable）。
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
          // カード表示のトグル自体は 0 件でも出す --- 出さないと、ごみ箱や
          // 絞り込みで 0 件になったタブではリスト表示に戻す手段が無くなる
          // （カード表示のまま次にヒットする画面までトグルへ到達できない）。
          !selecting && (recordings.length > 0 || view === 'card') ? (
            <div className="flex items-center gap-1">
              {/* 状態を持つトグル。読み上げは aria-pressed が担う（ラベルを
                  「リスト表示」に付け替えると、読み上げでは今どちらなのかが
                  分からなくなる）。**見た目の押下状態は variant で出す** ---
                  ghost の文字色は継承した既定の foreground と同じなので、
                  アイコンの className だけを text-foreground に変える旧実装は
                  画素が変わらず目には効かなかった（レビュー指摘）。 */}
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
              {/* 「選択」は 0 件のときは出さない --- 選ぶものが無い編集モードに
                  入れてしまう。0 件でも出すのはトグルだけ（上のコメント）。 */}
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

      {/* 固定の選択バーで末尾行を覆わないだけのスクロール余白を、編集モード中だけ
          予約する。モバイルの 2 段折り返しは e2e/recordings-selection.mjs で実測する。 */}
      <PageContent className={selecting ? 'pb-32 md:pb-20' : undefined}>
        {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()}>
          {apiErrorMessage(query.error) ??
            (trash ? 'ごみ箱の取得に失敗しました' : '録画の取得に失敗しました')}
        </ErrorState>
      ) : query.isPending ? (
        <ListSkeleton />
      ) : recordings.length === 0 ? (
        // 「条件に一致しない」と「まだ何も録れていない」は別の事実。同じ文言だと
        // 後者を誤読させる（issue #137 の罠）。
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

          {/* 番兵。進行方向の自動読み込み（IntersectionObserver）はこれを見る。
              計測できない環境では観測されず、ボタンだけが受け皿になる。 */}
          <div ref={sentinelRef} aria-hidden className="h-px" />

          {query.isFetchingNextPage && (
            // role="status" は付けない。IntersectionObserver でスクロールの
            // たびに mount/unmount するため、role="status" だと連続スクロール
            // 中に読み上げが繰り返される（起動時 1 回だけの他 7 箇所とは頻度が違う）
            <p className="px-4 py-3 text-center text-xs text-muted-foreground">読み込み中…</p>
          )}

          {showLoadMoreButton && (
            <div className="px-4 py-4">
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
                      {/* 取り返しがつかない操作の確定は destructive（issue #467、
                          alert-dialog.tsx の規約）。 */}
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

/**
 * ViewTab はライブラリとごみ箱を切り替えるタブ。
 * フォーカスは `Button` と同じ明示リングを使い、ブラウザ既定の outline は消す。
 */
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

/**
 * RecordingRow は録画一覧の 1 行。
 *
 * 行本体は詳細（`/recordings/$id`）への全面カバーリンク（予約一覧
 * `reservations.tsx` と同じ配置文法）。視聴・削除・エンコードは詳細ページに
 * 寄せ、一覧はインライン展開も常時「再生」列も持たない（issue #311）--- 詳細と
 * 展開が同じ `RecordingDetail` を共有していたので、一覧に同じプレイヤーを二重に
 * 抱える理由が無くなった。ごみ箱・`encodedAssets` が空の行も同じく詳細へリンクし、
 * 再生系の出し分け（`deleted_at` / encoded の有無）は詳細側の規律に任せる。
 */
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
  /** レジストリと読み込み済み録画の site の和集合が 2 件以上のときに出す。 */
  showSite: boolean
  /**
   * `card` はサムネイルを大きく縦に積む。**出す情報は list と同じ**で、
   * 変えるのは並べ方だけ --- 表示形式ごとに出す事実を変えると、切り替えた
   * ときに「見えていたはずのもの」が黙って消える。
   */
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
        // base の gap は list 分岐に持たせる。card 分岐の gap-2 と両方 base に
        // 置くと twMerge が常に後勝ち（gap-2）で解決し、base の gap-3 は
        // list でも死にクラスになる（レビュー指摘）。
        'relative hover:bg-muted/50',
        card
          ? 'flex h-full flex-col gap-2 rounded border border-border p-2'
          : 'flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5',
        selecting && 'cursor-pointer',
        selected && 'bg-muted/50',
      )}
    >
      {/* 編集モード中は全面リンクを外す。残すと checkbox と行クリックを奪う。 */}
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
      {/*
        サムネイルは openapi 外の streamer 経路（/api/recordings/{id}/thumbnail）。
        未生成時は 404 → onError でプレースホルダ。hasThumbnail 列は持たない（M3-4）。
        ごみ箱の録画は配信側が deleted_at IS NOT NULL を 404 にする契約（docs/api.md
        §メディア配信）なので、そもそもリクエストを出さずプレースホルダ固定にする
        （M3-18: 未生成と 404 で区別が付かない曖昧さもこれで消える）。
      */}
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
          <span className="shrink-0">{sourceLabels[recording.source]}</span>
          <IngestBadge recording={recording} />
          {/* エンコード失敗は StatusBadge / IngestBadge と同じ「この録画の
              パイプラインがどこで止まっているか」なので隣に置く。メタデータ列の
              末尾（DropBadges の後）に置くと、狭い端末で失敗バッジが 2 行目
              以降に回る（親は flex-wrap なので隠れはしない）。単体ページの
              ヘッダーも同じ並び。docs/frontend/recordings.md */}
          <EncodeStatusBadges recording={recording} />
          {showSite && (
            /* 文字色は text-foreground を明示（bg-muted 小バッジの合成後コントラスト
               対策。docs/frontend/design.md「コントラストは毎回測る」）。 */
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
      {/* カードは行ではないので、行末の「開く」記号は出さない（面全体がリンク）。 */}
      {!selecting && !card && <ChevronRight className="size-4 shrink-0 text-muted-foreground" />}
    </div>
  )
}
