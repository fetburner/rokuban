import { createRootRoute, createRoute, HeadContent, Outlet } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { SiteGate } from './components/site-gate'
import { pageTitle } from './lib/document-title'
import {
  parseProgramsSearch,
  serviceIdSchema,
  type ProgramsPageSearch,
} from './lib/programs-search'
import {
  parseRecordingsSearch,
  parseRuleId,
  type RecordingsPageSearch,
} from './lib/recording-search'
import { asInteger, validValue } from './lib/url-search'
import { HomePage } from './pages/home'
import { LivePage } from './pages/live'
import { ProgramsPage } from './pages/programs'
import { RecordingDetailPage } from './pages/recording-detail'
import { RecordingsPage } from './pages/recordings'
import { ReservationDetailPage } from './pages/reservation-detail'
import { ReservationsPage } from './pages/reservations'
import { RulesPage } from './pages/rules'
import { SearchPage } from './pages/search'

const rootRoute = createRootRoute({
  // 各ルートが `head` で自分の画面名を積むので、ここは「積み忘れ」への保険
  // （issue #304。子の `head` が無ければこの既定に落ちる。TanStack Router は
  // 末端から見て最初に見つかったタイトルを使うので、子が定義すればここは
  // 上書きされる）。
  head: () => ({ meta: [{ title: '録番' }] }),
  component: () => (
    <>
      {/* HeadContent が各ルートの `head` を実際の <title>/<meta> に描く。React 19
          はツリーのどこに描いても <head> へ hoist するので、AppShell の中でよい
          （SSR ではないので位置そのものに意味は無い）。
          ただし `<HeadContent />` 自身がこの component（root route）の一部
          なので、子ルートの component がレンダー中に例外を投げ、かつどの
          ルートも `errorComponent` を持たない場合、この component ごと
          （`<HeadContent />` も含めて）汎用のフォールバック画面に差し替わり、
          `document.title` は「積み忘れへの保険」（下記 `head` の既定値）にすら
          戻らず空文字になる（`<title>` 要素自体が無くなるため。実測: `head`
          未設定の `errorComponent` 無しルートがレンダーで例外を投げると、
          コンソールに `Warning: The following error wasn't caught by any
          route!` が出て body が丸ごと入れ替わる）。今のところどのルートも
          `errorComponent` を持たないので、この置き換えはどのルートの詳細
          画面が例外を投げても起こりうる --- 詳細ルートのテスト
          （`routes.test.tsx`）が、この 28ms 程度の過渡状態でアサーションが
          偽陽性で通らないよう、レンダーが収束してからの再アサートを入れて
          いるのはこのため。 */}
      <HeadContent />
      <AppShell>
        {/* SiteGate は Outlet だけを囲む。ナビゲーション（サイドバー/ボトムタブ）と
            サーキットブレーカーバナーは site に依存しないので、サイト解決を待たせない。 */}
        <SiteGate>
          <Outlet />
        </SiteGate>
      </AppShell>
    </>
  ),
})

/**
 * `/` はホーム（M8-3, issue #242）。番組表は `/programs` へ移設した --- 起動して
 * 最初に見えるのが「これから録るもの」（番組表）ではなく「録れているか・今夜
 * なにが録れるか・見るものはあるか・異常はないか」に 1 画面で答える場所になる。
 */
const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  // ナビ（`components/app-shell.tsx` の `navItems`）・`PageHeader` と同じ
  // 「ホーム」を使う。
  head: () => ({ meta: [{ title: pageTitle('ホーム') }] }),
  component: HomePage,
})

/**
 * 番組表のチャンネル絞り込みは `?service=<Service.id>` に持つ。壊れた
 * 値は `parseProgramsSearch` が要素ごとに落とすので、壊れたリンクでも画面は開く。
 *
 * ルートは `/programs`（M8-3 でホームに `/` を譲った。上記 `homeRoute` 参照）。
 */
const programsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/programs',
  validateSearch: (search: Record<string, unknown>): ProgramsPageSearch =>
    parseProgramsSearch(search),
  // `pages/programs.tsx` の `<PageHeader title="番組">` と同じ表記。
  head: () => ({ meta: [{ title: pageTitle('番組') }] }),
  component: ProgramsPage,
})

/** SearchPageSearch は `/search` のクエリパラメータ。 */
export type SearchPageSearch = {
  /**
     * 開いたときにルールの条件を下書きへ写す元のルール id（省略可）。
     * ルール画面が `<Link to="/search" search={{ ruleId }}>` で渡し、検索画面が
     * `useSearch()` で読む。互いのページを見ずに実装できるよう、消費側の実装
     * より前にこの型だけ決めておく。
     */
  ruleId?: number
}

/**
 * 検索は番組表とは別のルートに置く。番組表は「EPG を時間軸で眺める」画面だが、
 * 検索は ruler と同じ条件コンパイラを叩く「ルールの条件を試す」画面で、
 * 仕事が違う（issue #24 M2-11）。
 */
const searchRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/search',
  // 不正な値（数値に変換できない・非整数・NaN・Infinity）は undefined に落とす。
  // 存在しないルール id を積んだ壊れたリンクを踏んでも、検索画面は
  // 「ruleId 指定なし」の通常の検索フォームとして開ける。
  //
  // **落とす次元にも `undefined` を明示代入する**（`parseRecordingsSearch` /
  // `/live` の `service` と同じ形。issue #194）。TanStack Router は非 strict
  // モードで `{ ...生の location.search, ...validateSearch の戻り値 }` の順に
  // 合成するので、キーを省略すると生の値（`/search?ruleId=abc` なら文字列
  // `"abc"`）がそのまま残り、`pages/search.tsx` が `ruleId !== undefined` を
  // 真と判断して `GET /api/rules/abc` を発火させていた。
  //
  // `parseRuleId` は `/recordings` の `ruleId`（`lib/recording-search.ts`）と
  // 同じ `rules.id` を扱うキーなので、パースの流儀をそちらと共有する。
  validateSearch: (search: Record<string, unknown>): SearchPageSearch => ({
    ruleId: parseRuleId(search.ruleId),
  }),
  // `pages/search.tsx` の `<PageHeader title="検索">` と同じ表記。
  head: () => ({ meta: [{ title: pageTitle('検索') }] }),
  component: SearchPage,
})

const rulesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/rules',
  // `pages/rules.tsx` の `<PageHeader title="ルール">` と同じ表記。
  head: () => ({ meta: [{ title: pageTitle('ルール') }] }),
  component: RulesPage,
})

const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reservations',
  // `pages/reservations.tsx` の `<PageHeader title="予約">` と同じ表記。
  head: () => ({ meta: [{ title: pageTitle('予約') }] }),
  component: ReservationsPage,
})

/**
 * 予約詳細のディープリンクは `(site, programId)` を宛先にする（issue #99）。
 *
 * `reservations.id` は ruler の導出削除・再実体化（EPG フリッカー・ルール編集）
 * で変わりうる不安定な値なので、旧 `/reservations/$reservationId` を宛先に
 * ブックマーク・共有した URL は、予約が再実体化されると 404 になっていた。
 * `(site, programId)` は `UNIQUE (site, program_id)` があるキーなので、
 * 予約行が作り直されても同じ URL で引ける
 * （`GET /api/sites/{site}/programs/{programId}/reservation`）。
 */
const reservationDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reservations/$site/$programId',
  // `head` は Awaitable（`loader` が積む `loaderData` を待って動的な値を
  // 返せる）だが、このルートに `loader` は無い --- 番組名は
  // `useGetProgramReservation`（react-query。コンポーネント側で取得する）が
  // 持っていて、`head` からは見えない。今の構成には待てる対象がそもそも無い、
  // というだけで、`loader` を足せば動的な題名も原理的には積める。ここでは
  // その架け替え（loader を新設し、コンポーネント側の react-query 取得と
  // 二重管理させない）をこの issue の範囲外とし、`pages/
  // reservation-detail.tsx` の `<h1>` と同じ「予約の詳細」という画面の
  // 識別子に留めた（番組名を積むと、無い間 `undefined · 録番` が一瞬出る
  // ことにもなる。CLAUDE.md テスト規律「非同期の空虚な成功に注意する」と
  // 同じ穴）。番組名は本文の `<h2>` に任せる。
  head: () => ({ meta: [{ title: pageTitle('予約の詳細') }] }),
  component: ReservationDetailPage,
})

/**
 * 録画検索は `/recordings` に同居する（別ルートにしない。issue #137）。
 * 条件は URL に載せ、リロード・共有・戻るで同じ結果になるようにする。
 * 不正な値は `/search` の `ruleId` と同じ流儀で落とし、壊れたリンクを踏んでも
 * 「その条件なし」で開ける（`lib/recording-search.ts` の `parseRecordingsSearch`）。
 */
const recordingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/recordings',
  validateSearch: (search: Record<string, unknown>): RecordingsPageSearch =>
    parseRecordingsSearch(search),
  // `pages/recordings.tsx` の `<PageHeader title="録画">` と同じ表記。
  head: () => ({ meta: [{ title: pageTitle('録画') }] }),
  component: RecordingsPage,
})

/**
 * 録画単体の着地先（issue #232 M6-4）。`recordings.id` は ingest（watcher）が
 * 一度作ったら変わらない不可逆な事実の id なので、`/reservations/$site/$programId`
 * （issue #99、`reservations.id` が ruler の再実体化で変わるため id を避けた）と
 * 違って id をそのまま URL に使ってよい。
 */
const recordingDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/recordings/$id',
  // 録画名は `useGetRecording`（react-query。コンポーネント側で取得する）が
  // 持っていて、`loader` を持たないこのルートの `head` からは見えない
  // （`reservationDetailRoute` と同じ理由。詳細はそちらのコメント）。`pages/
  // recording-detail.tsx` の `<h1>` と同じ「録画の詳細」に留める。
  head: () => ({ meta: [{ title: pageTitle('録画の詳細') }] }),
  component: RecordingDetailPage,
})

/** LivePageSearch は `/live` のクエリパラメータ。 */
export type LivePageSearch = {
  /**
   * 視聴中のチャンネル（`Service.id`。省略時・一覧に無い id は番組を持つ先頭の
   * サービスに落ちる。`lib/live.ts` の `pickInitialService`）。`/programs` /
   * `/recordings` の `?service=` と同じ id 空間。
   */
  service?: number
}

/**
 * ライブ視聴は独立したルートに置く（issue #92 の着手時コメント）。番組表グリッド
 * は `lg` 以上でしか出ない（`docs/frontend.md`「リストを第一級に置く」）ため、
 * グリッドの「いま」から入る形にすると入口がモバイルとデスクトップで割れる。
 */
const liveRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/live',
  // 壊れた/古いリンクを踏んでも「チャンネル指定なし」の通常表示（先頭チャンネル）
  // に落ちる。**SI の 2 値をそのまま運んでいた旧クエリ形式の後方互換は持たない**
  // （issue #438。`5ab06f8` と同じ判断）--- `LivePageSearch` はそのキーを持たない
  // ので、旧リンクは「チャンネル指定なし」と同じ扱いになる。
  //
  // **落とす次元にも `undefined` を明示代入する**（`parseRecordingsSearch` と
  // 同じ形。issue #194）。TanStack Router は非 strict モードで
  // `{ ...生の location.search, ...validateSearch の戻り値 }` の順に合成するので、
  // キーを省略すると生の値（`/live?service=abc` なら文字列 `"abc"`）がそのまま
  // 残り、`LivePageSearch` の `service?: number` という型が実行時に嘘になる。
  // 旧形式の `networkId` / `serviceId` は型から消えたので上書きされず、
  // 旧ブックマークではアドレスバーに残ったままになる（読者が 1 人も居ないので
  // 実害は無く、チャンネルを 1 回選べばオブジェクト指定の `search` が丸ごと
  // 置き換えて消える。実測）。消すために型へ「持たないキー」を書き足すのは、
  // 落としたはずの旧形式の知識を持ち続けることになるのでしない。
  //
  // **`?service=` は `/programs` / `/recordings` と同じ生成スキーマ
  // （`serviceIdSchema`）と同じアダプタ（`validValue` + `asInteger`）で検証する。**
  // 同じ名前・同じ id 空間のパラメータを別の流儀で検証すると値域が食い違う
  // （`Service.id` の上限は openapi の `maximum` が権威。
  // `internal/api/spec_bounds_test.go`）。整数性・安全整数の判定は `asInteger`
  // が持つ（生成スキーマに `.int()` は出ない。`lib/url-search.ts`）。
  validateSearch: (search: Record<string, unknown>): LivePageSearch => ({
    service: validValue(serviceIdSchema, search.service, { coerce: asInteger }),
  }),
  // `pages/live.tsx` の `<PageHeader title="ライブ">` と同じ表記。issue #304 は
  // Playwright で確認した 6 ルートを挙げているが、`/live` だけ `head` を
  // 積まないと、`headContentUtils.js` が末端の match から見て最初に見つかった
  // title を使う仕様上、上記 rootRoute の既定（画面名の付かない「録番」だけ）に
  // 落ちる（実測: ルール → `/live` で `document.title === '録番'`。「直前の
  // ルールのタイトルが残る」わけではない --- rootRoute の既定はフロアとして
  // 必ず勝つ）。それでも画面を名乗れない既定のままにする理由は無いので、
  // 主要ルートと同じ扱いにする。
  head: () => ({ meta: [{ title: pageTitle('ライブ') }] }),
  component: LivePage,
})

export const routeTree = rootRoute.addChildren([
  homeRoute,
  programsRoute,
  searchRoute,
  rulesRoute,
  reservationsRoute,
  reservationDetailRoute,
  recordingsRoute,
  recordingDetailRoute,
  liveRoute,
])
