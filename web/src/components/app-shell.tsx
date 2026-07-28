import { Link, useRouterState } from '@tanstack/react-router'
import { CalendarClock, ListVideo, Tv } from 'lucide-react'
import type { ComponentType } from 'react'

import { CircuitBreakerBanner } from '@/components/circuit-breaker-banner'
import { cn } from '@/lib/utils'

type NavItem = {
  to: string
  label: string
  icon: ComponentType<{ className?: string }>
}

/**
 * 主ナビゲーションの行き先。モバイルのボトムタブとデスクトップのサイドバーで
 * 同じ定義を使う（ルート定義は 1 つで、レイアウトだけが切り替わる）。
 */
const navItems: NavItem[] = [
  { to: '/', label: '番組', icon: Tv },
  { to: '/reservations', label: '予約', icon: CalendarClock },
  { to: '/recordings', label: '録画', icon: ListVideo },
]

function useActivePath(): string {
  return useRouterState({ select: (s) => s.location.pathname })
}

function isActive(pathname: string, to: string): boolean {
  return to === '/' ? pathname === '/' : pathname.startsWith(to)
}

/**
 * BottomTabs はモバイル向けの主ナビゲーション。
 *
 * 下端にぴったり付けず `env(safe-area-inset-bottom)` + 最低 8px の余白を空ける。
 * iOS のホームインジケータと重なるのを防ぎ、Android のジェスチャーナビの
 * 「下端から上スワイプでホーム」領域からも離す。
 */
function BottomTabs() {
  const pathname = useActivePath()

  return (
    <nav
      aria-label="主ナビゲーション"
      className="fixed inset-x-0 bottom-0 z-20 border-t border-border bg-background/95 pb-[var(--bottom-nav-inset)] backdrop-blur md:hidden"
    >
      <ul className="flex">
        {navItems.map(({ to, label, icon: Icon }) => {
          const active = isActive(pathname, to)
          return (
            <li key={to} className="flex-1">
              <Link
                to={to}
                aria-current={active ? 'page' : undefined}
                // min-h-14 で最小タップ領域（44px 以上）を確保する
                className={cn(
                  'flex min-h-14 flex-col items-center justify-center gap-0.5 text-xs transition-colors',
                  active ? 'text-primary' : 'text-muted-foreground',
                )}
              >
                <Icon className="size-5" />
                {label}
              </Link>
            </li>
          )
        })}
      </ul>
    </nav>
  )
}

/** Sidebar はデスクトップ向けの主ナビゲーション。 */
function Sidebar() {
  const pathname = useActivePath()

  return (
    <nav
      aria-label="主ナビゲーション"
      className="hidden w-48 shrink-0 border-r border-border md:flex md:flex-col"
    >
      <div className="px-4 py-5 text-lg font-semibold tracking-tight">録番</div>
      <ul className="flex flex-col gap-1 px-2">
        {navItems.map(({ to, label, icon: Icon }) => {
          const active = isActive(pathname, to)
          return (
            <li key={to}>
              <Link
                to={to}
                aria-current={active ? 'page' : undefined}
                className={cn(
                  'flex items-center gap-2 rounded-lg px-3 py-2 text-sm transition-colors',
                  active
                    ? 'bg-muted font-medium text-foreground'
                    : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
                )}
              >
                <Icon className="size-4" />
                {label}
              </Link>
            </li>
          )
        })}
      </ul>
    </nav>
  )
}

/**
 * AppShell は全ページ共通の外枠。
 * モバイルはボトムタブ、`md` 以上はサイドバーに切り替わる。
 */
export function AppShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen bg-background text-foreground">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        {/* サーキットブレーカー発動中はどのページでも見えるよう、
            ルーティングされる children の外・全ページ共通の位置に置く */}
        <CircuitBreakerBanner />
        {/* ボトムタブに隠れないよう、モバイルでは下に余白を足す */}
        <main className="min-w-0 flex-1 pb-[var(--bottom-nav-height)] md:pb-0">
          {children}
        </main>
      </div>
      <BottomTabs />
    </div>
  )
}
