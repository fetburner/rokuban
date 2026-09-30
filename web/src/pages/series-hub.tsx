import { useInfiniteQuery } from '@tanstack/react-query'
import { Link, useNavigate, useParams, useRouter } from '@tanstack/react-router'
import { ArrowLeft, MoreVertical } from 'lucide-react'
import { useMemo, useState } from 'react'

import { ApiError } from '@/api/client'
import {
  LabelRuleInputKey,
  ListRecordingsOrder,
  RuleTextMatchMode,
  RuleTextMatchTarget,
  listRecordings,
  useGetRecording,
  useListLabelRules,
  useListRecordingUpcoming,
  useListRules,
  type ProgramSearchRequest,
  type Recording,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { LabelRuleForm } from '@/components/label-rule-form'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { RecordingRow } from '@/components/recording-row'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { programsQueryKeyPrefix, recordingsQueryKeyPrefix } from '@/lib/events'
import { formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { collapseUpcoming, isPlayableRecording, type UpcomingRow } from '@/lib/series'

/** hubPageSize は 1 回のフェッチで取る件数（API の既定と同じ）。 */
const hubPageSize = 50

type HubPageParam = { before?: string; beforeId?: number }

/** startAt が無い API 行を壊れた順序として扱わず、id の降順へ落とす。 */
function newestFirst(a: Recording, b: Recording): number {
  const dateDiff = Date.parse(b.startAt) - Date.parse(a.startAt)
  return dateDiff !== 0 ? dateDiff : b.id - a.id
}

/**
 * SeriesHubPage は番組ハブ（`/recordings/$id/series`）。
 *
 * 画面上部は実効シリーズの identity、中央は再生・毎回録画・分類修正の操作、
 * 下部は録画一覧の `RecordingRow` を使ったエピソード一覧という 3 ブロックに分ける。
 * 起点は導出キーではなく録画 id のままにし、分類ルールが変わっても URL の宛先を
 * 失わない。
 */
export function SeriesHubPage() {
  const { id } = useParams({ from: '/recordings/$id/series' })
  const idNum = Number(id)
  const navigate = useNavigate({ from: '/recordings/$id/series' })
  const router = useRouter()
  const [order, setOrder] = useState<ListRecordingsOrder>(ListRecordingsOrder.desc)
  const [labelRuleFormOpen, setLabelRuleFormOpen] = useState(false)

  // 単体 GET の 404 は purge 済みの起点として一覧側の行から復元する。404 以外は
  // retry してからエラーにするので、存在しない録画と一時障害を混同しない。
  const originQuery = useGetRecording(idNum, {
    query: {
      queryKey: [recordingsQueryKeyPrefix, 'detail', idNum] as const,
      retry: (failureCount, error) =>
        !(error instanceof ApiError && error.status === 404) && failureCount < 3,
    },
  })
  const origin = unwrap(originQuery.data)

  const upcomingQuery = useListRecordingUpcoming(idNum, {
    query: { queryKey: [programsQueryKeyPrefix, 'upcoming', idNum] as const },
  })
  const upcoming = useMemo(
    () => collapseUpcoming(unwrap(upcomingQuery.data) ?? []),
    [upcomingQuery.data],
  )

  const listParams = useMemo(
    () => ({ seriesOf: idNum, limit: hubPageSize, order }),
    [idNum, order],
  )
  const listQuery = useInfiniteQuery({
    queryKey: [recordingsQueryKeyPrefix, 'series', idNum, order] as const,
    queryFn: ({ pageParam }: { pageParam: HubPageParam }) =>
      listRecordings({ ...listParams, ...pageParam }),
    initialPageParam: {} as HubPageParam,
    getNextPageParam: (lastPage) => {
      const data = unwrap(lastPage) ?? []
      if (data.length < hubPageSize) return undefined
      const last = data[data.length - 1]
      return { before: last.startAt, beforeId: last.id }
    },
  })
  const recordings = useMemo(
    () => listQuery.data?.pages.flatMap((page) => unwrap(page) ?? []) ?? [],
    [listQuery.data],
  )

  const labelRulesQuery = useListLabelRules()
  const labelRules = useMemo(() => unwrap(labelRulesQuery.data) ?? [], [labelRulesQuery.data])
  const rulesQuery = useListRules()
  const rules = useMemo(() => unwrap(rulesQuery.data) ?? [], [rulesQuery.data])

  // The origin may also appear in the first list page. De-duplicate it before using the
  // loaded episodes for identity, the latest playable row, and automatic-key disclosure.
  const loadedRecordings = useMemo(() => {
    const byID = new Map<number, Recording>()
    if (origin !== undefined) byID.set(origin.id, origin)
    for (const recording of recordings) byID.set(recording.id, recording)
    return [...byID.values()]
  }, [origin, recordings])
  const newestRecordings = useMemo(
    () => [...loadedRecordings].sort(newestFirst),
    [loadedRecordings],
  )
  const latestRecording = newestRecordings[0]
  const latestPlayable = newestRecordings.find(isPlayableRecording)
  const identityRecording = origin ?? latestRecording
  const effectiveSeries =
    identityRecording?.series ??
    newestRecordings.find((recording) => recording.series != null)?.series ??
    undefined
  const manualRule = labelRules.find(
    (rule) =>
      (rule.key ?? LabelRuleInputKey.series) === LabelRuleInputKey.series &&
      rule.valueKey === effectiveSeries,
  )
  const automaticSeriesKeys = useMemo(() => {
    if (manualRule === undefined || effectiveSeries === undefined) return []
    return [
      ...new Set(
        newestRecordings
          .map((recording) => recording.seriesKey)
          .filter((key): key is string => key != null && key !== effectiveSeries),
      ),
    ]
  }, [effectiveSeries, manualRule, newestRecordings])
  const recurringRule = useMemo(() => {
    for (const recording of newestRecordings) {
      if (recording.ruleId === undefined) continue
      const rule = rules.find((candidate) => candidate.id === recording.ruleId)
      if (rule !== undefined) return rule
    }
    return undefined
  }, [newestRecordings, rules])
  const automaticKeyword =
    newestRecordings.find((recording) => recording.seriesKey != null)?.seriesKey ?? undefined
  const recurringKeyword = manualRule?.keyword || automaticKeyword || effectiveSeries
  const recurringCondition = useMemo<ProgramSearchRequest | undefined>(() => {
    if (identityRecording === undefined || recurringKeyword === undefined) return undefined
    return {
      textMatches: [
        {
          target: RuleTextMatchTarget.name,
          mode: RuleTextMatchMode.keyword,
          value: recurringKeyword,
        },
      ],
      // The same name can be broadcast by different services. Keep the origin service
      // in the search so the user creates a rule for this channel only.
      services: [
        {
          networkId: identityRecording.networkId,
          serviceId: identityRecording.serviceId,
        },
      ],
    }
  }, [identityRecording, recurringKeyword])
  const showSite = new Set(loadedRecordings.map((recording) => recording.site)).size > 1

  const originPurged = originQuery.error instanceof ApiError && originQuery.error.status === 404
  const seriesSettled = !listQuery.isPending && !upcomingQuery.isPending
  const nothingToShow =
    originPurged &&
    seriesSettled &&
    !listQuery.isError &&
    !upcomingQuery.isError &&
    loadedRecordings.length === 0 &&
    upcoming.length === 0
  const showHub = origin !== undefined || (originPurged && seriesSettled)

  const goBack = () => {
    if (router.history.canGoBack()) {
      router.history.back()
    } else if (originPurged) {
      void navigate({ to: '/recordings' })
    } else {
      void navigate({ to: '/recordings/$id', params: { id: String(idNum) } })
    }
  }

  return (
    <>
      <PageHeader
        title="シリーズ"
        leading={
          <Button type="button" variant="ghost" size="icon" aria-label="戻る" onClick={goBack}>
            <ArrowLeft />
          </Button>
        }
      />

      {(originQuery.isError && !originPurged) || nothingToShow ? (
        <ErrorState>録画が見つかりません</ErrorState>
      ) : !showHub ? (
        <ListSkeleton rows={4} />
      ) : (
        <PageContent className="flex flex-col gap-8 px-4 py-4">
          <section aria-label="シリーズ情報" className="flex items-start gap-3">
            {latestRecording !== undefined && (
              <SeriesThumbnail key={latestRecording.id} recording={latestRecording} />
            )}
            <div className="min-w-0">
              {effectiveSeries !== undefined && (
                <div className="flex flex-wrap items-center gap-2">
                  <h2 className="truncate text-lg font-medium">{effectiveSeries}</h2>
                  {manualRule !== undefined && (
                    <span className="shrink-0 rounded border border-border px-1 text-xs text-muted-foreground">
                      手動
                    </span>
                  )}
                </div>
              )}
              {manualRule !== undefined && automaticSeriesKeys.length > 0 && (
                <p className="mt-1 text-sm text-muted-foreground">
                  自動: {automaticSeriesKeys[0]}
                  {automaticSeriesKeys.length > 1 && `（ほか ${automaticSeriesKeys.length - 1}）`}
                </p>
              )}
              {latestRecording !== undefined && (
                <p className="mt-1 text-xs text-muted-foreground">
                  最新 {formatDateTime(latestRecording.startAt)}
                </p>
              )}
            </div>
          </section>

          <section aria-label="シリーズの操作" className="flex flex-col gap-4">
            <div className="flex flex-wrap items-center gap-2">
              {latestPlayable !== undefined && (
                <Button
                  render={
                    <Link
                      to="/recordings/$id"
                      params={{ id: String(latestPlayable.id) }}
                      aria-label="最新話を再生"
                    />
                  }
                >
                  最新話を再生
                </Button>
              )}
              {recurringRule !== undefined ? (
                <Button
                  variant="outline"
                  render={<Link to="/search" search={{ ruleId: recurringRule.id }} />}
                >
                  ルール「{recurringRule.name}」で毎回録画中
                </Button>
              ) : (
                recurringCondition !== undefined && (
                  <Button
                    variant="outline"
                    render={<Link to="/search" search={{ cond: recurringCondition }} />}
                  >
                    毎回録画する
                  </Button>
                )
              )}
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      aria-label="シリーズのその他の操作"
                    />
                  }
                >
                  <MoreVertical />
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onClick={() => setLabelRuleFormOpen(true)}>
                    分類を直す（割る・指定する）
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>

            {(upcomingQuery.isError || upcoming.length > 0) && (
              <section className="flex flex-col gap-2" aria-label="次回">
                <h3 className="text-sm font-medium text-muted-foreground">次回</h3>
                {upcomingQuery.isError ? (
                  <ErrorState onRetry={() => void upcomingQuery.refetch()}>
                    次回の取得に失敗しました
                  </ErrorState>
                ) : (
                  <ul className="flex flex-col gap-2">
                    {upcoming.map((row) => (
                      <li key={`${row.networkId}:${row.serviceId}:${row.startAt}`}>
                        <UpcomingRowItem row={row} />
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            )}
          </section>

          <section aria-label="このシリーズの録画" className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-base font-medium">エピソード</h2>
              <div
                role="group"
                aria-label="エピソードの並び順"
                className="flex items-center gap-1"
              >
                <Button
                  type="button"
                  size="sm"
                  variant={order === ListRecordingsOrder.desc ? 'secondary' : 'ghost'}
                  aria-pressed={order === ListRecordingsOrder.desc}
                  onClick={() => setOrder(ListRecordingsOrder.desc)}
                >
                  新しい順
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant={order === ListRecordingsOrder.asc ? 'secondary' : 'ghost'}
                  aria-pressed={order === ListRecordingsOrder.asc}
                  onClick={() => setOrder(ListRecordingsOrder.asc)}
                >
                  古い順
                </Button>
              </div>
            </div>
            {listQuery.isError ? (
              <ErrorState onRetry={() => void listQuery.refetch()}>
                録画の取得に失敗しました
              </ErrorState>
            ) : listQuery.isPending ? (
              <ListSkeleton />
            ) : recordings.length === 0 ? (
              <EmptyState>このシリーズの録画はありません</EmptyState>
            ) : (
              <ul className="flex flex-col">
                {recordings.map((recording) => (
                  <li key={recording.id}>
                    <RecordingRow recording={recording} showSite={showSite} />
                  </li>
                ))}
              </ul>
            )}
            {listQuery.hasNextPage && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="self-start"
                disabled={listQuery.isFetchingNextPage}
                onClick={() => void listQuery.fetchNextPage()}
              >
                さらに読み込む
              </Button>
            )}
          </section>
        </PageContent>
      )}

      {labelRuleFormOpen && (
        <LabelRuleForm
          open={labelRuleFormOpen}
          onOpenChange={setLabelRuleFormOpen}
          initial={{ key: LabelRuleInputKey.series, value: effectiveSeries ?? undefined }}
        />
      )}
    </>
  )
}

/** ハブの identity に置く最新録画のサムネイル。未生成・404 は静かにプレースホルダーへ落とす。 */
function SeriesThumbnail({ recording }: { recording: Recording }) {
  const [failed, setFailed] = useState(false)

  return (
    <div className="size-20 shrink-0 overflow-hidden rounded bg-muted sm:size-28">
      {!failed ? (
        <img
          src={`/api/media/recordings/${recording.id}/thumbnail`}
          alt=""
          className="size-full object-cover"
          loading="lazy"
          onError={() => setFailed(true)}
        />
      ) : (
        <div className="size-full bg-muted" aria-hidden />
      )}
    </div>
  )
}

/** UpcomingRowItem は「次回」の 1 行。同じ放送は `collapseUpcoming` で 1 行になる。 */
function UpcomingRowItem({ row }: { row: UpcomingRow }) {
  return (
    <div className="flex flex-col gap-1 rounded-lg border border-border p-3">
      <span className="text-sm text-foreground">{programTitle(row.name)}</span>
      <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
        <span>{formatDateTime(row.startAt)}</span>
        <span>{formatDuration(row.durationMs)}</span>
        {row.sites.map((site) => (
          <span key={site} className="rounded bg-muted px-1.5 py-0.5 text-foreground">
            {site}
          </span>
        ))}
      </span>
    </div>
  )
}
