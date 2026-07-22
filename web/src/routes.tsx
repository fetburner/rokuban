import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'
import { IndexPage } from './pages/index'

const rootRoute = createRootRoute({
  component: () => (
    <div className="min-h-screen bg-zinc-950 text-zinc-100">
      <Outlet />
    </div>
  ),
})

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: IndexPage,
})

export const routeTree = rootRoute.addChildren([indexRoute])
