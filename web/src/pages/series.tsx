import { useQueryClient } from '@tanstack/react-query'
import { LayoutGrid, Pencil, Plus, Trash2 } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'

import {
  ListRecordingShelvesKey,
  getListLabelRulesQueryKey,
  getListRecordingShelvesQueryKey,
  useDeleteLabelRule,
  useListLabelRules,
  useListRecordingShelves,
  type LabelRule,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { LabelRuleForm } from '@/components/label-rule-form'
import { ManualSeriesBadge } from '@/components/manual-series'
import { RecordingSeriesToggle } from '@/components/recording-series-toggle'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/field'
import { formatDate } from '@/lib/format'
import { loadRecordingView, saveRecordingView, type RecordingView } from '@/lib/recording-view'
import { buildShelfRows, sortShelfRows, type ShelfRow, type ShelfSort } from '@/lib/shelves'
import { seriesLabelRules } from '@/lib/series'
import { cn } from '@/lib/utils'

/**
 * SeriesPage は生きている録画をシリーズ単位で眺めるライブラリ。
 *
 * タイルの宛先は正規化キーではなく代表録画 id の番組ハブにする。キーは表示用の
 * 導出値なので、正規化規則を変えてもリンク先が壊れないようにする
 * （docs/data/series.md §8「資源同定: 起点は録画 id」）。値が NULL の棚はハブを
 * 開けないため API には残るが、この画面では表示しない。
 */
export function SeriesPage() {
  const shelvesQuery = useListRecordingShelves({ key: ListRecordingShelvesKey.series })
  const rulesQuery = useListLabelRules()
  const labelRules = useMemo(() => unwrap(rulesQuery.data) ?? [], [rulesQuery.data])
  const [view, setView] = useState(loadRecordingView)
  const [sort, setSort] = useState<ShelfSort>('latest')
  const [filter, setFilter] = useState('')
  const [labelRuleEditor, setLabelRuleEditor] = useState<{ rule?: LabelRule }>()

  const rows = useMemo(() => buildShelfRows(unwrap(shelvesQuery.data) ?? []), [shelvesQuery.data])
  const manualValues = useMemo(
    () => new Set(seriesLabelRules(labelRules).map((rule) => rule.valueKey)),
    [labelRules],
  )
  const visibleRows = useMemo(() => {
    const needle = filter.trim().toLocaleLowerCase('ja-JP')
    const filtered =
      needle === ''
        ? rows
        : rows.filter(
            (row) =>
              row.value.toLocaleLowerCase('ja-JP').includes(needle) ||
              row.title.toLocaleLowerCase('ja-JP').includes(needle),
          )
    return sortShelfRows(filtered, sort)
  }, [filter, rows, sort])

  const toggleView = () => {
    const next = view === 'card' ? 'list' : 'card'
    setView(next)
    saveRecordingView(next)
  }

  return (
    <>
      <PageHeader
        title="シリーズ"
        actions={
          <div className="flex items-center gap-2">
            <RecordingSeriesToggle active="series" />
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
          </div>
        }
      >
        <div className="flex flex-wrap items-center gap-2 border-t border-border px-4 py-2">
          <label className="flex min-w-44 flex-1 items-center gap-2 text-xs text-muted-foreground">
            <span className="shrink-0">絞り込み</span>
            <Input
              aria-label="シリーズを絞り込む"
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="シリーズ名・代表タイトル"
              className="h-8"
            />
          </label>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <span>並び順</span>
            <select
              aria-label="シリーズの並び順"
              value={sort}
              onChange={(event) => setSort(event.target.value as ShelfSort)}
              className="h-8 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
            >
              <option value="latest">新着順</option>
              <option value="count">件数順</option>
              <option value="name">名前順</option>
            </select>
          </label>
        </div>
      </PageHeader>

      <PageContent>
        {shelvesQuery.isError ? (
          <ErrorState onRetry={() => void shelvesQuery.refetch()}>
            シリーズの取得に失敗しました
          </ErrorState>
        ) : shelvesQuery.isPending ? (
          <ListSkeleton />
        ) : visibleRows.length === 0 ? (
          <EmptyState>
            {filter.trim() === '' ? 'シリーズがありません' : '条件に一致するシリーズがありません'}
          </EmptyState>
        ) : (
          <ul
            aria-label="シリーズ一覧"
            className={cn(
              'p-4',
              view === 'card'
                ? 'grid grid-cols-2 lg:grid-cols-[repeat(auto-fill,minmax(min(100%,16rem),1fr))] gap-3'
                : 'flex flex-col gap-2',
            )}
          >
            {visibleRows.map((row) => (
              <li
                key={row.value}
                data-testid="series-shelf"
                data-series-value={row.value}
                data-count={row.count}
                data-playable-count={row.playableCount}
                className="min-w-0"
              >
                <SeriesShelfLink row={row} manual={manualValues.has(row.value)} view={view} />
              </li>
            ))}
          </ul>
        )}

        <section
          aria-labelledby="series-classification-rules-title"
          className="flex flex-col gap-3 border-t border-border px-4 py-5"
        >
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <h2
                id="series-classification-rules-title"
                className="text-sm font-medium text-foreground"
              >
                シリーズ分類
              </h2>
              <p className="mt-1 text-xs text-muted-foreground">
                録画タイトルのキーワードに当たった録画を、指定したシリーズキーへ分類します。
              </p>
            </div>
            <Button
              type="button"
              size="sm"
              className="shrink-0"
              onClick={() => setLabelRuleEditor({})}
            >
              <Plus />
              分類ルールを作成
            </Button>
          </div>

          {rulesQuery.isError ? (
            <ErrorState onRetry={() => void rulesQuery.refetch()}>
              分類ルールの取得に失敗しました（「手動」の表示を省略しています）
            </ErrorState>
          ) : rulesQuery.isPending ? (
            <ListSkeleton rows={2} />
          ) : labelRules.length === 0 ? (
            <EmptyState>分類ルールがありません</EmptyState>
          ) : (
            <ul className="flex flex-col gap-2">
              {labelRules.map((rule) => (
                <li key={rule.id}>
                  <LabelRuleRow rule={rule} onEdit={() => setLabelRuleEditor({ rule })} />
                </li>
              ))}
            </ul>
          )}
        </section>
      </PageContent>

      {labelRuleEditor !== undefined && (
        <LabelRuleForm
          open
          rule={labelRuleEditor.rule}
          onOpenChange={(open) => {
            if (!open) setLabelRuleEditor(undefined)
          }}
        />
      )}
    </>
  )
}

/** LabelRuleRow はシリーズ分類ルールの一覧 1 行。 */
function LabelRuleRow({ rule, onEdit }: { rule: LabelRule; onEdit: () => void }) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const remove = useDeleteLabelRule()
  const [confirmOpen, setConfirmOpen] = useState(false)

  const doRemove = () => {
    remove.mutate(
      { id: rule.id },
      {
        onSuccess: () => {
          setConfirmOpen(false)
          toast({ message: '分類ルールを削除しました（シリーズは再評価の後に変わります）' })
          void queryClient.invalidateQueries({ queryKey: getListLabelRulesQueryKey() })
          void queryClient.invalidateQueries({ queryKey: getListRecordingShelvesQueryKey() })
        },
        onError: (err) =>
          toast({ message: apiErrorMessage(err) ?? '分類ルールの削除に失敗しました', kind: 'error' }),
      },
    )
  }

  return (
    <div className="flex items-center justify-between gap-3 rounded-lg border border-border p-3">
      <div className="flex min-w-0 flex-col">
        <span className="truncate text-sm text-foreground">
          「{rule.keyword}」→ {rule.value}
        </span>
        <span className="text-xs text-muted-foreground">優先度 {rule.priority ?? 0}</span>
        {rule.valueKey !== rule.value && (
          <span className="text-xs text-muted-foreground">
            この値は棚キー {rule.valueKey} として扱われます
          </span>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <Button type="button" size="icon-sm" variant="ghost" aria-label="編集" onClick={onEdit}>
          <Pencil />
        </Button>
        <Button
          type="button"
          size="icon-sm"
          variant="ghost"
          aria-label="削除"
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2 />
        </Button>
      </div>

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>分類ルール「{rule.keyword}」を削除しますか？</DialogTitle>
            <DialogDescription>
              このルールが勝っていた録画は、次に当たるルールへ移ります。無ければ自動キーの
              シリーズへ戻ります（全録画の再評価が走ります）。
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setConfirmOpen(false)}>
              キャンセル
            </Button>
            <Button type="button" variant="destructive" onClick={doRemove} disabled={remove.isPending}>
              削除する
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

/** SeriesShelfLink は操作を重ねず、カード/行全体を番組ハブへのリンクにする。 */
function SeriesShelfLink({
  row,
  manual,
  view,
}: {
  row: ShelfRow
  manual: boolean
  view: RecordingView
}) {
  return (
    <Link
      to="/recordings/$id/series"
      params={{ id: String(row.representativeId) }}
      aria-label={`${row.value}のシリーズ`}
      className={cn(
        'group flex min-w-0 gap-3 rounded-lg border border-border p-3 transition-colors hover:bg-muted/40',
        view === 'card' ? 'h-full flex-col' : 'items-center',
      )}
    >
      <SeriesThumbnail row={row} view={view} />
      <span className="min-w-0 flex-1">
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate text-base text-foreground">{row.value}</span>
          {manual && <ManualSeriesBadge />}
        </span>
        <span className="mt-1 block truncate text-sm text-muted-foreground">{row.title}</span>
        <span
          data-testid="series-shelf-meta"
          className="mt-1 block text-sm text-muted-foreground"
        >
          見られる {row.playableCount.toLocaleString('ja-JP')} 件 · {formatDate(row.latestStartAt)}
        </span>
      </span>
    </Link>
  )
}

function SeriesThumbnail({ row, view }: { row: ShelfRow; view: RecordingView }) {
  const [failed, setFailed] = useState(false)
  return (
    <span
      className={cn(
        'aspect-video shrink-0 overflow-hidden rounded bg-muted',
        view === 'card' ? 'w-full' : 'w-28',
      )}
    >
      {failed ? (
        <span aria-hidden className="block size-full bg-muted" />
      ) : (
        <img
          src={`/api/media/recordings/${row.representativeId}/thumbnail`}
          alt=""
          loading="lazy"
          className="size-full object-cover"
          onError={() => setFailed(true)}
        />
      )}
    </span>
  )
}
