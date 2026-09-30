import { LayoutGrid } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'

import {
  ListRecordingShelvesKey,
  useListLabelRules,
  useListRecordingShelves,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { RecordingSeriesToggle } from '@/components/recording-series-toggle'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/field'
import { formatDate } from '@/lib/format'
import { loadRecordingView, saveRecordingView, type RecordingView } from '@/lib/recording-view'
import { buildShelfRows, sortShelfRows, type ShelfRow, type ShelfSort } from '@/lib/shelves'
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
  const [view, setView] = useState(loadRecordingView)
  const [sort, setSort] = useState<ShelfSort>('latest')
  const [filter, setFilter] = useState('')

  const rows = useMemo(() => buildShelfRows(unwrap(shelvesQuery.data) ?? []), [shelvesQuery.data])
  const manualValues = useMemo(
    () => new Set((unwrap(rulesQuery.data) ?? []).map((rule) => rule.valueKey)),
    [rulesQuery.data],
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
              view === 'card' ? 'grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4' : 'flex flex-col gap-2',
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
        {rulesQuery.isError && !shelvesQuery.isPending && (
          <p role="status" className="px-4 pb-4 text-xs text-muted-foreground">
            分類ルールを取得できないため、「手動」の表示を省略しています
          </p>
        )}
      </PageContent>
    </>
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
          {manual && (
            <span className="shrink-0 rounded border border-border px-1 text-xs text-muted-foreground">
              手動
            </span>
          )}
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
