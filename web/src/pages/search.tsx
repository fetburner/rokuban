import { keepPreviousData, useQueryClient, useQueries } from '@tanstack/react-query'
import { Link, useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getGetProgramQueryOptions,
  getGetRuleQueryKey,
  getListRulesQueryKey,
  useCreateRule,
  useGetRule,
  useListCapacityOverages,
  useSearchPrograms,
  useUpdateRule,
  type ProgramListItem,
  type ProgramSearchMatch,
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
import { useAllSitesServices } from '@/lib/all-sites-services'
import { countProgramsInShortfall } from '@/lib/capacity'
import { dayOrigin } from '@/lib/day-offset'
import { formatDateTime, formatDuration } from '@/lib/format'
import { cn } from '@/lib/utils'
import {
  buildRuleInput,
  buildSearchRequest,
  draftError,
  emptyDraft,
  emptyRuleMeta,
  hasNoConditions,
  ruleMetaError,
  conditionsToDraft,
  ruleToMeta,
  type RuleMetaDraft,
  type SearchDraft,
} from '@/lib/program-search'
import { loadLastSearchConditions, saveLastSearchConditions } from '@/lib/search-storage'
import {
  epgWindowDays,
  estimateRuleCost,
  ruleCostWeekDays,
  type RuleCostEstimate,
} from '@/lib/rule-cost'

/**
 * pageSize は一度に詳細を取りに行く結果の件数。
 *
 * 検索 API が返すのは `{site, programId}` の行だけなので、1 件表示するたびに
 * `GET /api/sites/{site}/programs/{programId}` が必要になる。数百件を一斉に取りに行かないよう区切る
 * （API に一括取得がないことの申し送りは issue #24 のコメント）。
 */
const pageSize = 30

/**
 * SearchPage は EPG をルールと同じ条件で検索する画面。
 *
 * 検索 API（`POST /api/sites/{site}/programs/search`）は ruler 評価と同じコンパイラを通るため、
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
  // nowMs はこのレンダーの間で一貫させる（`pages/home.tsx`・`pages/programs.tsx`
  // と同じ規律）。容量ノートの問い合わせ窓（下の `shortfallWindowStartMs`）だけが使う。
  const nowMs = Date.now()
  const routeSearch = useRouteSearch({ from: '/search' })
  const ruleId = routeSearch.ruleId
  const navigate = useNavigate()

  /**
   * 下書きの初期値は **URL > localStorage > 空**（docs/frontend/design.md §個人化）。
   *
   * URL（`?cond=` / `?ruleId=`）が条件を持つときは下のハイドレーション effect が
   * 写すので、ここでは空から始める。どちらも無いときだけ、前回押した条件を端末から
   * 復元する --- 検索は「思い出して打ち直す」より「見て直す」方が速い画面で、
   * 条件は毎回ほぼ同じものを少しずつ変えて使う。
   *
   * **復元するのはフォームだけで、検索は実行しない。** 開いた瞬間に前回の
   * 問い合わせが飛ぶのは、押していない操作が起きたのと同じになる。
   */
  const [draft, setDraft] = useState<SearchDraft>(() => {
    if (routeSearch.ruleId !== undefined || routeSearch.cond !== undefined) return emptyDraft()
    const last = loadLastSearchConditions()
    return last === undefined ? emptyDraft() : conditionsToDraft(last)
  })
  const [visibleCount, setVisibleCount] = useState(pageSize)
  /**
   * serviceById は結果行（`SearchResultRow`）のサービス名解決に使う。
   *
   * **`sites` が条件 UI に出た（issue #531）ので、検索結果は複数 site の行を
   * 含みうる。** 以前は検索が常に先頭 site 固定の単一 site にしか
   * 投げなかったため、結果行の名前解決も同じ単一 site の一覧
   * だけで閉じていたが、その前提が崩れた --- 先頭 site の一覧だけでは他 site
   * だけが受けているサービスの名前が引けない（`<ConditionFields>` の選択肢と
   * 同じ非対称の再発なので、`useAllSitesServices()`（全 site から `Service.id`
   * で畳んだ union）に揃える）。
   *
   * **キーは `Service.id` そのものではなく `${networkId}:${serviceId}`。**
   * `GET /api/sites/{site}/programs/{programId}` のレスポンス（`ProgramListItem`）は
   * `networkId` / `serviceId` を別フィールドで持ち、合成済みの `Service.id`
   * は持たないため、番組側からは同じ式でキーを組み直す必要がある。かつては
   * `serviceId` 単独をキーにしていたが、それは network をまたぐと一意でない
   * （`condition-fields.test.tsx` のフィクスチャに実例あり: 32676/1033 と
   * 32677/1033 --- どちらも「瀬戸内海放送」で serviceId 1033 が重複する）ため、
   * union に切り替えると network の種類が増えるぶん衝突の機会も増える。
   * `networkId` を組にすることでこの衝突を避ける。
   */
  const {
    sites,
    services: serviceList,
    isPending: registryPending,
    isError: registryError,
    refetch: refetchRegistry,
  } = useAllSitesServices()
  // 検索 API の path parameter は site ごとのルーティングに必要だが、検索対象は
  // body の空 `sites`（全 site）で指定する。レジストリの先頭は routing anchor
  // にだけ使い、結果行の site は検索レスポンスをそのまま運ぶ。
  const searchSite = sites[0]
  const search = useSearchPrograms()
  // ruleId が無いときは問い合わせを止める。useGetRule は id を必須の number で
  // 取るため、無効化中はダミー値を渡す（program-overlap-warning.tsx と同じ流儀）。
  const ruleQuery = useGetRule(ruleId ?? -1, { query: { enabled: ruleId !== undefined } })
  const sourceRule = unwrap(ruleQuery.data)

  const serviceById = useMemo(() => {
    const map = new Map<string, Service>()
    for (const s of serviceList) map.set(`${s.networkId}:${s.serviceId}`, s)
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
    if (searchSite === undefined) return
    if (hydratedRuleIdRef.current === ruleId) return
    hydratedRuleIdRef.current = ruleId

    const nextDraft = conditionsToDraft(sourceRule)
    setDraft(nextDraft)
    setVisibleCount(pageSize)
    searchRef.current.mutate({ site: searchSite, data: buildSearchRequest(nextDraft) })
  }, [ruleId, searchSite, sourceRule])

  /**
   * `?cond=` のハイドレーション。共有・ブックマークされた URL を開いたときに、
   * 条件をフォームへ写してそのまま検索する（結果が出ていない共有リンクは
   * 「同じ結果を見せる」という目的を果たさない）。
   *
   * **`?ruleId=` があるときは何もしない。** ルール編集として開いた画面の正本は
   * ルールの条件で、そちらのハイドレーションと二重に下書きを書くと、どちらが
   * 勝ったかがタイミング次第になる。
   *
   * ガードは ruleId 版と同じ理屈（適用済みの条件を ref に持ち、同じ値なら
   * 何もしない）だが、比較する値がオブジェクトなので JSON 文字列で持つ。
   * 自分で `navigate` して URL を書き換えたときも、この ref のおかげで
   * 再検索にはならない（下の `submit`）。
   *
   * **比べるのは URL の生の値ではなく、そこから作ったリクエスト。** URL の値は
   * `validateSearch` の zod スキーマを通って戻ってくるので、既定値
   * （`caseSensitive` / `negate`）が埋まった形になり、`submit` が送った
   * リクエストとは文字列として一致しない。生の JSON を比べると、自分で書いた
   * URL が「別の条件」に見えて**押すたびに同じ検索を 2 回叩く**
   * （`e2e/personalization.mjs` の③がこれを見ている）。
   */
  const appliedCondRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (ruleId !== undefined) return
    if (searchSite === undefined) return
    const cond = routeSearch.cond
    const nextDraft = cond === undefined ? undefined : conditionsToDraft(cond)
    const encoded =
      nextDraft === undefined ? undefined : JSON.stringify(buildSearchRequest(nextDraft))
    if (appliedCondRef.current === encoded) return
    appliedCondRef.current = encoded
    if (nextDraft === undefined) return

    setDraft(nextDraft)
    setVisibleCount(pageSize)
    searchRef.current.mutate({ site: searchSite, data: buildSearchRequest(nextDraft) })
  }, [ruleId, routeSearch.cond, searchSite])

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
    if (error !== undefined || registryPending || registryError || searchSite === undefined) return
    const request = buildSearchRequest(draft)
    setVisibleCount(pageSize)
    pendingResultScrollRef.current = true
    search.mutate({ site: searchSite, data: request })
    // 押した条件だけを「最後の条件」として残す。打っている途中の下書きを保存すると、
    // 次に開いたとき送れない下書き（値が空のテキスト条件など）が復元されうる。
    saveLastSearchConditions(request)

    // 押した条件を URL にも載せる（共有・ブックマーク・リロードで同じ結果）。
    // 履歴は汚さない（`replace`）--- 条件を 3 回直して押したあとの「戻る」で
    // 検索画面を 3 回通るのは、押した回数を覚えていない側の負担になる。
    //
    // **`?ruleId=` で開いているときは載せない。** その画面の条件の正本はルールで、
    // 両方が URL にあると、次に開いたときどちらを写したのかが読めなくなる。
    if (ruleId === undefined) {
      appliedCondRef.current = JSON.stringify(request)
      void navigate({
        to: '/search',
        search: (prev) => ({
          ...prev,
          // 条件なしの検索（全件）で `?cond={}` を残さない（不変条件 10）
          cond: Object.keys(request).length > 0 ? request : undefined,
        }),
        replace: true,
      })
    }
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

  /**
   * matches は検索結果の行そのもの（`[{site, programId}]`。畳まない）。
   * **行数 = 予約数**（ruler はマッチした全 site で予約を作るため、同一放送が
   * 2 site でマッチすれば 2 予約になる）なので、件数を数えるところは常に
   * `matches.length` を使い、`programId` で重複排除しない（issue #531）。
   */
  const matches = unwrap(search.data) ?? []

  /**
   * costStatus は値札（`RuleCostSummary`）に渡す検索の状態。「未検索」（idle）と
   * 「検索したが 0 件」はどちらも `matches.length === 0` になり `matches` だけでは
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
   * costSample は値札の時間見積もりに使う番組の部分集合。`SearchResultList`
   * が表示のために取得する `matches.slice(0, visibleCount)`（下の JSX）と同じ
   * 集合を使う。`useQueries` のクエリキー（`getGetProgramQueryOptions(site, id)`）が
   * 一致するので、値札のために追加の HTTP リクエストは発生しない（React Query が
   * キャッシュを共有する）。**実測済み**: `search.test.tsx` の
   * 「読み込みが母数に追いついていない間は『先頭 N 件』からの外挿である旨を
   * 明記し、追いつくと消える（値札のために追加の HTTP リクエストは発生しない）」
   * が `GET /api/sites/{site}/programs/{programId}` の呼び出し件数を数えて確認している（37 件マッチ
   * で 30 → 37 と増える一方、重複が無いこと）。
   *
   * **詳細取得の `site` は行が運ぶ `match.site` を使う**
   * （issue #531）。`epg_programs` の主キーは `(site, program_id)` なので、
   * 現在 site 固定で引くと第 2 site だけの結果が 404 になる
   * （`docs/frontend/shell.md`「サイトの扱い」の「行が運ぶ」と同じ規律）。
   *
   * `loadedDurationsMs` の由来（先頭 N 件で無作為抽出ではない）は
   * `lib/rule-cost.ts` の `RuleCostSample` のコメントを参照。
   */
  const costSample = matches.slice(0, visibleCount)
  const costDetails = useQueries({
    queries: costSample.map((match) => getGetProgramQueryOptions(match.site, match.programId)),
  })
  const loadedDurationsMs = costDetails
    .map((d) => unwrap(d.data)?.durationMs)
    .filter((ms): ms is number => ms !== undefined)

  /**
   * costEstimate は値札（件数・時間の見込み）。`RuleCostSummary` と
   * `ShortfallOverlapNote` の両方に同じ 1 つを渡す --- 呼び出し側ごとに
   * 母集団の式を書き直すと、同じ「先頭 N 件」がずれうる（レビュー指摘）。
   */
  const costEstimate = estimateRuleCost({ totalCount: matches.length, loadedDurationsMs })

  /**
   * 容量ノート（`ShortfallOverlapNote`）用の `GET /api/capacity/overages` の窓は
   * `nowMs` の時境界（`pages/home.tsx` と同じ量子化）+ `epgWindowDays`。サンプル
   * 番組の時刻から作ると、詳細が 1 件ずつ届くたびに窓＝クエリキーが変わって点滅し、
   * 終了未定番組（`durationMs = 0`）だけのサンプルでは `start === end` に退化して
   * 400 で沈黙する（回帰判定は `search.test.tsx` の 3 件と `design.mjs` ①''''）。
   *
   * **この窓は検索結果の放送時間帯を覆いきらない。** 検索に `now()` 述語は無く、
   * `epg_programs` は放送済みを `epg.retention_grace`（既定 24h）ぶん残す
   * （`lib/rule-cost.ts` の `epgWindowDays`）ので、放送済みの結果と地平線末尾の
   * 最大 59 分ぶんは窓の外で数え落とす（向きは下界側なので許容する）。
   */
  const shortfallWindowStartMs = dayOrigin(0, nowMs).getTime()
  const shortfallWindowEndMs = shortfallWindowStartMs + epgWindowDays * 86_400_000
  const overagesQuery = useListCapacityOverages(
    {
      start: new Date(shortfallWindowStartMs).toISOString(),
      end: new Date(shortfallWindowEndMs).toISOString(),
    },
    {
      query: {
        // 検索結果が無い間は問い合わせない（容量への影響を確かめる対象が無い）。
        enabled: matches.length > 0,
        // 時境界を越えてキーが進んだ瞬間にノートが 1 RTT 消えないため
        // （判定は `search.test.tsx`「時境界を越えても…」。`pages/home.tsx` に同じ対策）。
        placeholderData: keepPreviousData,
      },
    },
  )
  // 取得の未完了・失敗（pending/error/400）も `unwrap` で `[]` に畳まれる。
  // したがって `ShortfallOverlapNote` が出ないことは「今は重なる不足区間が
  // 無い」以外に「まだ取得できていない／失敗した」も意味しうる
  // （`pages/reservations.tsx` の `overagesQuery` と同じ事情・同じ規律 ---
  // 取得失敗を隠して「警告なし」側に倒すのは既存の踏襲先と揃えた意図的な選択）。
  const overages = unwrap(overagesQuery.data) ?? []

  /**
   * shortfallCount は容量への影響の近似（判定 (b)）。新たな不足を予測しない理由・
   * 0 件の意味・終了未定番組の扱いは `lib/capacity.ts` の
   * `countProgramsInShortfall`。母集団は `costEstimate` と同じサンプルに揃える
   * （別の母集団だと同じ値札の中で「先頭 N 件」の意味が場所によって変わる）。
   *
   * **サイト軸は行ごと**（issue #531。`lib/capacity.ts` の
   * `countProgramsInShortfall` のコメント参照）。`costDetails[i]` は
   * `costSample[i]` と同じ添字（`useQueries` は入力順を保つ）なので、
   * 番組の詳細に `costSample[i].site` を添えて渡す。
   */
  const loadedPrograms = costSample
    .map((match, i) => {
      const program = unwrap(costDetails[i]?.data)
      return program === undefined ? undefined : { ...program, site: match.site }
    })
    .filter((p): p is ProgramListItem & { site: string } => p !== undefined)
  const shortfallCount = countProgramsInShortfall(overages, loadedPrograms)

  /**
   * searchedHasPeriod は値札に「8 日分を 7 日換算」という根拠を出してよいかの判定。
   * `periodStartAt` / `periodEndAt` で期間を絞った検索は観測スパンが 8 日ではなく
   * その期間そのものになるため、8 日を根拠にすると偽の説明になる。
   *
   * **下書き（`draft`）ではなく実行した検索（`search.variables`）から導く。**
   * 値札の数値（`matches.length` / `loadedDurationsMs`）は実行済みの検索の産物なので、
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

      {registryError && (
        <ErrorState onRetry={() => void refetchRegistry()}>
          サイト一覧の取得に失敗しました
        </ErrorState>
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
          {registryPending && (
            <p role="status" className="text-xs text-muted-foreground">
              サイト一覧を取得中…
            </p>
          )}
          <div className="flex gap-2">
            <Button
              type="submit"
              size="lg"
              disabled={
                error !== undefined || search.isPending || registryPending || registryError
              }
            >
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

      <RuleCostSummary status={costStatus} estimate={costEstimate} hasPeriod={searchedHasPeriod} />
      <ShortfallOverlapNote
        count={shortfallCount}
        sampleSize={costEstimate.sampleSize}
        isSampled={costEstimate.isSampled}
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
          <SearchError
            error={search.error}
            onRetry={() => {
              if (search.variables) search.mutate(search.variables)
            }}
          />
        ) : matches.length === 0 ? (
          <EmptyState>条件に一致する番組がありません</EmptyState>
        ) : (
          <>
            <p role="status" className="px-4 py-2 text-xs text-muted-foreground">
              {/* 件数は 1 つの文字列にする（JSX で連結するとテキストノードが分かれ、
                  読み上げも「37」「件」と切れる） */}
              {visibleCount < matches.length
                ? `${matches.length} 件（番組 ID 順）— ${visibleCount} 件を表示`
                : `${matches.length} 件（番組 ID 順）`}
            </p>
            <SearchResultList matches={matches.slice(0, visibleCount)} serviceById={serviceById} />
            {visibleCount < matches.length && (
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
 * **`hasPeriod` は `estimate` と同じ検索の産物でなければならない**
 * （呼び出し側の `searchedHasPeriod` を参照）。数値と根拠の由来が食い違うと、
 * 消したはずの偽の根拠が「フォームを触っただけ」で復活する。
 *
 * `estimate` は呼び出し側が 1 回計算したものを受け取る（`ShortfallOverlapNote`
 * も同じものを使うので、ここで計算し直さない）。
 */
function RuleCostSummary({
  status,
  estimate,
  hasPeriod,
}: {
  status: 'idle' | 'pending' | 'error' | 'success'
  estimate: RuleCostEstimate
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
 * ShortfallOverlapNote は検索結果のうち放送時間帯が既存のチューナー不足区間と
 * 交差する番組の件数を値札の隣に出す（判定 (b)。docs/frontend/search.md
 * 「保存前の値札」）。**0 件のときは何も描画しない**（`CapacityShortfallBadge`
 * と同じ「沈黙は保証ではない」規律。緑にも「収まります」にもしない）。上限で
 * 切れているときは値札の他の注記と同じ形で「先頭 N 件のうち」と明記する。
 */
function ShortfallOverlapNote({
  count,
  sampleSize,
  isSampled,
}: {
  count: number
  sampleSize: number
  isSampled: boolean
}) {
  if (count === 0) return null

  const scope = isSampled ? `先頭 ${sampleSize} 件のうち、` : ''
  return (
    <p className="px-4 py-2 text-xs text-muted-foreground">
      {scope}既にチューナー不足の区間と重なる番組が {count} 件あります
    </p>
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
 * （`description` 等）を推測して埋めることはしない。
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
  const noConditions = hasNoConditions(draft)
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
 * `filenameTemplate` / `metadata`）を `buildRuleInput` の `preserve` で必ず
 * 引き継ぐ —— 落とすとユーザーの設定が黙って消える（`sites` は issue #531 で
 * `<ConditionFields>` の次元になったため、いまは下書きから普通に送る）。
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
  const noConditions = hasNoConditions(draft)
  const hasPeriod = draft.periodStartAt !== '' || draft.periodEndAt !== ''
  const pending = updateRule.isPending || createRule.isPending

  const blocked =
    draftHasError || metaError !== undefined || (noConditions && !confirmedEmpty) || pending

  const overwrite = () => {
    if (blocked) return
    // preserve に sourceRule を渡す。渡し忘れると UI を持たない項目
    // （description / dedupe* / filenameTemplate / metadata）が
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
    // `draft.sites` は `POST /api/rules` に載る。API の「保存済み site 名は
    // レジストリ照合を免除する」はルール単位で PATCH にしか効かないので、
    // レジストリから消えた site を含んだまま POST するとこの経路だけ 400
    // `unknown site` になりうる --- ただし `sites` は issue #531 で条件 UI の
    // 次元になったため、`<ConditionFields>` のサイトチップ（レジストリと下書きの
    // 和集合を選択肢にする）でユーザーが画面内で外せる。落として送るのは禁止
    // —— 絞り込みが無音で全サイトに反転する。
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
function SearchError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const message = apiErrorMessage(error)
  return (
    <ErrorState onRetry={onRetry}>
      <span className="block">検索に失敗しました</span>
      {message !== undefined && (
        <span className="mt-1 block break-all font-mono text-xs">{message}</span>
      )}
    </ErrorState>
  )
}

/**
 * SearchResultList は検索結果の行（`[{site, programId}]`）を番組の行にする。
 *
 * 検索 API は `site` と `programId` しか返さないため、行ごとに
 * `GET /api/sites/{site}/programs/{programId}` を引く。`useQueries` で 1 箇所に
 * まとめているのは、行コンポーネントに hook を置くと「行が消えるとクエリも
 * 消える」形になり、表示件数を増やしたときの取得状態が追いにくくなるため。
 *
 * **`site` は行が運ぶ `match.site` を使う**
 * ---`epg_programs` の主キーは `(site, program_id)` なので、現在 site 固定で
 * 引くと第 2 site だけの結果が 404 になる（issue #531）。
 *
 * **key は `${site}:${programId}`。** 同一放送（同じ `programId`）が複数 site で
 * マッチすると行が複数出る（畳まない契約）ため、`programId` だけを key にすると
 * React が 2 行を同一視して警告し、行の再利用が壊れる。
 */
function SearchResultList({
  matches,
  serviceById,
}: {
  matches: ProgramSearchMatch[]
  serviceById: Map<string, Service>
}) {
  const details = useQueries({
    queries: matches.map((match) => getGetProgramQueryOptions(match.site, match.programId)),
  })

  return (
    <ul data-testid="search-results">
      {matches.map((match, i) => {
        const detail = details[i]
        const program = unwrap(detail?.data)
        return (
          <li key={`${match.site}:${match.programId}`}>
            {program !== undefined ? (
              <SearchResultRow
                program={program}
                serviceName={serviceById.get(`${program.networkId}:${program.serviceId}`)?.name}
              />
            ) : detail?.isError ? (
              // 取得できなかった行を黙って落とさない。EPG のローリング
              // ウィンドウから抜けた番組が検索結果に残ることは実際に起きる
              <p className="border-b border-border px-4 py-3 text-xs text-destructive">
                番組 #{match.programId} の詳細を取得できませんでした
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
