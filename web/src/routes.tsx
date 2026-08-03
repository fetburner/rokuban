import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { ProgramsPage } from './pages/programs'
import { RecordingsPage } from './pages/recordings'
import { ReservationDetailPage } from './pages/reservation-detail'
import { ReservationsPage } from './pages/reservations'
import { RulesPage } from './pages/rules'
import { SearchPage } from './pages/search'

const rootRoute = createRootRoute({
  component: () => (
    <AppShell>
      <Outlet />
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

const recordingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/recordings',
  component: RecordingsPage,
})

export const routeTree = rootRoute.addChildren([
  programsRoute,
  searchRoute,
  rulesRoute,
  reservationsRoute,
  reservationDetailRoute,
  recordingsRoute,
])
