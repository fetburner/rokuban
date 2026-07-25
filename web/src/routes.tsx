import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'

import { AppShell } from './components/app-shell'
import { ProgramsPage } from './pages/programs'
import { RecordingsPage } from './pages/recordings'
import { ReservationDetailPage } from './pages/reservation-detail'
import { ReservationsPage } from './pages/reservations'

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
  reservationsRoute,
  reservationDetailRoute,
  recordingsRoute,
])
