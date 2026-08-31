import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useSearch as useRouteSearch, useNavigate } from '@tanstack/react-router'
import { ChevronRight, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getListRecordingsQueryKey,
  listRecordings,
  restoreRecording as restoreRecordingRequest,
  useAddRecordingEncodeProfiles,
  useDeleteRecording,
  useListEncodeProfiles,
  useListRecordingDropStats,
  useListRules,
  useListSites,
  usePurgeRecording,
  type DropSummary,
  type Recording,
} from '@/api/generated'
import { ApiError } from '@/api/client'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { RecordingFilters } from '@/components/recording-filters'
import { RecordingPlayer } from '@/components/recording-player'
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
import { shouldAutoLoadNextPage, shouldShowLoadMoreButton } from '@/lib/auto-load'
import { encodeJobStatusLabel } from '@/lib/encode-status'
import { useEncodeProgress } from '@/lib/events'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import {
  hasLiveIngestProgress,
  ingestDisplay,
  ingestRefetchIntervalMs,
  type IngestDisplay,
} from '@/lib/ingest'
import { domLayoutMeasurable } from '@/lib/list-virtualization'
import { mutationErrorMessage } from '@/lib/mutation-error-message'
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

export function RecordingsPage() {
  // 検索条件・表示タブはどちらも URL に載せる（リロード・共有・戻るで同じ結果に
  // なる。docs/frontend.md「録画検索は /recordings に同居する」/「ごみ箱タブと
  // 検索条件は直交させる」）。タブは条件と直交する別の軸なので `tab` として
  // 別に持ち、既定のライブラリは URL に書かない（履歴を汚さない・共有 URL を短く）。
  const search = useRouteSearch({ from: '/recordings' })
  const navigate = useNavigate()
  const trash = search.tab === 'trash'

  // 多サイトのときだけ行に site を出す（同じ (networkId, serviceId) を 2 サイトで
  // 受けたとき行を見分ける材料が site しか無い。issue #283）。単一サイトでは
  // 「default」がノイズになるだけなので出さない。レジストリは SiteGate が既に
  // 取得済み（同じクエリキー）。
  const showSite = (unwrap(useListSites().data) ?? []).length > 1
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
    // getListRecordingsQueryKey は先頭要素が '/api/recordings' になるキーを返すので、
    // RecordingActions の invalidateQueries({ queryKey: ['/api/recordings'] })
    // （前方一致）がここにも効く。カーソル（before/beforeId）はキーに含めない ---
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
        <StorageBalance />
      </PageHeader>

      <PageContent>
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
                <RecordingRow recording={r} trash={trash} showSite={showSite} />
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
}: {
  recording: Recording
  trash: boolean
  /** 多サイトのときだけ site を出す（issue #283）。 */
  showSite: boolean
}) {
  const [thumbFailed, setThumbFailed] = useState(false)

  return (
    <div className="relative flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5 hover:bg-muted/50">
      {/* 行本体は詳細（/recordings/$id）への全面カバーリンク（予約一覧
          `reservations.tsx` と同じ配置文法）。`position: relative` の親を
          containing block にして、リンクだけを見えない全面の層に退避させる ---
          サムネイル・バッジ列・chevron は通常フローに残す。子を持たないので
          accessible name は aria-label で渡す（children から計算できない）。 */}
      <Link
        to="/recordings/$id"
        params={{ id: String(recording.id) }}
        aria-label={recording.title || '（番組名なし）'}
        className="absolute inset-0"
      />
      {/*
        サムネイルは openapi 外の streamer 経路（/api/recordings/{id}/thumbnail）。
        未生成時は 404 → onError でプレースホルダ。hasThumbnail 列は持たない（M3-4）。
        ごみ箱の録画は配信側が deleted_at IS NOT NULL を 404 にする契約（docs/api.md
        §メディア配信）なので、そもそもリクエストを出さずプレースホルダ固定にする
        （M3-18: 未生成と 404 で区別が付かない曖昧さもこれで消える）。
      */}
      <div className="aspect-video h-12 shrink-0 overflow-hidden rounded bg-muted">
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
        <div className="truncate text-base">{recording.title || '（番組名なし）'}</div>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          <StatusBadge status={recording.status} />
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
      <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
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
 *
 * `finished` の文字色は `text-foreground`（bg-muted 小バッジの合成後コントラスト
 * 対策。docs/frontend/design.md「コントラストは毎回測る」）。foreground は色では
 * なく地の無彩 3 値の一部なので「色は信号のみ」は破っていない。
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
 * ingestDetailText は詳細ページの「取り込み」欄の文言（issue #212）。
 *
 * 一覧のバッジ（`IngestBadge`）より一段詳しく、分母が取れていれば
 * 「1.2 GB / 3.4 GB」まで出す。**分母が無いときに割合をでっち上げない**
 * （mirakc が record の length を返さない構成があるため。`openapi.yaml` の
 * `IngestProgress.expectedBytes`）。
 *
 * `originalDeleted` をここで言い切れるのは、サーバーが「`kind='original'` の
 * 行が state を問わず存在するか」を見て `committed` を返しているから ---
 * `sizeBytes` の省略だけを見ていた頃は未 ingest と区別できず、未 ingest の
 * 録画に「削除済み」と読める表示が出ていた（issue #211）。
 */
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

/**
 * DropBadges はドロップ統計をひと目で分かる形で出す。
 * 0 のものは出さないので、正常な録画ではバッジが 1 つも出ない。
 */
function DropBadges({ summary }: { summary: DropSummary }) {
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
 * は何も出さない --- 機能しないキュー画面や空の進捗バーを出さない判断（下記
 * `docs/frontend/recordings.md`）はサーバー側の省略で表現されており、ここは
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
          {encodeJobStatusLabel(
            s.state,
            s.state === 'running' ? progress.get(s.profile) : undefined,
          )}
        </span>
      ))}
    </>
  )
}

/**
 * RecordingDetail は録画 1 件の詳細本体（プレイヤー・メタデータ・操作）。
 * 単体ページ（`pages/recording-detail.tsx`）が使う。一覧はインライン展開せず、
 * 行本体から単体ページへ移動する（issue #311）。
 *
 * 単体ページはここで行われる削除 / 復元 / 完全削除 / 追加エンコードのどの
 * mutate が成功しても自分自身を再描画したいが、それを prop で 1 段ずつ手渡す
 * 形（例: `onMutated`）は「この部品の下で mutate する者は全員 prop を受け取る」
 * という規律を要求し、守らせる仕組みが無い。実際、最初の実装はこの穴を
 * `RecordingActions` にだけ塞いで `AddEncodeProfilesAction`（下記）を素通しし、
 * 単体ページで「追加エンコードを依頼」しても再検証されない不具合になった
 * （issue #232 のレビューで実機再現）。
 *
 * 直し方は prop を増やすことではなく、**単体ページ自身のクエリキーを一覧の
 * invalidate に前方一致させる**（`pages/recording-detail.tsx` の
 * `recordingDetailQueryKey` 参照）。ここに mutater を何人足しても、
 * 各自が今のまま `['/api/recordings']` を invalidate するだけで単体ページも
 * 自動的に巻き込まれるので、`RecordingDetail` 自身に配線用の prop は要らない。
 */
export function RecordingDetail({
  recording,
  trash,
}: {
  recording: Recording
  trash: boolean
}) {
  const encodedAssets = recording.encodedAssets ?? []
  const hasOriginal = recording.sizeBytes !== undefined
  const ingestState = ingestDisplay(recording, Date.now())
  const showSite = (unwrap(useListSites().data) ?? []).length > 1

  return (
    <div className="flex flex-col gap-4 bg-muted/30 px-4 py-3 text-xs">
      {/*
        ごみ箱の録画は配信 3 クエリ（GetOriginalMediaAssetForServing 等）が
        deleted_at IS NOT NULL を 404 にする（docs/api.md §メディア配信）。
        再生・サムネイル・原本リンクはどれも配信経路を叩くので、ごみ箱では
        そもそも出さない（M3-18）。復元してから見る。
        ListTrashRecordings が available_encoded_assets を射影しないままなのも
        この理由による（プレイヤーを出さないので揃える必要がない）。
      */}
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
        {/* 取り込み（issue #212）。正常に完了して原本がある録画では
            ingestDisplay が undefined を返すので、この行ごと出ない ---
            言うことが無いときに「完了」とだけ書かれた行を並べない。 */}
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

      {/* 手動予約由来の録画には ruleId が無い。「機能しないコントロールは
          置かない」の既存規律に従い、セクションごと出さない（issue #230）。 */}
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

      {/* PID 別の内訳は行数が多いので、モバイルで横スクロールさせずここに畳む */}
      {recording.dropSummary && <DropStatsTable recordingId={recording.id} />}

      <RecordingActions recording={recording} trash={trash} />
    </div>
  )
}

/**
 * RuleSection は「この録画はどのルールが録ったのか」への導線（issue #230）。
 * 呼び出し側（RecordingDetail）が `recording.ruleId !== undefined` を確認して
 * からマウントするので、ここでは「ある」ことを前提にできる。
 *
 * **ルール名の解決は `useListRules` のキャッシュから引く（単体取得の
 * `GET /api/rules/{id}` / `useGetRule` はあるが使わない）。** `RulesPage` が
 * `useListRules()`（パラメータなし = 常に全件）で一覧を引く設計に既に乗って
 * いるので、録画ごとに個別の 1 件取得を増やす理由がない。`/rules` を
 * 経由していればキャッシュに乗っており、していなければここで引く（後者は
 * 下記の `#N` → ルール名の差し替えとして見える）。同じ `queryKey`
 * （`/api/rules`）の 1 本のクエリで、`ruleId` ごとの取得は発行しない。
 *
 * **`rules.find` が見つからない場合は `#N` 表記に落とす。** これは「ルールが
 * 削除された」ケースではない --- `recordings.rule_id` は `rules` への FK
 * `recordings_rule_id_fkey` が `ON DELETE SET NULL` なので、ルールを削除すると
 * `recordings.rule_id` が NULL になり `Recording.ruleId` 自体が省略され、この
 * セクションごと消える（`#N` へは落ちない）。`#N` に落ちるのは `rules.find`
 * が空を返す間、つまり一覧クエリが未解決 / 失敗（どちらも `query.data` が
 * `undefined`）か、返ってきた一覧にその id がまだ無い（新しく作られたルール等）
 * という一時的な状態。未解決の場合に `#N` → ルール名へ差し替わることは
 * `recording-detail.test.tsx`「ルール一覧が未解決の間は #N を出し、解決後に
 * ルール名へ差し替わる」で固定した。
 *
 * 原則「固有名詞はリンク」（issue #221）に従い、ルールの識別（名前 or
 * `#N`）そのものをリンクテキストにする --- 装飾テキストの隣にリンクを
 * 置く形にしない。リンク先は `/search?ruleId=N`（ルールの実質的な編集画面。
 * `RulesPage` の「検索しながら編集」と同じ着地先）。
 */
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

/**
 * RecordingActions は論理削除 / 復元 / 即時 purge 印 + 追加エンコードの依頼。
 * 削除系はいずれも DB だけを触り、ファイルは消さない（M3-7）。
 */
export function RecordingActions({ recording, trash }: { recording: Recording; trash: boolean }) {
  const recordingId = recording.id
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const [restoring, setRestoring] = useState(false)
  const queryClient = useQueryClient()
  const toast = useToast()
  const deleteRecording = useDeleteRecording()
  const purgeRecording = usePurgeRecording()

  const invalidate = () => {
    // ライブラリとごみ箱の両方を捨てる（片側の操作がもう片側の集合を変える）。
    // 単体ページ（pages/recording-detail.tsx）はこのキーに前方一致する
    // クエリキー（recordingDetailQueryKey）を自ら使っているので、単体ページ
    // だけを別途再検証する配線はここには要らない（テスト:
    // recording-detail.test.tsx「ごみ箱へ移すと、ナビゲーションなしで…」）。
    void queryClient.invalidateQueries({ queryKey: ['/api/recordings'] })
  }

  // restore は「復元」ボタン本体と、ごみ箱送りトーストの Undo（下記）の
  // 両方から呼ぶ。後者は、Undo を呼んだ時点で元のごみ箱送りを起こした
  // RecordingActions 自身がすでにアンマウントされていることがある --- トーストは
  // 別の画面へ遷移した後にも押せるので、そのとき詳細ページ（と RecordingActions）は
  // 画面に無い。`useRestoreRecording` の `mutate` はコンポーネントに束縛された
  // `useMutation` の内部状態を経由するため、渡した `onSuccess`/`onError` が
  // アンマウント後も確実に呼ばれる保証を前提にできない（実測: `recording-detail.test.tsx`
  // 「ごみ箱へ移すと Undo 付きトーストが出て、「元に戻す」でライブラリ表示に戻る」を
  // `useRestoreRecording` の `mutate` 経由に戻して壊すと、リクエストは飛ぶが渡した
  // onSuccess が呼ばれないまま表示が戻らず、アサーションで落ちる）。生成された
  // 素の関数（`restoreRecordingRequest`）を直接呼び、`queryClient`
  // （`useQueryClient()` はマウント状態に依存しない安定した参照）で
  // invalidate する形にして、この依存を断つ。
  //
  // 復元の効果は「復元」ボタン本体からの呼び出しでは必ず画面に見える ---
  // 単体ページ（recording-detail.tsx）で trash 判定が反転してボタン・削除日時
  // 表示が入れ替わる。追加で言うことも無いので成功トーストは無音化する
  // （issue #297）。
  //
  // **Undo 経由の呼び出しは事情が違う。** トーストは最大 6 秒後、かつ別の
  // 画面へ遷移した後にも押せるので、そのときは復元の効果を画面上で確認
  // できるとは限らない（対象の録画をもう見ていないことがある）うえ、
  // 成功トーストも出さないので追加のフィードバックも無い。ここでは
  // 「Undo ボタンを押した」こと自体（ボタンが消える）を操作の完了通知として
  // 扱い、割り切っている --- 失敗時は場所を問わず追える情報（`復元に
  // 失敗しました`）を出す。
  const restore = () => {
    setRestoring(true)
    restoreRecordingRequest(recordingId)
      .then(() => invalidate())
      .catch((err: unknown) =>
        toast({ message: mutationErrorMessage('復元に失敗しました', err), kind: 'error' }),
      )
      .finally(() => setRestoring(false))
  }

  const busy = deleteRecording.isPending || restoring || purgeRecording.isPending

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
                    // ごみ箱送りの効果（単体ページのボタン入れ替え）は
                    // restore と同じ理由で常に画面に見えるが、
                    // ごみ箱送りは復元で即座に取り消せる安価な操作なので、
                    // 素の成功通知の代わりに Undo 付きトーストにする
                    // （`pages/programs.tsx` の予約作成 + 取消と同じ形。
                    // issue #297 が指す理想形）。復元と違ってここは Undo を
                    // 提供する側なので silence だけでは終わらせない。
                    toast({
                      message: 'ごみ箱に移しました',
                      action: { label: '元に戻す', onClick: () => restore() },
                    })
                  },
                  onError: (err) =>
                    toast({ message: mutationErrorMessage('削除に失敗しました', err), kind: 'error' }),
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
        onClick={() => restore()}
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
              この録画の原本・変換後のファイル・サムネイルを削除します。取り消せません。
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
                    onError: (err) =>
                      toast({
                        message: mutationErrorMessage('完全削除の予約に失敗しました', err),
                        kind: 'error',
                      }),
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
 * `sizeBytes` は一覧の射影（`recordingsFromJoins`、`internal/api/recordings_query.go`）
 * が `a.kind = 'original' AND a.state <> 'deleted'` の行から埋める。一方サーバー側の
 * 409 判定（`GetActiveOriginalMediaAsset`、`internal/db/queries/media_assets.sql`）は
 * `state = 'active'` だけを見る --- **2 つの条件は同じではない**。`state = 'deleting'`
 * （unlink 待ち）の原本は射影には出る（`sizeBytes` あり）が 409 判定には出ないため、
 * `sizeBytes` があってもボタンを押すと確定で 409 になることがある
 * （`internal/api/recordings.go` の `AddRecordingEncodeProfiles` が同じ理由でこの
 * ケースを踏まえて 409 文言を hedge している）。ここでの `hasOriginal` はこの近似の
 * 上に立つ先読みであり、409 を完全には避けられない。
 *
 * `sizeBytes` が省略される（`hasOriginal` が偽になる）録画は「原本が削除された」に
 * 限らない --- ingest がまだ完了していない/失敗中でリトライ待ちの録画（`media_assets`
 * に `kind=original` の行が 1 つも無い）も同じ形になる（issue #211: 実観測では
 * `/mnt/media` の権限不足で ingest が permission denied のままリトライ中だった録画に
 * 「原本が削除済み」と断定する文言が出て誤誘導になった）。区別する情報が API に
 * 無いので、断定しない中立文言に落とす。
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
        この録画には再生可能な原本がありません。追加のエンコードは依頼できません。
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
                    toast({
                      message:
                        // 409 はサーバー側のメッセージが英語かつ「削除済みとは
                        // 限らない」複数原因の hedge 文言（AddRecordingEncodeProfiles
                        // の doc コメント参照）なので、そのまま出さず 409 専用の
                        // 日本語文言に翻訳する。hasOriginal の近似が破れて
                        // 「原本あり」に見えるボタンを押しても 409 になりうる
                        // （上記 doc コメントの `state = 'deleting'` の説明）ため、
                        // ここで初めてこの状態を利用者に伝える。
                        err instanceof ApiError && err.status === 409
                          ? '原本の状態が変わったため追加できませんでした（削除済み・削除処理中・未取り込みのいずれか）。画面を更新してから再度お試しください。'
                          : (apiErrorMessage(err) ?? 'エンコードの依頼に失敗しました'),
                      kind: 'error',
                    }),
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
    return (
      <p role="status" className="text-muted-foreground">
        ドロップ統計を読み込み中…
      </p>
    )
  }
  if (query.isError) {
    return <p className="text-destructive">ドロップ統計の取得に失敗しました</p>
  }
  if (stats.length === 0) return null

  return (
    <section>
      <h4 className="mb-1 font-medium">PID 別ドロップ統計</h4>
      <div className="grid grid-cols-[auto_auto_1fr_1fr_1fr_1fr] gap-x-3 gap-y-0.5">
        <span className="text-muted-foreground">PID</span>
        <span className="text-muted-foreground">種別</span>
        <span className="text-right text-muted-foreground">packets</span>
        <span className="text-right text-muted-foreground">ドロップ</span>
        <span className="text-right text-muted-foreground">エラー</span>
        <span className="text-right text-muted-foreground">スクランブル</span>
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
