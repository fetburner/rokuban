import { useQueries } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import {
  getGetProgramQueryOptions,
  useListServices,
  useSearchPrograms,
  type ProgramListItem,
  type Service,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { ConditionFields } from '@/components/condition-fields'
import { EmptyState, ErrorState, ListSkeleton, PageHeader, Skeleton } from '@/components/page'
import { Button } from '@/components/ui/button'
import { formatDateTime, formatDuration } from '@/lib/format'
import { DEFAULT_SITE } from '@/lib/site'
import { buildSearchRequest, draftError, emptyDraft, type SearchDraft } from '@/lib/program-search'

/**
 * pageSize は一度に詳細を取りに行く結果の件数。
 *
 * 検索 API が返すのは programId の配列だけなので、1 件表示するたびに
 * `GET /api/programs/{id}` が必要になる。数百件を一斉に取りに行かないよう区切る
 * （API に一括取得がないことの申し送りは issue #24 のコメント）。
 */
const pageSize = 30

/**
 * SearchPage は EPG をルールと同じ条件で検索する画面。
 *
 * 検索 API（`POST /api/programs/search`）は ruler 評価と同じコンパイラを通るため、
 * ここで出る番組はルールにしたときにマッチする番組と一致する（M2-2）。
 * つまりこの画面の役目は「条件をルールとして保存する前に試すこと」であり、
 * 番組表（`/`）と関心事が違うので独立したルートに置いている。
 *
 * 結果は表示のみ（予約操作を持たない）。理由は下の `SearchResultRow` を参照。
 */
export function SearchPage() {
  const [draft, setDraft] = useState<SearchDraft>(emptyDraft)
  const [visibleCount, setVisibleCount] = useState(pageSize)
  const services = useListServices(DEFAULT_SITE)
  const search = useSearchPrograms()

  const serviceList = useMemo(() => unwrap(services.data) ?? [], [services.data])
  const serviceById = useMemo(() => {
    const map = new Map<number, Service>()
    for (const s of serviceList) map.set(s.serviceId, s)
    return map
  }, [serviceList])

  const error = draftError(draft)

  // 送れない下書きは 2 つの層で止める。ボタンの無効化だけだと、Enter による
  // 暗黙の送信（既定ボタンが無効でも submit が届きうる）が素通りする。
  const submit = () => {
    if (error !== undefined) return
    setVisibleCount(pageSize)
    search.mutate({ site: DEFAULT_SITE, data: buildSearchRequest(draft) })
  }

  const ids = unwrap(search.data) ?? []

  return (
    <>
      <PageHeader title="検索" />

      <form
        aria-label="検索条件"
        className="flex flex-col gap-5 border-b border-border px-4 py-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <ConditionFields draft={draft} onChange={setDraft} />

        <div className="flex flex-col gap-2">
          {/* 送れない理由は押せないボタンの隣に出す。ボタンだけ無効にすると
              「なぜ押せないのか」が分からない */}
          {error !== undefined && (
            <p role="alert" className="text-xs text-destructive">
              {error}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="submit" size="lg" disabled={error !== undefined || search.isPending}>
              {search.isPending ? '検索中…' : '検索'}
            </Button>
            <Button
              type="button"
              variant="outline"
              size="lg"
              onClick={() => {
                setDraft(emptyDraft())
                search.reset()
              }}
            >
              条件をクリア
            </Button>
          </div>
        </div>
      </form>

      {search.isIdle ? (
        // 「まだ検索していない」と「0 件」は別の事実。同じ文言にすると
        // 条件の書き方が悪いのか該当がないのかが分からない
        <EmptyState>条件を指定して検索してください</EmptyState>
      ) : search.isPending ? (
        <ListSkeleton />
      ) : search.isError ? (
        <SearchError error={search.error} />
      ) : ids.length === 0 ? (
        <EmptyState>条件に一致する番組がありません</EmptyState>
      ) : (
        <>
          <p className="px-4 py-2 text-xs text-muted-foreground">
            {/* 件数は 1 つの文字列にする（JSX で連結するとテキストノードが分かれ、
                読み上げも「37」「件」と切れる） */}
            {visibleCount < ids.length
              ? `${ids.length} 件（番組 ID 順）— ${visibleCount} 件を表示`
              : `${ids.length} 件（番組 ID 順）`}
          </p>
          <SearchResultList ids={ids.slice(0, visibleCount)} serviceById={serviceById} />
          {visibleCount < ids.length && (
            <div className="px-4 py-6">
              <Button
                type="button"
                variant="outline"
                size="lg"
                className="w-full"
                onClick={() => setVisibleCount((c) => c + pageSize)}
              >
                さらに表示
              </Button>
            </div>
          )}
        </>
      )}
    </>
  )
}

/**
 * SearchError は検索の失敗を表示する。
 *
 * **サーバーのメッセージをそのまま見せる。** 不正な正規表現は 400 で理由が返る
 * （`invalid regex ... (POSIX ARE; lookbehind is not supported)`）ので、これを
 * 隠して汎用の文言に落とすと「書き方が悪い」のか「該当なし」なのかを
 * ユーザーが区別できない。
 */
function SearchError({ error }: { error: unknown }) {
  const message = apiErrorMessage(error)
  return (
    <ErrorState>
      <span className="block">検索に失敗しました</span>
      {message !== undefined && (
        <span className="mt-1 block break-all font-mono text-xs">{message}</span>
      )}
    </ErrorState>
  )
}

/**
 * SearchResultList は programId の一覧を番組の行にする。
 *
 * 検索 API は programId しか返さないため、行ごとに
 * `GET /api/programs/{id}` を引く。`useQueries` で 1 箇所にまとめているのは、
 * 行コンポーネントに hook を置くと「行が消えるとクエリも消える」形になり、
 * 表示件数を増やしたときの取得状態が追いにくくなるため。
 */
function SearchResultList({
  ids,
  serviceById,
}: {
  ids: number[]
  serviceById: Map<number, Service>
}) {
  const details = useQueries({
    queries: ids.map((id) => getGetProgramQueryOptions(DEFAULT_SITE, id)),
  })

  return (
    <ul data-testid="search-results">
      {ids.map((id, i) => {
        const detail = details[i]
        const program = unwrap(detail?.data)
        return (
          <li key={id}>
            {program !== undefined ? (
              <SearchResultRow
                program={program}
                serviceName={serviceById.get(program.serviceId)?.name}
              />
            ) : detail?.isError ? (
              // 取得できなかった行を黙って落とさない。EPG のローリング
              // ウィンドウから抜けた番組が検索結果に残ることは実際に起きる
              <p className="border-b border-border px-4 py-3 text-xs text-destructive">
                番組 #{id} の詳細を取得できませんでした
              </p>
            ) : (
              <div className="border-b border-border px-4 py-2.5">
                <Skeleton className="h-9" />
              </div>
            )}
          </li>
        )
      })}
    </ul>
  )
}

/**
 * SearchResultRow は結果 1 件。番組リスト（components/program-row.tsx）と
 * 同じ語彙で描く。
 *
 * 予約ボタンを持たないのは、この画面が「条件を試す」ためのものだから。
 * `ProgramRow` をそのまま使うには (a) 予約操作を持ち込むか (b) 操作列を
 * 省けるように作り替えるかのどちらかが必要で、(a) は `programs.tsx` の
 * `useReservationActions` の複製、(b) は並行作業中のコンポーネントの改変になる。
 * どちらも M2-11 の範囲外なので申し送りにしてある。
 *
 * 時刻ではなく日時を出す。結果は programId 昇順（API の契約）で時刻順ではないため、
 * 番組リストのような日付ヘッダでは日付が繰り返し現れて意味を失う。
 */
function SearchResultRow({
  program,
  serviceName,
}: {
  program: ProgramListItem
  serviceName?: string
}) {
  return (
    <div className="flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5">
      <div className="w-20 shrink-0 text-sm tabular-nums">{formatDateTime(program.startAt)}</div>
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm">{program.name}</div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          {serviceName !== undefined && <span className="truncate">{serviceName}</span>}
          <span className="shrink-0">{formatDuration(program.durationMs)}</span>
          {!program.isFree && <span className="shrink-0">有料</span>}
        </div>
      </div>
    </div>
  )
}