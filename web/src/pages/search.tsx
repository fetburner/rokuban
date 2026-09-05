import { keepPreviousData, useQueries } from '@tanstack/react-query'
import { useNavigate, useSearch as useRouteSearch } from '@tanstack/react-router'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getGetProgramQueryOptions,
  useGetRule,
  useListCapacityOverages,
  useSearchPrograms,
  type ProgramListItem,
  type ProgramSearchMatch,
  type Service,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { ConditionFields } from '@/components/condition-fields'
import {
  CreateRuleSection,
  RuleCostSummary,
  RuleEditSection,
  RuleSourceBanner,
  ShortfallOverlapNote,
} from '@/components/rule-form'
import { EmptyState, ErrorState, ListSkeleton, PageHeader, Skeleton } from '@/components/page'
import { Button } from '@/components/ui/button'
import { useAllSitesServices } from '@/lib/all-sites-services'
import { countProgramsInShortfall } from '@/lib/capacity'
import { dayOrigin } from '@/lib/day-offset'
import { formatDateTime, formatDuration } from '@/lib/format'
import {
  buildSearchRequest,
  draftError,
  emptyDraft,
  conditionsToDraft,
  type SearchDraft,
} from '@/lib/program-search'
import { loadLastSearchConditions, saveLastSearchConditions } from '@/lib/search-storage'
import {
  epgWindowDays,
  estimateRuleCost,
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
  // nowMs はこのレンダーの間で一貫させる（`pages/home.tsx`・`pages/programs.tsx`
  // と同じ規律）。容量ノートの問い合わせ窓（下の `shortfallWindowStartMs`）だけが使う。
  // 検索結果の容量窓はクエリ再取得ごとの「いま」を使う。時刻を mount 時に固定
  // すると、日境界をまたいだ後も古い窓を問い合わせ続ける。
  // oxlint-disable-next-line react/purity -- クエリ再取得ごとの現在時刻スナップショットが必要
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
    services: serviceList,
    isPending: registryPending,
    isError: registryError,
    refetch: refetchRegistry,
  } = useAllSitesServices()
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
  useEffect(() => {
    searchRef.current = search
  }, [search])

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

    const nextDraft = conditionsToDraft(sourceRule)
    // URL の共有条件をフォームと検索へ反映する外部入力同期。
    // oxlint-disable-next-line react/set-state-in-effect -- URL 条件をローカルフォームへ同期する
    setDraft(nextDraft)
    // oxlint-disable-next-line react/set-state-in-effect -- URL 条件に対応する表示件数を同期する
    setVisibleCount(pageSize)
    searchRef.current.mutate({ data: buildSearchRequest(nextDraft) })
  }, [ruleId, sourceRule])

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
   *
   * **`submit` と違い、ここは `registryPending` / `registryError` で止めない。**
   * `submit` を registry で止めるのは、条件フォームのチップがまだ出ていない
   * 状態でユーザーに押させないため --- URL が運ぶ条件は既に完成した値なので、
   * その理由が当たらない。加えて `registryError` はほぼ api ロール自身の不調
   * （`GET /api/sites` は config、`/services` は同じプロセスの DB 射影。
   * `lib/all-sites-services.ts` 参照）なので、ここで止めても検索自体が同じ
   * プロセスで失敗するだけで救えるものが無く、止めると共有リンクが運んできた
   * 条件がフォームから無言で消える。
   */
  const appliedCondRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    if (ruleId !== undefined) return
    const cond = routeSearch.cond
    const nextDraft = cond === undefined ? undefined : conditionsToDraft(cond)
    const encoded =
      nextDraft === undefined ? undefined : JSON.stringify(buildSearchRequest(nextDraft))
    if (appliedCondRef.current === encoded) return
    appliedCondRef.current = encoded
    if (nextDraft === undefined) return

    // URL の共有条件をフォームと検索へ反映する外部入力同期。
    // oxlint-disable-next-line react/set-state-in-effect -- URL 条件をローカルフォームへ同期する
    setDraft(nextDraft)
    // oxlint-disable-next-line react/set-state-in-effect -- URL 条件に対応する表示件数を同期する
    setVisibleCount(pageSize)
    searchRef.current.mutate({ data: buildSearchRequest(nextDraft) })
  }, [ruleId, routeSearch.cond])

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
    if (error !== undefined || registryPending || registryError) return
    const request = buildSearchRequest(draft)
    setVisibleCount(pageSize)
    pendingResultScrollRef.current = true
    search.mutate({ data: request })
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
