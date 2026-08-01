import { useQueryClient, useQueries } from '@tanstack/react-query'
import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getGetProgramQueryOptions,
  getListRulesQueryKey,
  useCreateRule,
  useGetRule,
  useListServices,
  useSearchPrograms,
  type ProgramListItem,
  type Rule,
  type Service,
} from '@/api/generated'
import { ApiError } from '@/api/client'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { ConditionFields } from '@/components/condition-fields'
import { EncodeSettingsFields } from '@/components/encode-settings-fields'
import { EmptyState, ErrorState, ListSkeleton, PageHeader, Skeleton } from '@/components/page'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/field'
import { formatDateTime, formatDuration } from '@/lib/format'
import { DEFAULT_SITE } from '@/lib/site'
import { cn } from '@/lib/utils'
import {
  buildRuleInput,
  buildSearchRequest,
  draftError,
  emptyDraft,
  emptyRuleMeta,
  ruleMetaError,
  ruleToDraft,
  ruleToMeta,
  type RuleMetaDraft,
  type SearchDraft,
} from '@/lib/program-search'

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
 * この画面は 2 方向で `rules` と繋がる（M2-11 の続き）:
 * - この条件をそのまま `POST /api/rules` に送って新しいルールを作る（下の
 *   `CreateRuleSection`）
 * - `?ruleId=N` で開くと、そのルールの条件を下書きに写して自動検索する
 *   （下の `RuleSourceBanner` とハイドレーション effect）。ここでの編集は
 *   `ruleId` のルールを一切変更しない — 保存すれば別の新しいルールになる
 *
 * 結果は表示のみ（予約操作を持たない）。理由は下の `SearchResultRow` を参照。
 */
export function SearchPage() {
  const [draft, setDraft] = useState<SearchDraft>(emptyDraft)
  const [visibleCount, setVisibleCount] = useState(pageSize)
  const services = useListServices(DEFAULT_SITE)
  const search = useSearchPrograms()

  const routeSearch = useRouteSearch({ from: '/search' })
  const ruleId = routeSearch.ruleId
  // ruleId が無いときは問い合わせを止める。useGetRule は id を必須の number で
  // 取るため、無効化中はダミー値を渡す（program-overlap-warning.tsx と同じ流儀）。
  const ruleQuery = useGetRule(ruleId ?? -1, { query: { enabled: ruleId !== undefined } })
  const sourceRule = unwrap(ruleQuery.data)

  const serviceList = useMemo(() => unwrap(services.data) ?? [], [services.data])
  const serviceById = useMemo(() => {
    const map = new Map<number, Service>()
    for (const s of serviceList) map.set(s.serviceId, s)
    return map
  }, [serviceList])

  // search（useMutation の戻り値）を毎レンダー新しいオブジェクトのまま
  // ハイドレーション effect の依存に置くと、ユーザーが 1 文字打つたびに
  // （setDraft → 再レンダー → search 参照更新 → 依存変化）effect が動いてしまい、
  // ガードの実装を 1 行間違えるだけで「打つたびに下書きが巻き戻る」
  // 無限ループに退化する。ref 経由の最新値参照にして、そもそも依存に入れない。
  const searchRef = useRef(search)
  searchRef.current = search

  // ハイドレーションは 1 回だけ。ref に「最後にハイドレートした ruleId」を持ち、
  // 同じ ruleId のまま（refetch でオブジェクトの参照が変わっただけ）なら
  // 何もしない。これが無いと、ユーザーが下書きを編集したあとの再レンダー
  // （invalidate / refetch）で入力が巻き戻る。
  const hydratedRuleIdRef = useRef<number | undefined>(undefined)
  useEffect(() => {
    if (ruleId === undefined) return
    if (sourceRule === undefined) return
    if (hydratedRuleIdRef.current === ruleId) return
    hydratedRuleIdRef.current = ruleId

    const nextDraft = ruleToDraft(sourceRule)
    setDraft(nextDraft)
    setVisibleCount(pageSize)
    searchRef.current.mutate({ site: DEFAULT_SITE, data: buildSearchRequest(nextDraft) })
  }, [ruleId, sourceRule])

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

      {ruleId !== undefined && (
        <RuleSourceBanner
          ruleId={ruleId}
          rule={sourceRule}
          isPending={ruleQuery.isPending}
          isError={ruleQuery.isError}
          error={ruleQuery.error}
        />
      )}

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

      <CreateRuleSection draft={draft} draftError={error} sourceRule={sourceRule} />

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
 * RuleSourceBanner は `?ruleId=N` で開いたときの由来表示。
 *
 * 読み込み中・404・その他の失敗・成功を区別する。存在しない ruleId で
 * 無言の空白（フォームが何事もなく空のまま出る）にしないため、失敗も明示する。
 */
function RuleSourceBanner({
  ruleId,
  rule,
  isPending,
  isError,
  error,
}: {
  ruleId: number
  rule: Rule | undefined
  isPending: boolean
  isError: boolean
  error: unknown
}) {
  if (isPending) {
    return (
      <div className="border-b border-border bg-muted/40 px-4 py-2 text-xs text-muted-foreground">
        ルール #{ruleId} の条件を読み込み中…
      </div>
    )
  }

  if (isError) {
    // ApiError.status を見て 404 と他の失敗（ネットワーク断など）を区別する。
    // どちらも「無言の空白」にはしない
    const notFound = error instanceof ApiError && error.status === 404
    return (
      <div
        role="alert"
        className="border-b border-border bg-muted/40 px-4 py-2 text-xs text-destructive"
      >
        {notFound
          ? `ルール #${ruleId} が見つかりません（削除された可能性があります）`
          : (apiErrorMessage(error) ?? `ルール #${ruleId} の取得に失敗しました`)}
      </div>
    )
  }

  if (rule === undefined) return null

  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-muted/40 px-4 py-2 text-xs">
      <span>
        ルール「{rule.name}」の条件を表示中です。ここでの変更はこのルールを更新しません。保存すると別の新しいルールが作成されます。
      </span>
      <Button type="button" variant="outline" size="sm" render={<Link to="/rules" />}>
        ルール一覧に戻る
      </Button>
    </div>
  )
}

/**
 * CreateRuleSection は「この条件でルールを作成」の入口。
 *
 * 折りたたみ式にしているのは、条件を試しているだけのユーザー（この画面の主用途）
 * にメタ情報の入力欄を常時見せないため。
 */
function CreateRuleSection({
  draft,
  draftError: draftHasError,
  sourceRule,
}: {
  draft: SearchDraft
  draftError: string | undefined
  sourceRule: Rule | undefined
}) {
  const [open, setOpen] = useState(false)

  if (!open) {
    return (
      <div className="border-b border-border px-4 py-4">
        <Button
          type="button"
          variant="outline"
          size="lg"
          className="w-full"
          disabled={draftHasError !== undefined}
          onClick={() => setOpen(true)}
        >
          この条件でルールを作成
        </Button>
      </div>
    )
  }

  return (
    <div className="border-b border-border px-4 py-4">
      <CreateRuleForm
        draft={draft}
        draftHasError={draftHasError !== undefined}
        sourceRule={sourceRule}
        onCancel={() => setOpen(false)}
        onDone={() => setOpen(false)}
      />
    </div>
  )
}

/**
 * CreateRuleForm は条件以外のメタ情報（名前・有効・優先度・エンコード設定）を
 * 入力して `POST /api/rules` に送る。
 *
 * `sourceRule` が渡っているとき（`?ruleId=N` から開いた場合）は、名前・有効・
 * 優先度・エンコード設定の初期値と、UI を持たない項目（sites 等）をそのルールから
 * 引き継ぐ（`buildRuleInput` の `preserve`）。単なる検索条件からの推測ではなく
 * 実在するルールの値なので、不変条件 10 が禁じる「試していない次元を黙って
 * 埋める」には当たらない —— ユーザーが実際に開いたルールをフォークしている。
 */
function CreateRuleForm({
  draft,
  draftHasError,
  sourceRule,
  onCancel,
  onDone,
}: {
  draft: SearchDraft
  draftHasError: boolean
  sourceRule: Rule | undefined
  onCancel: () => void
  onDone: () => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const createRule = useCreateRule()

  const [meta, setMeta] = useState<RuleMetaDraft>(() =>
    sourceRule ? ruleToMeta(sourceRule) : emptyRuleMeta(),
  )
  // 「全番組が対象になる」ことを理解した上での作成かどうか。条件を追加すれば
  // このチェックは意味を失うが、外れたままでも実害はない（次の保存試行時に
  // 改めて noConditions を評価するだけ）。
  const [confirmedEmpty, setConfirmedEmpty] = useState(false)

  const metaError = ruleMetaError(meta)
  const request = buildSearchRequest(draft)
  const noConditions = Object.keys(request).length === 0
  const hasPeriod = draft.periodStartAt !== '' || draft.periodEndAt !== ''
  const pending = createRule.isPending

  const blocked =
    draftHasError || metaError !== undefined || (noConditions && !confirmedEmpty) || pending

  const save = () => {
    if (blocked) return
    const input = buildRuleInput(draft, meta, sourceRule)
    createRule.mutate(
      { data: input },
      {
        onSuccess: () => {
          toast({ message: 'ルールを作成しました' })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
          onDone()
          void navigate({ to: '/rules' })
        },
        onError: (err) => toast({ message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました' }),
      },
    )
  }

  return (
    <form
      aria-label="この条件でルールを作成"
      className="flex flex-col gap-4 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        save()
      }}
    >
      {hasPeriod && (
        <p className="text-xs text-amber-700 dark:text-amber-500">
          期間を指定したまま作成すると、ルールの恒久的な期間制限になります。「いまだけ絞り込みたい」
          場合は、上の条件フォームで期間を空にしてから作成してください。
        </p>
      )}

      {noConditions && (
        <div className="flex flex-col gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5">
          <p role="alert" className="text-xs text-destructive">
            条件を 1 つも指定していません。このまま作成すると、放送されるすべての番組が対象になります。
          </p>
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              type="checkbox"
              className="size-4 accent-primary"
              checked={confirmedEmpty}
              disabled={pending}
              onChange={(e) => setConfirmedEmpty(e.target.checked)}
            />
            すべての番組が対象になることを理解した上で作成します
          </label>
        </div>
      )}

      <Field label="名前">
        <Input
          value={meta.name}
          disabled={pending}
          onChange={(e) => setMeta((m) => ({ ...m, name: e.target.value }))}
          placeholder="例: ニュース全部"
          required
        />
      </Field>

      <div className="flex flex-wrap items-center gap-4">
        <label
          className={cn(
            'flex items-center gap-2 text-sm',
            pending && 'pointer-events-none opacity-50',
          )}
        >
          <input
            type="checkbox"
            className="size-4 accent-primary"
            checked={meta.enabled}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, enabled: e.target.checked }))}
          />
          有効
        </label>
        <Field label="優先度" className="w-28">
          <Input
            type="number"
            min={0}
            value={meta.priority}
            disabled={pending}
            onChange={(e) => setMeta((m) => ({ ...m, priority: e.target.value }))}
          />
        </Field>
      </div>

      <EncodeSettingsFields
        value={{ keepOriginal: meta.keepOriginal, encodeProfiles: meta.encodeProfiles }}
        onChange={(next) => setMeta((m) => ({ ...m, ...next }))}
        disabled={pending}
      />

      {metaError !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {metaError}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="lg" disabled={blocked}>
          {pending ? '作成中…' : 'ルールを作成'}
        </Button>
        <Button type="button" variant="outline" size="lg" disabled={pending} onClick={onCancel}>
          キャンセル
        </Button>
      </div>
    </form>
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
