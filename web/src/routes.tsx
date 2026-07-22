import { createRootRoute, createRoute, Outlet } from '@tanstack/react-router'
import { IndexPage } from './pages/index'

const rootRoute = createRootRoute({
  component: () => (
    <div className="min-h-screen bg-background text-foreground">
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
