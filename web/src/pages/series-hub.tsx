import { useInfiniteQuery } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { useMemo } from 'react'

import { ApiError } from '@/api/client'
import {
  listRecordings,
  useGetRecording,
  useListRecordingUpcoming,
  type Recording,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageContent, PageHeader } from '@/components/page'
import { IngestBadge, StatusBadge } from '@/components/recording-badges'
import { Button } from '@/components/ui/button'
import { programsQueryKeyPrefix, recordingsQueryKeyPrefix } from '@/lib/events'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { collapseUpcoming, type UpcomingRow } from '@/lib/series'

/** hubPageSize は 1 回のフェッチで取る件数（API の既定と同じ）。 */
const hubPageSize = 50

type HubPageParam = { before?: string; beforeId?: number }

/**
 * SeriesHubPage は番組ハブ（`/recordings/$id/series`）。
 *
 * 録画から同じシリーズの他の回へ辿る手段が無かったので、シリーズの棚（M8-5）から
 * 1 つを開いた画面として置く。上に「次回」（これから放送される回）、下に
 * そのシリーズの録画を並べる。
 *
 * **起点は録画 id**（正規化キーではない）。正規化キーを URL に置くと、規則を
 * 変えた時点で 404 ではなく 0 件で黙って壊れる（docs/data/series.md §8
 * 「資源同定: 起点は録画 id」）。
 *
 * **起点の単体 GET が 404 でも、ハブは開く。** 単体 GET は purged の tombstone を
 * 除く契約なので（`queryRecordingByID`）、ハブを開いたブックマークの起点が後から
 * purge されると 404 になる。この場合も一覧（`?seriesOf=`）と次回は tombstone の
 * 行からシリーズを返すので、404 を「シリーズが無い」と読まずに一覧と次回を出す。
 * **存在しない id との区別は API が返す情報だけで付く** --- 行が無ければ一覧も
 * 次回も 0 件になる（openapi.yaml の `seriesOf` / `upcoming` 参照）。404 以外の
 * エラー（5xx など）は purged と見なさない。
 *
 * 見出しは**起点の録画の生のタイトル**（正規化キーではない）。値は正規化の
 * 産物なので表示名にならない（棚の見出しと同じ規律）。起点が purged なら一覧の
 * 先頭（最も新しい回）、一覧も空なら次回の先頭の生のタイトルで代用する。
 * 起点を purge した後にシリーズの他の回が 1 つも無い場合（その回しか録っていない
 * シリーズ）は、出せる一覧も次回も無いので見出しを諦めて「録画が見つかりません」
 * を出す（受け入れた限界）。
 *
 * **シリーズが無い録画（`series` が null）では導線を出さない**ので、この画面は
 * そこからは到達しない。直接 URL を叩かれた場合は「0 件」の画面になる
 * （サーバーは `?seriesOf=` に 0 件を返す。openapi.yaml 参照）。
 */
export function SeriesHubPage() {
  const { id } = useParams({ from: '/recordings/$id/series' })
  const idNum = Number(id)

  // 起点の録画。単体ページと同じクエリキーにする（`recordingsQueryKeyPrefix`
  // で前方一致するので、削除・エンコード追加などの mutate が自動で巻き込む。
  // pages/recording-detail.tsx と同じ理由）。
  const originQuery = useGetRecording(idNum, {
    query: { queryKey: [recordingsQueryKeyPrefix, 'detail', idNum] as const },
  })
  const origin = unwrap(originQuery.data)

  // 次回。キーの先頭を `programsQueryKeyPrefix` に揃える --- 分類ルールを
  // 変えた直後（label_rules のトリガーが recordings トピックへ流す）に、
  // 開いているハブが自動で更新されるのはこの接頭辞の経路である
  // （lib/events.ts の recordings グループが programsQueryKeyPrefix も
  // invalidate する）。
  const upcomingQuery = useListRecordingUpcoming(idNum, {
    query: { queryKey: [programsQueryKeyPrefix, 'upcoming', idNum] as const },
  })
  const upcoming = useMemo(
    () => collapseUpcoming(unwrap(upcomingQuery.data) ?? []),
    [upcomingQuery.data],
  )

  const listParams = useMemo(() => ({ seriesOf: idNum, limit: hubPageSize }), [idNum])
  const listQuery = useInfiniteQuery({
    // 先頭要素を `recordingsQueryKeyPrefix` に揃える（一覧・単体ページと同じ
    // 前方一致の規律）。
    queryKey: [recordingsQueryKeyPrefix, 'series', idNum] as const,
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

  // 起点が purged の tombstone（単体 GET が 404）。存在しない id とは分けて扱う。
  const originPurged = originQuery.error instanceof ApiError && originQuery.error.status === 404
  // 一覧と次回が確定するまで「0 件」を判断しない（pending は空配列と同じ形）。
  const seriesSettled = !listQuery.isPending && !upcomingQuery.isPending
  // 起点が purged で、行が残っている証拠がどこにも無い場合だけ「見つかりません」。
  // どちらかがエラーなら 0 件は判断材料にならないので、節ごとのエラー表示に委ねる。
  const nothingToShow =
    originPurged &&
    seriesSettled &&
    !listQuery.isError &&
    !upcomingQuery.isError &&
    recordings.length === 0 &&
    upcoming.length === 0
  const showHub = origin !== undefined || (originPurged && seriesSettled)
  const heading =
    origin !== undefined
      ? programTitle(origin.title)
      : recordings.length > 0
        ? programTitle(recordings[0].title)
        : upcoming.length > 0
          ? programTitle(upcoming[0].name)
          : undefined

  return (
    <>
      <PageHeader
        title="シリーズ"
        leading={
          <Button
            variant="ghost"
            size="icon"
            aria-label="戻る"
            // 起点が purged なら戻り先の詳細も 404 になるので、一覧へ向ける。
            render={
              originPurged ? (
                <Link to="/recordings" />
              ) : (
                <Link to="/recordings/$id" params={{ id: String(idNum) }} />
              )
            }
          >
            <ArrowLeft />
          </Button>
        }
      />

      {originQuery.isError && !originPurged ? (
        <ErrorState>録画が見つかりません</ErrorState>
      ) : nothingToShow ? (
        <ErrorState>録画が見つかりません</ErrorState>
      ) : !showHub ? (
        <ListSkeleton rows={4} />
      ) : (
        <PageContent className="flex flex-col gap-4 px-4 py-4">
          {heading !== undefined && <h2 className="text-lg font-medium">{heading}</h2>}

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

          <section className="flex flex-col gap-2" aria-label="このシリーズの録画">
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
                    <SeriesRecordingRow recording={recording} />
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
    </>
  )
}

/**
 * UpcomingRowItem は「次回」の 1 行。**同じ放送は 1 行にまとまっている**
 * （`collapseUpcoming`）。site はチップで出す（表示だけ。同じ放送が 2 拠点の
 * EPG にあることを見せる）。
 */
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

/**
 * SeriesRecordingRow はハブの録画の 1 行。行本体が詳細への全面リンク
 * （録画一覧の `RecordingRow` と同じ配置文法）。一覧側の行は編集モード・
 * カード表示・一括操作を抱えるので共有しない --- ハブは選択を持たない。
 */
function SeriesRecordingRow({ recording }: { recording: Recording }) {
  return (
    <div className="relative flex min-h-14 flex-col justify-center gap-1 border-b border-border px-1 py-2.5 hover:bg-muted/40">
      <Link
        to="/recordings/$id"
        params={{ id: String(recording.id) }}
        aria-label={programTitle(recording.title)}
        className="absolute inset-0"
      />
      <span className="truncate text-sm">{programTitle(recording.title)}</span>
      <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
        <StatusBadge status={recording.status} />
        <IngestBadge recording={recording} />
        <span className="shrink-0">{recording.serviceName}</span>
        <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
        {recording.sizeBytes !== undefined && (
          <span className="shrink-0">{formatBytes(recording.sizeBytes)}</span>
        )}
      </span>
    </div>
  )
}
