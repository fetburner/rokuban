import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { ProgramsPage } from './pages/programs'
import { RecordingsPage } from './pages/recordings'
import { ReservationDetailPage } from './pages/reservation-detail'
import { ReservationsPage } from './pages/reservations'
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

/**
 * 検索は番組表とは別のルートに置く。番組表は「EPG を時間軸で眺める」画面だが、
 * 検索は ruler と同じ条件コンパイラを叩く「ルールの条件を試す」画面で、
 * 関心事が違う（issue #24 M2-11）。
 */
const searchRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/search',
  component: SearchPage,
})

const reservationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reservations',
  component: ReservationsPage,
})

const reservationDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reservations/$reservationId',
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
  reservationsRoute,
  reservationDetailRoute,
  recordingsRoute,
])
