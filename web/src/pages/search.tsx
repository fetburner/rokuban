import { useQueryClient, useQueries } from '@tanstack/react-query'
import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getGetProgramQueryOptions,
  getGetRuleQueryKey,
  getListRulesQueryKey,
  useCreateRule,
  useGetRule,
  useListServices,
  useSearchPrograms,
  useUpdateRule,
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
import { useCurrentSite } from '@/lib/site'
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
import { epgWindowDays, estimateRuleCost, ruleCostWeekDays } from '@/lib/rule-cost'

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
 *   `CreateRuleSection`）。`?ruleId` を伴わない通常の検索からの経路
 * - `?ruleId=N` で開くと、そのルールの条件を下書きに写して自動検索する
 *   （下の `RuleSourceBanner` とハイドレーション effect）。マッチする番組を見ながら
 *   条件を詰められるこの画面が実質のルール編集画面になるため、保存の主動作は
 *   `PATCH /api/rules/{id}` による**上書き**にしている（下の `RuleEditSection`）。
 *   元のルールを残したまま別のルールとして保存する経路も副動作として残す
 *
 * 結果は表示のみ（予約操作を持たない）。理由は下の `SearchResultRow` を参照。
 */
export function SearchPage() {
  const site = useCurrentSite()
  const [draft, setDraft] = useState<SearchDraft>(emptyDraft)
  const [visibleCount, setVisibleCount] = useState(pageSize)
  const services = useListServices(site)
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
    searchRef.current.mutate({ site, data: buildSearchRequest(nextDraft) })
    // site は SiteGate が解決した後は再マウントまで変わらない。ruleId /
    // sourceRule だけを見るガード（上記コメント）が効くよう依存には入れず、
    // ESLint 警告を消すためだけに include するのは避ける。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ruleId, sourceRule])

  const error = draftError(draft)

  /**
   * resultsRef は「押した結果」の先頭（`検索結果` セクション）。主操作を条件
   * フォームの先頭に上げた代償として、押しても折り目の中では何も変わらない
   * （件数・値札・結果はチップ列全部の下）状態になったため、送信のたびに
   * ここへスクロールとフォーカスを移す。
   *
   * **実測値**（390x844・テキスト条件「ニュース」・結果 20 件）: 移す前は
   * クリック後も `window.scrollY = 0` のままで、件数行は y=1179（折り目 844 の
   * 335px 下）。移した後は `scrollY = 1130` で件数行 y=49・結果 1 件目 y=81。
   * 実ブラウザでの合否判定は `web/e2e/search-mobile.mjs` の④。
   *
   * **移動先は結果の先頭（件数行）で、値札と保存はその直前に残す。** 値札を
   * 保存の隣に常置する判断（`RuleCostSummary` のコメント）を動かさずに、
   * 押した結果を折り目に入れるため。同じ実測で値札は y=-68、
   * 「この条件でルールを作成」は y=-4 --- 折り目の外だが数十 px 上にあり、
   * 保存へ戻る動線では従来どおり両者が並んで目に入る。
   *
   * `pendingResultScrollRef` で「ユーザーが押した送信」だけに限る ---
   * `?ruleId=N` で開いたときの自動検索（上のハイドレーション effect）で
   * 飛ばすと、ページを開いた瞬間に条件フォームが画面外へ出てしまう。
   */
  const resultsRef = useRef<HTMLElement>(null)
  const pendingResultScrollRef = useRef(false)

  // 送れない下書きは 2 つの層で止める。ボタンの無効化だけだと、Enter による
  // 暗黙の送信（既定ボタンが無効でも submit が届きうる）が素通りする。
  const submit = () => {
    if (error !== undefined) return
    setVisibleCount(pageSize)
    pendingResultScrollRef.current = true
    search.mutate({ site, data: buildSearchRequest(draft) })
  }

  // 検索が決着した（成功・失敗のどちらでも）フレームで移す。0 件・失敗も
  // 「押した結果」なので、成功だけに絞らない。`status` を依存に置くのは
  // 2 回目の検索でも pending → success と遷移して effect が再び走るため。
  useEffect(() => {
    if (!pendingResultScrollRef.current) return
    if (search.status === 'idle' || search.status === 'pending') return
    pendingResultScrollRef.current = false
    const el = resultsRef.current
    if (el === null) return
    // jsdom は scrollIntoView を実装していない（test/setup.ts でスタブ）。
    el.scrollIntoView({ block: 'start' })
    // 読み上げにも「押した結果」の位置を伝える。スクロールは上で済んでいるので
    // preventScroll でブラウザ既定の再スクロール（scroll-margin を無視する）を止める。
    el.focus({ preventScroll: true })
  }, [search.status])

  const ids = unwrap(search.data) ?? []

  /**
   * costStatus は値札（`RuleCostSummary`）に渡す検索の状態。「未検索」（idle）と
   * 「検索したが 0 件」はどちらも `ids.length === 0` になり `ids` だけでは
   * 区別できないため、結果一覧（下の `search.isIdle` 分岐）と同じ判定を渡す。
   */
  const costStatus: 'idle' | 'pending' | 'error' | 'success' = search.isIdle
    ? 'idle'
    : search.isPending
      ? 'pending'
      : search.isError
        ? 'error'
        : 'success'

  /**
   * costSampleIds は値札の時間見積もりに使う番組の部分集合。`SearchResultList`
   * が表示のために取得する `ids.slice(0, visibleCount)`（下の JSX）と同じ集合を
   * 使う。`useQueries` のクエリキー（`getGetProgramQueryOptions(site, id)`）が
   * 一致するので、値札のために追加の HTTP リクエストは発生しない（React Query が
   * キャッシュを共有する）。**実測済み**: `search.test.tsx` の
   * 「読み込みが母数に追いついていない間は『先頭 N 件』からの外挿である旨を
   * 明記し、追いつくと消える（値札のために追加の HTTP リクエストは発生しない）」
   * が `GET /api/programs/{id}` の呼び出し件数を数えて確認している（37 件マッチ
   * で 30 → 37 と増える一方、重複が無いこと）。
   *
   * `loadedDurationsMs` の由来（先頭 N 件で無作為抽出ではない）は
   * `lib/rule-cost.ts` の `RuleCostSample` のコメントを参照。
   */
  const costSampleIds = ids.slice(0, visibleCount)
  const costDetails = useQueries({
    queries: costSampleIds.map((id) => getGetProgramQueryOptions(site, id)),
  })
  const loadedDurationsMs = costDetails
    .map((d) => unwrap(d.data)?.durationMs)
    .filter((ms): ms is number => ms !== undefined)

  /**
   * searchedHasPeriod は値札に「8 日分を 7 日換算」という根拠を出してよいかの判定。
   * `periodStartAt` / `periodEndAt` で期間を絞った検索は観測スパンが 8 日ではなく
   * その期間そのものになるため、8 日を根拠にすると偽の説明になる。
   *
   * **下書き（`draft`）ではなく実行した検索（`search.variables`）から導く。**
   * 値札の数値（`ids.length` / `loadedDurationsMs`）は実行済みの検索の産物なので、
   * 根拠だけをフォームの現在値から取ると再検索するまでの間だけ両者が食い違う ---
   * 期間を入れて検索したあと欄を空にするだけで、期間で絞った数値に
   * 「8 日分を 7 日換算」という偽の根拠が付き直す（逆向きも同様）。検証:
   * `search.test.tsx`「期間の根拠は実行した検索から導く: 下書きを触っても
   * 再検索するまで変わらない（両方向）」。
   *
   * `buildSearchRequest`（`lib/program-search.ts`）は期間が空ならキーごと落とす
   * ので、キーの有無がそのまま「期間で絞ったか」になる。型が許す `null`
   * （＝問わない）は「指定なし」側に畳む。
   *
   * `CreateRuleForm` / `RuleEditForm` にも同名の判定があるが、あちらは
   * 「この下書きを保存すると恒久的な期間制限になる」という**下書き**についての
   * 警告なので、下書きから導くのが正しい（同じ式に見えて由来が違う）。
   */
  const searchedRequest = search.variables?.data
  const searchedHasPeriod =
    (searchedRequest?.periodStartAt ?? null) !== null ||
    (searchedRequest?.periodEndAt ?? null) !== null

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
        {/*
         * 主操作（検索・条件をクリア）は条件の入力欄より前に置く（issue #305）。
         * `ConditionFields` はサービス・ジャンル・時間帯などのチップが画面の
         * 大半を占めるため、以前はこのブロックが `ConditionFields` の後ろに
         * あり、390px 幅ではスクロールしないとボトムタブの上に出てこなかった。
         * 条件を試す画面なので「まず送信できる状態を見せ、絞り込みは下に続く」
         * という並びに変える --- 条件を追加する操作自体は `ConditionFields`
         * 先頭のテキスト条件が担うので、ここで先に出しても
         * 「サービスチップ列より先に届く」という受け入れ基準は変わらない。
         *
         * **上に出しただけでは足りない。** 「押した結果」（値札・件数・結果）は
         * この 1 本の縦カラムの末尾に残るので、主操作を上端へ動かしても総
         * スクロール量は変わらず、負担が送信前から送信後に移るだけになる
         * （押しても折り目の中では `検索中…` の一瞬のラベル変化しか起きない）。
         * 送信のたびに結果の先頭へスクロールとフォーカスを移すことで対にする
         * （上の `resultsRef`）。
         */}
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

        <ConditionFields draft={draft} onChange={setDraft} />
      </form>

      <RuleCostSummary
        status={costStatus}
        totalCount={ids.length}
        loadedDurationsMs={loadedDurationsMs}
        hasPeriod={searchedHasPeriod}
      />

      {ruleId !== undefined ? (
        // ruleId のルールがまだ読み込めていない間（読み込み中 / 404 / 失敗）は
        // 保存 UI を出さない。バナー側が状態を伝えているので、ここで
        // 空の下書きに対する「新規作成」フォームを一瞬見せて紛らわしくしない。
        sourceRule !== undefined && (
          <RuleEditSection
            key={sourceRule.id}
            draft={draft}
            draftError={error}
            sourceRule={sourceRule}
          />
        )
      ) : (
        <CreateRuleSection draft={draft} draftError={error} />
      )}

      {/*
       * 送信後のスクロール・フォーカスの移動先（上の `resultsRef`）。
       * `tabIndex={-1}` はキーボードの tab 順を変えない（プログラムからだけ
       * フォーカスできる）。`scrollMarginTop` はページヘッダ（`sticky`）と
       * サーキットブレーカーのバナー（同）の高さぶん --- これが無いと結果の
       * 先頭が両者の下に潜り、スクロールしたのに件数行が見えない。
       * `--page-header-height` は `PageHeader` が共通の親（`<main>`）に実測値を
       * 書き出しており、その子であるこのセクションに継承される。
       * `outline-none` はフォーカスリングを消すためで、キーボード操作の助けを
       * 削ってはいない --- tab では到達しない要素であり、リングは結果一覧全体を
       * 囲う巨大な枠になる（`recording-filters.tsx` 等のコンテナと同じ流儀）。
       */}
      <section
        ref={resultsRef}
        aria-label="検索結果"
        tabIndex={-1}
        className="outline-none"
        style={{
          scrollMarginTop:
            'calc(var(--sticky-banners-height, 0px) + var(--page-header-height, 0px))',
        }}
      >
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
      </section>
    </>
  )
}

/**
 * RuleCostSummary は「この条件でルールを作成」「上書き保存」の近くに常置する値札
 * （issue #237）。ルールは保存した瞬間から録画（チューナー・ストレージ）を消費し
 * 続けるが、保存前に見えるのはマッチする番組リストだけで量としてのコストが無音
 * だった、という問題への対処。
 *
 * **値札は警告ではない。** しきい値で色を変えたり保存を止めたりしない --- 多いか
 * 少ないかの判断はユーザーのもの。文字色は他の情報表示と同じ `text-muted-foreground`
 * を使う（`--warning` は「条件ゼロ」「期間指定の恒久化」用、`--destructive` は
 * 「壊れた・取り返しがつかない」用で、どちらも意味が違うので流用しない。
 * docs/frontend/design.md「色は信号のみ」）。**GB 換算もやらない** ---
 * ビットレートの実測の出所が未決で、件数と時間は検索結果だけから導出でき
 * 未決に依存しないため、そこをこの値札のスコープの切れ目にしている。
 *
 * 「未検索」（`status === 'idle'`）と「検索したが 0 件」（`status === 'success'`
 * かつ `totalCount === 0`）を同じ文言にしない --- 両方とも件数が無い状態だが、
 * 条件を指定し忘れているだけなのか、条件が正しく絞り込めているのかは区別が要る
 * （`/search` の既存規律「未検索と 0 件を混同しない」と同じ精神）。
 *
 * 件数は `totalCount`（検索 API が返す全件、ページングなし）から厳密に出せる。
 * 時間は番組ごとの `durationMs` が要るため `loadedDurationsMs`（画面が結果表示の
 * ために読み込んだ分。`programId` 昇順の先頭 N 件で、無作為抽出ではない ---
 * `lib/rule-cost.ts` の `RuleCostSample` のコメントを参照）の平均から外挿する
 * 近似値になる。母数（`totalCount`）に対して読み込みが追いついていないときは
 * `estimateRuleCost` の `isSampled` を見て「先頭 N 件」であることを文言に足す
 * （黙って過小に見せない。かつ読み込みが 1 件も済んでいない間はこの注記を出さない
 * --- `estimate.durationMsPerWeek === undefined`（算出中）のときに
 * 「0 件の平均から算出」という自己矛盾した文言を出さないため）。
 *
 * `hasPeriod` が真（`periodStartAt` / `periodEndAt` で期間を絞った検索）のときは
 * 「8 日分を 7 日換算」という根拠を出さない --- その根拠は「検索結果は EPG の
 * 前方 8 日ぶんの観測」という前提に立っており、期間を絞った検索では観測スパンが
 * その期間そのものになるため前提が崩れる（8 日分ではないのに 8 日分と言うと偽の
 * 根拠になる。issue #237 の罠「黙って過小に見せない」に反する）。代わりに
 * 「期間条件で絞っているため、週あたりの見込みは実際より小さく出ます」と明記する。
 *
 * **`hasPeriod` は `totalCount` / `loadedDurationsMs` と同じ検索の産物でなければ
 * ならない**（呼び出し側の `searchedHasPeriod` を参照）。数値と根拠の由来が
 * 食い違うと、消したはずの偽の根拠が「フォームを触っただけ」で復活する。
 */
function RuleCostSummary({
  status,
  totalCount,
  loadedDurationsMs,
  hasPeriod,
}: {
  status: 'idle' | 'pending' | 'error' | 'success'
  totalCount: number
  loadedDurationsMs: number[]
  hasPeriod: boolean
}) {
  if (status === 'idle') {
    return (
      <p className="px-4 py-2 text-xs text-muted-foreground">
        検索すると、この条件で保存した場合の週あたりの見込み（件数・録画時間）が表示されます
      </p>
    )
  }
  if (status === 'pending') {
    return <p className="px-4 py-2 text-xs text-muted-foreground">見込みを計算中…</p>
  }
  if (status === 'error') {
    return (
      <p className="px-4 py-2 text-xs text-muted-foreground">
        検索が失敗したため見込みを表示できません
      </p>
    )
  }

  const estimate = estimateRuleCost({ totalCount, loadedDurationsMs })
  const countText = `約 ${Math.round(estimate.countPerWeek)} 件`
  const durationText =
    estimate.durationMsPerWeek === undefined
      ? '算出中…'
      : `約 ${formatDuration(estimate.durationMsPerWeek)}`

  // 期間条件で絞っている検索は観測スパンが 8 日ではないため、8 日を根拠にする
  // 文言は出さず、実際より小さく出ることを明記する（上のコメント参照）。
  const basisText = hasPeriod
    ? ''
    : `（現在の EPG 実測 ${estimate.totalCount} 件・${epgWindowDays} 日分を ${ruleCostWeekDays} 日換算）`
  const periodNote = hasPeriod
    ? '（期間条件で絞っているため、週あたりの見込みは実際より小さく出ます）'
    : ''

  // 読み込みが 1 件も済んでいない間（durationMsPerWeek === undefined）は
  // 「0 件の平均から算出」という自己矛盾した文言を出さない。
  const sampledNote =
    estimate.durationMsPerWeek !== undefined && estimate.isSampled
      ? `（時間は先頭 ${estimate.sampleSize} 件の平均から算出）`
      : ''

  // 件数は 1 つの文字列にする（JSX で連結するとテキストノードが分かれ、
  // 読み上げも切れて聞こえる。上の検索結果件数の表示と同じ流儀）。
  const text =
    `この条件で保存すると、週あたり見込みで${countText}・${durationText}` +
    basisText +
    periodNote +
    sampledNote

  return <p className="px-4 py-2 text-xs text-muted-foreground">{text}</p>
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
      <div
        role="status"
        className="border-b border-border bg-muted/40 px-4 py-2 text-xs text-muted-foreground"
      >
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
        ルール「{rule.name}」の条件を編集中です。「ルールを上書き保存」で保存すると、このルール自体が書き換わります。元のルールを残したい場合は「別の新しいルールとして保存」を使ってください。
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
 * にメタ情報の入力欄を常時見せないため。`?ruleId=N` から開いたときの保存
 * （上書き / 別ルールとして保存）は `RuleEditSection` が担うので、ここは
 * `ruleId` を伴わない通常の検索専用（新規作成のみ）。
 */
function CreateRuleSection({
  draft,
  draftError: draftHasError,
}: {
  draft: SearchDraft
  draftError: string | undefined
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
        onCancel={() => setOpen(false)}
        onDone={() => setOpen(false)}
      />
    </div>
  )
}

/**
 * CreateRuleForm は条件以外のメタ情報（名前・有効・優先度・エンコード設定）を
 * 入力して `POST /api/rules` に送る。`?ruleId` を伴わない通常の検索からのみ
 * 使われる（既存ルールのフォークは `RuleEditSection` の「別の新しいルールとして
 * 保存」に一本化した）ので、`preserve` は渡さない — UI を持たない項目
 * （`sites` 等）を推測して埋めることはしない。
 */
function CreateRuleForm({
  draft,
  draftHasError,
  onCancel,
  onDone,
}: {
  draft: SearchDraft
  draftHasError: boolean
  onCancel: () => void
  onDone: () => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const createRule = useCreateRule()

  const [meta, setMeta] = useState<RuleMetaDraft>(emptyRuleMeta)
  // 「全番組が対象になる」ことを理解した上での作成かどうか。条件を追加すれば
  // このチェックは意味を失うが、外れたままでも実害はない(次の保存試行時に
  // 改めて noConditions を評価するだけ)。
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
    const input = buildRuleInput(draft, meta)
    createRule.mutate(
      { data: input },
      {
        onSuccess: () => {
          toast({ message: 'ルールを作成しました' })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
          onDone()
          void navigate({ to: '/rules' })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました',
            kind: 'error',
          }),
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
        <p className="text-xs text-warning">
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
 * RuleEditSection は `?ruleId=N` で開いたときの保存 UI。
 *
 * `CreateRuleSection` と違って折りたたまない —— ユーザーは「試している」の
 * ではなく、既にあるルールを編集する目的でこの画面を開いている（マッチする
 * 番組を見ながら条件を詰められるこの画面が実質のルール編集画面になる、という
 * 判断）。`key={sourceRule.id}` を親で付けているので、`ruleId` を切り替えて
 * 別のルールを開き直したときはこのコンポーネントごと作り直され、下の
 * `RuleEditForm` の `meta` / `confirmedEmpty` が古いルールの値のまま残らない。
 */
function RuleEditSection({
  draft,
  draftError: draftHasError,
  sourceRule,
}: {
  draft: SearchDraft
  draftError: string | undefined
  sourceRule: Rule
}) {
  return (
    <div className="border-b border-border px-4 py-4">
      <RuleEditForm draft={draft} draftHasError={draftHasError !== undefined} sourceRule={sourceRule} />
    </div>
  )
}

/**
 * RuleEditForm は `?ruleId=N` で開いたルールの保存本体。
 *
 * 主動作は **上書き保存**（`PATCH /api/rules/{id}`）。`UpdateRule` は子テーブル
 * 全置換なので、UI を持たない項目（`description` / `dedupe*` /
 * `filenameTemplate` / `metadata` / `sites`）を `buildRuleInput` の `preserve`
 * で必ず引き継ぐ —— 落とすとユーザーの設定が黙って消える。
 *
 * 副動作は「別の新しいルールとして保存」（`POST /api/rules`）。元のルールを
 * 下敷きに別のルールを作れる経路を残す。押し間違いを防ぐため、主動作
 * （`type="submit"`、既定の見た目）と副動作（`type="button"`、`outline`）で
 * 見た目を変え、副動作の脇に「元のルールは変更されません」と明示する。
 *
 * 保存後は `/rules` へ遷移しない —— 条件を詰め直す作業の途中で画面が飛ぶと
 * 作業が切れる。`getListRulesQueryKey()` とこのルール自身のクエリ
 * （`getGetRuleQueryKey`）の両方を invalidate し、一覧とバナー双方の表示を
 * 最新化する。
 */
function RuleEditForm({
  draft,
  draftHasError,
  sourceRule,
}: {
  draft: SearchDraft
  draftHasError: boolean
  sourceRule: Rule
}) {
  const toast = useToast()
  const queryClient = useQueryClient()
  const updateRule = useUpdateRule()
  const createRule = useCreateRule()

  // 上書き保存が既定の動作なので、名前欄の初期値は元のルール名そのまま
  // （`〜 のコピー` を付けない）。フォークではなく編集だからである。
  const [meta, setMeta] = useState<RuleMetaDraft>(() => ruleToMeta(sourceRule))
  // 「全番組が対象になる」ことを理解した上での保存かどうか。上書き・別ルール
  // 保存のどちらにも効く（両方とも「保存すると全番組が対象になる」という
  // 同じ危険を持つため）。
  const [confirmedEmpty, setConfirmedEmpty] = useState(false)

  const metaError = ruleMetaError(meta)
  const request = buildSearchRequest(draft)
  const noConditions = Object.keys(request).length === 0
  const hasPeriod = draft.periodStartAt !== '' || draft.periodEndAt !== ''
  const pending = updateRule.isPending || createRule.isPending

  const blocked =
    draftHasError || metaError !== undefined || (noConditions && !confirmedEmpty) || pending

  const overwrite = () => {
    if (blocked) return
    // preserve に sourceRule を渡す。渡し忘れると UI を持たない項目
    // （description / dedupe* / filenameTemplate / metadata / sites）が
    // `UpdateRule` の子テーブル全置換で黙って消える。
    const input = buildRuleInput(draft, meta, sourceRule)
    updateRule.mutate(
      { id: sourceRule.id, data: input },
      {
        onSuccess: () => {
          toast({ message: `ルール「${meta.name.trim()}」を上書き保存しました` })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
          void queryClient.invalidateQueries({ queryKey: getGetRuleQueryKey(sourceRule.id) })
          // /rules へは遷移しない。条件を詰め直す作業の途中なので、画面が
          // 飛ぶと作業が切れる。
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの更新に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  const saveAsNew = () => {
    if (blocked) return
    // `rules.name` に一意制約は無い（rules テーブル定義）ので、名前を
    // そのまま引き継ぐと一覧に同名の 2 本が並び、条件の要約でしか見分けられ
    // なくなる。押した時点で名前が元のルールと同じままなら `〜 のコピー` を
    // 付ける。名前欄そのものは書き換えない（上書き保存に戻ったときに元の名前
    // のままであってほしいため）。ユーザーが既に名前を変えているなら、
    // その意図（別の名前を選んだ）を尊重してそのまま使う。
    const trimmed = meta.name.trim()
    const name = trimmed === sourceRule.name.trim() ? `${trimmed} のコピー` : trimmed
    // preserve した `sites` は `POST /api/rules` に載る。API の「保存済み site 名は
    // レジストリ照合を免除する」はルール単位で PATCH にしか効かないので、レジストリから
    // site が消えた後はこの経路だけが 400 `unknown site` になり、`sites` は条件 UI に
    // 無いのでユーザーは画面内で外せない（未解決。docs/frontend/search.md §「UI が
    // 持たない次元は勝手に埋めない」）。落として送るのは禁止 —— 絞り込みが無音で
    // 全サイトに反転する。
    const input = buildRuleInput(draft, { ...meta, name }, sourceRule)
    createRule.mutate(
      { data: input },
      {
        onSuccess: () => {
          toast({ message: `「${name}」として新しいルールを保存しました` })
          void queryClient.invalidateQueries({ queryKey: getListRulesQueryKey() })
        },
        onError: (err) =>
          toast({
            message: apiErrorMessage(err) ?? 'ルールの作成に失敗しました',
            kind: 'error',
          }),
      },
    )
  }

  return (
    <form
      aria-label="ルールの条件を編集"
      className="flex flex-col gap-4 rounded-lg border border-border p-3"
      onSubmit={(e) => {
        e.preventDefault()
        overwrite()
      }}
    >
      {hasPeriod && (
        <p className="text-xs text-warning">
          期間を指定したまま保存すると、ルールの恒久的な期間制限になります。「いまだけ絞り込みたい」
          場合は、上の条件フォームで期間を空にしてから保存してください。
        </p>
      )}

      {noConditions && (
        <div className="flex flex-col gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5">
          <p role="alert" className="text-xs text-destructive">
            条件を 1 つも指定していません。このまま保存すると、放送されるすべての番組が対象になります。
          </p>
          <label className="flex items-center gap-2 text-xs text-foreground">
            <input
              type="checkbox"
              className="size-4 accent-primary"
              checked={confirmedEmpty}
              disabled={pending}
              onChange={(e) => setConfirmedEmpty(e.target.checked)}
            />
            すべての番組が対象になることを理解した上で保存します
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

      <div className="flex flex-col gap-2 border-t border-border pt-3">
        <div className="flex flex-wrap gap-2">
          <Button type="submit" size="lg" disabled={blocked}>
            {updateRule.isPending ? '上書き保存中…' : 'ルールを上書き保存'}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="lg"
            disabled={blocked}
            onClick={saveAsNew}
          >
            {createRule.isPending ? '保存中…' : '別の新しいルールとして保存'}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          「ルールを上書き保存」はルール「{sourceRule.name}」自体を書き換えます。元のルールを
          残したまま試したい場合は「別の新しいルールとして保存」を使ってください（元のルールは
          変更されません）。
        </p>
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
  const site = useCurrentSite()
  const details = useQueries({
    queries: ids.map((id) => getGetProgramQueryOptions(site, id)),
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
      <div className="w-20 shrink-0 text-sm">{formatDateTime(program.startAt)}</div>
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
