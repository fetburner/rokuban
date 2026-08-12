import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { useSearch as useRouteSearch, useNavigate } from '@tanstack/react-router'
import { ChevronDown, Play, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getListRecordingsQueryKey,
  listRecordings,
  useAddRecordingEncodeProfiles,
  useDeleteRecording,
  useListEncodeProfiles,
  useListRecordingDropStats,
  usePurgeRecording,
  useRestoreRecording,
  type DropSummary,
  type Recording,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { RecordingFilters } from '@/components/recording-filters'
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
import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import {
  buildListRecordingsParams,
  clearRecordingsFilters,
  hasAnyRecordingsCondition,
  sourceLabels,
  statusLabels,
  type RecordingsPageSearch,
} from '@/lib/recording-search'
import { cn } from '@/lib/utils'

/** pageSize は 1 回のフェッチで取る件数（API の既定と同じ）。 */
const pageSize = 50

type RecordingsPageParam = { before?: string; beforeId?: number }

type ViewMode = 'library' | 'trash'

export function RecordingsPage() {
  const [mode, setMode] = useState<ViewMode>('library')
  const trash = mode === 'trash'

  // 検索条件は URL に載せる（リロード・共有・戻るで同じ結果になる。
  // docs/frontend.md「録画検索は /recordings に同居する」）。タブ（ライブラリ /
  // ごみ箱）は条件と直交する別の軸なので URL には載せず、ここでは component
  // state のまま持つ。
  const search = useRouteSearch({ from: '/recordings' })
  const navigate = useNavigate()
  const updateSearch = (updater: (prev: RecordingsPageSearch) => RecordingsPageSearch) => {
    // debounce（キーワード）・チップの個別解除のどちらも history を汚さないよう
    // 常に replace で書く（docs/frontend.md「debounce と URL 同期で履歴を汚さない」）。
    //
    // **updater の引数は「全ルートの search を合成した型」で来る**（TanStack Router
    // の `ParamsReducerFn`）。`/live` が同じ名前の `serviceId` を単数（`number`）で
    // 持つため、合成後は `number | number[]` になり `RecordingsPageSearch` に
    // そのままは代入できない。この関数が呼ばれるのは `/recordings` に居るときだけ
    // で、そのとき実際に入っているのは `parseRecordingsSearch` が検証した形なので、
    // ここで絞ってから updater に渡す
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
    // getListRecordingsQueryKey は先頭要素が '/api/recordings' になるキーを返すので、
    // RecordingActions の invalidateQueries({ queryKey: ['/api/recordings'] })
    // （前方一致）がここにも効く。カーソル（before/beforeId）はキーに含めない ---
    // 同じ絞り込みの中でページを積んでいくのが useInfiniteQuery の前提であり、
    // カーソルをキーに入れるとページごとに別クエリになってしまう。
    queryKey: getListRecordingsQueryKey(listParams),
    queryFn: ({ pageParam }: { pageParam: RecordingsPageParam }) =>
      listRecordings({ ...listParams, ...pageParam }),
    initialPageParam: {} as RecordingsPageParam,
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

  // autoLoadFailed: 直近の自動読み込みが失敗したか。失敗したらボタン + エラー
  // 表示に落とし、番兵が可視のままでも自動では再試行しない（さもないと失敗した
  // まま無限にリクエストを投げ続ける。pages/programs.tsx と同じ規律）。
  const [autoLoadFailed, setAutoLoadFailed] = useState(false)
  const paramsKey = JSON.stringify(listParams)
  useEffect(() => {
    setAutoLoadFailed(false)
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
        <RecordingFilters search={search} onChange={updateSearch} />
      </PageHeader>

      {query.isError ? (
        <ErrorState>
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
          <ul>
            {recordings.map((r) => (
              <li key={r.id}>
                <RecordingRow recording={r} trash={trash} />
              </li>
            ))}
          </ul>

          {/* 番兵。進行方向の自動読み込み（IntersectionObserver）はこれを見る。
              計測できない環境では観測されず、ボタンだけが受け皿になる。 */}
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

/**
 * RecordingRow は録画一覧の 1 行。
 *
 * 行本体のタップは従来どおり詳細（メタデータ・削除系操作を含む全体）を展開する。
 * それとは別に、最頻操作である「再生」を行右端の固定幅ボタンに独立させる
 * （issue #227。予約行の予約ボタンと同じ配置文法 --- `program-row.tsx` の
 * 「予約ボタンは行本体と分離した固定幅」参照）。以前は再生も他のメタデータと
 * 同じ「展開」の 1 段下に埋もれていた。
 *
 * **再生ボタンは「展開してプレイヤーへスクロール + フォーカス」であって
 * 「即再生」ではない。** `<video preload="metadata">` はメタデータだけを
 * 先読みするが、実際の再生開始（`.play()` に相当する処理）はブラウザが
 * 本編データの取得を始める重い操作で、M7 の値札方針（コストのかかる操作を
 * 暗黙に始めない）と衝突する。行のボタンをワンタップしただけで本編の転送が
 * 始まると、一覧をスクロール中の誤タップがそのまま通信量になる。ネイティブ
 * `<video controls>` の再生ボタンをもう一段挟むことで、実際のデータ転送は
 * 利用者の最後の明示的なクリックに紐付く（予約のワンタップ + トーストとは
 * 非対称だが、予約は DB 行を作るだけで安価、再生は帯域を伴うため意図的に
 * 揃えていない）。
 *
 * 出し分けは 2 条件: ごみ箱では出さない（配信側が `deleted_at IS NOT NULL` を
 * 404 にする契約なので出しても必ず失敗する）。`encodedProfiles` が空でも
 * 出さない（`RecordingPlayer` が実際に `<video>` を描くかどうかと同じ条件 ---
 * 原本だけがある録画は VLC リンクしか出さないので「再生」ボタンの対象ではない）。
 */
function RecordingRow({ recording, trash }: { recording: Recording; trash: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const [thumbFailed, setThumbFailed] = useState(false)
  // プレイヤーへのフォーカス要求。値そのものに意味は無く、変化を検知する
  // トークンとして使う（同じ値を渡し続けても再度スクロール/フォーカスしたい
  // ケース --- 展開済みの行でもう一度「再生」を押した場合 --- を拾うため）。
  //
  // **行を閉じるたびに 0 へ戻す。** `RecordingDetail`（延いては
  // `RecordingPlayer`）は `expanded` の真偽で mount/unmount される。0 に
  // 戻さず値を残したまま次に「行本体タップ」で開き直すと、Play を経由せず
  // 開いただけなのに新しくマウントされた `RecordingPlayer` の初回 effect が
  // 古いトークン（非 0）を見てフォーカス/スクロールを要求してしまう ---
  // 「Play を押したときだけプレイヤーへ連れて行く」という決定が、行本体
  // タップという別の経路でも起きる意図しない再発火になる。閉じた時点で
  // 0 に戻せば、次に非 0 になるのは再び Play を押したときだけになる。
  const [focusPlayerToken, setFocusPlayerToken] = useState(0)

  const playable = !trash && (recording.encodedProfiles ?? []).length > 0

  return (
    <div className="border-b border-border">
      <div className="flex items-stretch">
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() =>
            setExpanded((v) => {
              const next = !v
              if (!next) setFocusPlayerToken(0)
              return next
            })
          }
          className="flex min-h-14 min-w-0 flex-1 items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/50"
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

        {/* 再生ボタンは行本体と分離した固定幅。最小 44px のタップ領域を確保する
            （program-row.tsx の予約ボタンと同じ配置文法）。 */}
        {playable && (
          <div className="flex w-20 shrink-0 items-center justify-center border-l border-border">
            <Button
              type="button"
              size="sm"
              className="min-h-11 w-full rounded-none"
              aria-label={`${recording.title || '（番組名なし）'}を再生`}
              onClick={() => {
                setExpanded(true)
                setFocusPlayerToken((t) => t + 1)
              }}
            >
              <Play data-icon="inline-start" />
              再生
            </Button>
          </div>
        )}
      </div>

      {expanded && (
        <RecordingDetail
          recording={recording}
          trash={trash}
          focusPlayerToken={focusPlayerToken}
        />
      )}
    </div>
  )
}

/**
 * StatusBadge は録画の状態。**録画中だけがタリーレッドの塗り**になる
 * （docs/frontend/design.md「色は信号のみ」）。
 *
 * 赤は 2 つの意味に使うが、色相ではなく**形で分ける**: タリーは「点灯」なので
 * 塗り（`bg-tally` + 紙白の文字）、destructive は「取り返しがつかない」なので
 * 文字と淡い地（`text-destructive` + `bg-destructive/10`）。同じ赤でも、
 * 塗られているかどうかで「いま電波に乗っている」と「壊れた」を見分けられる。
 */
function StatusBadge({ status }: { status: Recording['status'] }) {
  return (
    <span
      className={cn(
        'shrink-0 rounded px-1.5 py-0.5 text-[0.65rem]',
        status === 'failed' && 'bg-destructive/10 text-destructive',
        status === 'recording' && 'bg-tally font-medium text-tally-foreground',
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

function RecordingDetail({
  recording,
  trash,
  focusPlayerToken,
}: {
  recording: Recording
  trash: boolean
  /** 再生ボタンから展開されたときにインクリメントされる（RecordingRow）。 */
  focusPlayerToken?: number
}) {
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
          focusToken={focusPlayerToken}
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
        <dd>{sourceLabels[recording.source]}</dd>
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

      <RecordingActions recording={recording} trash={trash} />
    </div>
  )
}

/**
 * RecordingActions は論理削除 / 復元 / 即時 purge 印 + 追加エンコードの依頼。
 * 削除系はいずれも DB だけを触り、ファイルは消さない（M3-7）。
 */
function RecordingActions({ recording, trash }: { recording: Recording; trash: boolean }) {
  const recordingId = recording.id
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
      <div className="flex flex-col gap-2">
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
        {/*
          事後追加は凍結の例外（issue #133、docs/storage.md §6「凍結の例外:
          事後追加」）。ごみ箱に入った録画は削除 reconcile 対象なので出さない
          （下の trash 分岐と同じ理由）。
        */}
        <AddEncodeProfilesAction recording={recording} />
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

/**
 * AddEncodeProfilesAction は録画完了後に事後的にエンコードを追加依頼するボタン
 * （issue #133、凍結の例外。docs/storage.md §6「凍結の例外: 事後追加」）。
 *
 * `GET /api/encode-profiles` の一覧から、この録画の `encodeProfiles`（凍結された
 * desired。pending なジョブのぶんも含む）に無いものだけを選択肢にする ---
 * 既に追加済み/完了済みのプロファイルを選ばせない（罠: `UniqueOpts` が二重投入を
 * 黙って握りつぶすため、UI 側で「追加済み」を出して二重依頼に見せない）。
 *
 * 原本削除済みの録画は `sizeBytes` が省略される（`recordingFromListFields` の
 * 射影。`OriginalSizeBytes` が無い = 原本 media_asset が active でない）ので、
 * それをボタンを出す/出さないの判定にそのまま使う --- サーバー側の 409 判定
 * （`GetActiveOriginalMediaAsset`）と同じ条件を UI 側でも先読みし、押しても
 * 必ず失敗するボタンを表示しない。
 */
function AddEncodeProfilesAction({ recording }: { recording: Recording }) {
  const hasOriginal = recording.sizeBytes !== undefined
  const profilesQuery = useListEncodeProfiles()
  const profiles = unwrap(profilesQuery.data) ?? []
  const alreadyRequested = recording.encodeProfiles ?? []
  const alreadyRequestedSet = new Set(alreadyRequested)
  const addable = profiles.filter((p) => !alreadyRequestedSet.has(p.name))
  const [selected, setSelected] = useState<string[]>([])
  const queryClient = useQueryClient()
  const toast = useToast()
  const addProfiles = useAddRecordingEncodeProfiles()

  if (!hasOriginal) {
    return (
      <p className="text-xs text-muted-foreground">
        原本が削除済みのため、追加のエンコードは依頼できません。
      </p>
    )
  }
  if (profilesQuery.isError) {
    return <p className="text-xs text-destructive">プロファイル一覧の取得に失敗しました</p>
  }
  if (profilesQuery.isPending || profiles.length === 0) {
    // 取得中、または設定にプロファイルが無い場合は何も出さない
    // （EncodeSettingsFields と違い、こちらは無くても他の操作に支障が無い）。
    return null
  }

  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border p-2">
      <span className="text-xs text-muted-foreground">事後エンコードの追加</span>
      {alreadyRequested.length > 0 && (
        <p className="text-xs text-muted-foreground">追加済み: {alreadyRequested.join(', ')}</p>
      )}
      {addable.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          すべてのエンコードプロファイルが追加済みです。
        </p>
      ) : (
        <>
          <ul
            role="group"
            aria-label="追加するエンコードプロファイル"
            className="flex flex-col gap-1"
          >
            {addable.map((p) => {
              const checked = selected.includes(p.name)
              return (
                <li key={p.name}>
                  <label className="flex min-h-8 cursor-pointer items-center gap-2 text-sm text-foreground">
                    <input
                      type="checkbox"
                      className="size-4 accent-primary"
                      checked={checked}
                      disabled={addProfiles.isPending}
                      onChange={() =>
                        setSelected((s) =>
                          checked ? s.filter((n) => n !== p.name) : [...s, p.name],
                        )
                      }
                    />
                    <span>{p.name}</span>
                  </label>
                </li>
              )
            })}
          </ul>
          <Button
            type="button"
            size="sm"
            disabled={selected.length === 0 || addProfiles.isPending}
            onClick={() => {
              addProfiles.mutate(
                { id: recording.id, data: { profiles: selected } },
                {
                  onSuccess: () => {
                    setSelected([])
                    void queryClient.invalidateQueries({ queryKey: ['/api/recordings'] })
                    toast({ message: 'エンコードを依頼しました' })
                  },
                  onError: (err) =>
                    toast({ message: apiErrorMessage(err) ?? 'エンコードの依頼に失敗しました' }),
                },
              )
            }}
          >
            {addProfiles.isPending ? '依頼中…' : '追加エンコードを依頼'}
          </Button>
        </>
      )}
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
