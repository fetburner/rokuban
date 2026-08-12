import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { SiteGate } from './components/site-gate'
import { parseRecordingsSearch, type RecordingsPageSearch } from './lib/recording-search'
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

const programsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
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
  // 不正な値（数値に変換できない・NaN・Infinity）は undefined に落とす。
  // 存在しないルール id を積んだ壊れたリンクを踏んでも、検索画面は
  // 「ruleId 指定なし」の通常の検索フォームとして開ける
  validateSearch: (search: Record<string, unknown>): SearchPageSearch => {
    const raw = search.ruleId
    const n = typeof raw === 'number' ? raw : typeof raw === 'string' ? Number(raw) : NaN
    return Number.isFinite(n) ? { ruleId: n } : {}
  },
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
  validateSearch: (search: Record<string, unknown>): LivePageSearch => {
    const raw = search.serviceId
    const n = typeof raw === 'number' ? raw : typeof raw === 'string' ? Number(raw) : NaN
    return { serviceId: Number.isInteger(n) && n > 0 ? n : undefined }
  },
  component: LivePage,
})

export const routeTree = rootRoute.addChildren([
  programsRoute,
  searchRoute,
  rulesRoute,
  reservationsRoute,
  reservationDetailRoute,
  recordingsRoute,
  recordingDetailRoute,
  liveRoute,
])
