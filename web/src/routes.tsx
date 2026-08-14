import { createRootRoute, createRoute, Outlet, redirect } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { SiteGate } from './components/site-gate'
import { parsePositiveIntId } from './lib/positive-id'
import { parseProgramsSearch, type ProgramsPageSearch } from './lib/programs-search'
import {
  parseRecordingsSearch,
  parseRuleId,
  type RecordingsPageSearch,
} from './lib/recording-search'
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
  component: () => (
    <AppShell>
      {/* SiteGate は Outlet だけを囲む。ナビゲーション（サイドバー/ボトムタブ）と
          サーキットブレーカーバナーは site に依存しないので、サイト解決を待たせない。 */}
      <SiteGate>
        <Outlet />
      </SiteGate>
    </AppShell>
  ),
})

/**
 * `/` はホーム（M8-3, issue #242）。番組表は `/programs` へ移設した --- 起動して
 * 最初に見えるのが「これから録るもの」（番組表）ではなく「録れているか・今夜
 * なにが録れるか・見るものはあるか・異常はないか」に 1 画面で答える場所になる。
 *
 * **裸の `/` はホームへ、`?serviceId=` か `?at=` が付いた `/` だけ `/programs` へ
 * リダイレクトする（下記 `homeRoute` の `beforeLoad`）。** この 2 つは番組表固有の
 * クエリで、`/` が番組表だった頃に外部（共有・ブラウザ履歴）へ出た URL
 * （容量不足バッジの `?at=`・ライブ「この局の番組表」の `?serviceId=`）を救うため。
 * リポジトリ内の発行元はこの PR で `/programs` に直したので、リダイレクトが
 * 実際に効くのは外部に残った旧リンクだけになる。区別できない裸の `/` は新しい
 * 意味（ホーム）を優先する --- 番組表はナビの 2 番目にあるので、踏んだ人の
 * コストはクリック 1 回にとどまる。
 */
const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  // このルート自身は validateSearch を持たない（ホームは検証すべきクエリ次元を
  // 持たない）。TanStack Router の非 strict モードは未検証の生の location.search
  // をそのまま素通しするので、`beforeLoad` はここで `?serviceId=` / `?at=` の
  // **有無**だけを見る（値の形は問わない --- 形の検証は `/programs` 側
  // （`parseProgramsSearch`）の仕事のままにする。ここで検証すると「検証は
  // 誰の仕事か」が 2 箇所に散る）。
  beforeLoad: ({ search }) => {
    const raw = search as Record<string, unknown>
    if (raw.serviceId !== undefined || raw.at !== undefined) {
      // `search: true` で現在の location.search をそのまま引き継ぐ。ここで
      // 値を正規化・再構築しない --- `/programs` 側の `validateSearch` が
      // 同じ生の search を検証するので、二重に検証を書かない。
      throw redirect({ to: '/programs', search: true, replace: true })
    }
  },
  component: HomePage,
})

/**
 * 番組表のチャンネル絞り込みは URL の `?serviceId=` に持つ（issue #231）。
 * `/recordings` の `serviceId` と同じ形（`number[]`。複数可・OR）で、絞り込み
 * 済みの番組表への深いリンクや共有ができるようにする。壊れた値は
 * `parseProgramsSearch`（`/recordings` の `parseRecordingsSearch` と同じ流儀）が
 * 落とすので、壊れたリンクを踏んでも「絞り込みなし」で開ける。
 *
 * ルートは `/programs`（M8-3 でホームに `/` を譲った。上記 `homeRoute` 参照）。
 */
const programsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/programs',
  validateSearch: (search: Record<string, unknown>): ProgramsPageSearch =>
    parseProgramsSearch(search),
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
  // `/live` の `serviceId` と同じ形。issue #194）。TanStack Router は非 strict
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
  component: SearchPage,
})

const rulesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/rules',
  component: RulesPage,
})

const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reservations',
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
  component: RecordingDetailPage,
})

/** LivePageSearch は `/live` のクエリパラメータ。 */
export type LivePageSearch = {
  /** 視聴中のチャンネル（省略時は番組を持つ先頭のサービスに落ちる。`lib/live.ts` の `pickInitialServiceId`）。 */
  serviceId?: number
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
  // に落ちる。
  //
  // **落とす次元にも `undefined` を明示代入する**（`parseRecordingsSearch` と
  // 同じ形。issue #194）。TanStack Router は非 strict モードで
  // `{ ...生の location.search, ...validateSearch の戻り値 }` の順に合成するので、
  // キーを省略すると生の値（`/live?serviceId=abc` なら文字列 `"abc"`）がそのまま
  // 残り、`LivePageSearch` の `serviceId?: number` という型が実行時に嘘になる。
  // いまの唯一の読者（`pickInitialServiceId`）は厳密比較なので実害は無いが、
  // `serviceId` を `livePlaylistURL` に直接渡す読者が 1 人増えた瞬間に
  // `/api/.../services/abc/live/playlist.m3u8` が飛ぶ
  //
  // `parsePositiveIntId`（`lib/positive-id.ts`）を使う（issue #275）。以前は
  // `Number.isInteger(n) && n > 0` だけを見ており、`Number.MAX_SAFE_INTEGER` を
  // 超える値が黙って別の値に丸まる経路（`Number('9007199254740993')` が既に
  // `9007199254740992` になる）を塞いでいなかった。`ruleId`（`lib/recording-search.ts`
  // の `parseRuleId`）と同じ「シーケンス/SI 由来で 1 以上しか存在しない識別子」の
  // 流儀なので、この PR で共有ヘルパーへ寄せた。
  validateSearch: (search: Record<string, unknown>): LivePageSearch => ({
    serviceId: parsePositiveIntId(search.serviceId),
  }),
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
