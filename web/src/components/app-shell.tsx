import { Link, useRouterState } from '@tanstack/react-router'
import {
  CalendarClock,
  ListVideo,
  Menu,
  MoreHorizontal,
  Radio,
  Search,
  Settings2,
  Tv,
} from 'lucide-react'
import type { ComponentType } from 'react'
import { useEffect, useState } from 'react'

import { CircuitBreakerBanner } from '@/components/circuit-breaker-banner'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { cn } from '@/lib/utils'

/**
 * サイドバーの畳み状態を持続させる localStorage キー。
 * `lib/playback-position.ts` の `rokuban:<関心事>:...` という命名に揃える。
 */
const SIDEBAR_COLLAPSED_KEY = 'rokuban:sidebar:collapsed'

/** loadSidebarCollapsed は保存済みの畳み状態を返す。無い/読めない場合は false（展開）。 */
function loadSidebarCollapsed(): boolean {
  try {
    return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === '1'
  } catch {
    // private mode 等で localStorage が使えない場合は展開扱い
    return false
  }
}

/** saveSidebarCollapsed は畳み状態を保存する。 */
function saveSidebarCollapsed(collapsed: boolean): void {
  try {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? '1' : '0')
  } catch {
    // ignore
  }
}

type NavItem = {
  to: string
  label: string
  icon: ComponentType<{ className?: string }>
}

/**
 * 主ナビゲーションの行き先。モバイルのボトムタブとデスクトップのサイドバーで
 * 同じ定義を使う（ルート定義は 1 つで、レイアウトだけが切り替わる）。
 *
 * 並びは実装順ではなく**触る頻度**で決める（docs/frontend/design.md §頻度 3 段）。
 * 単一世帯の運用を前提にした仮説であり、実測ではない:
 *
 * - 一等地: 番組（毎回の起点）・録画（視聴のたび）
 * - 中間: 予約（差分・重複の確認は毎日ではないが週次では触る）・
 *   ライブ（視聴のたびではないが番組ほど反復しない）
 * - 端: 検索（意図して探すときだけ）・ルール（set-and-forget、月数回以下）
 */
const navItems: NavItem[] = [
  { to: '/', label: '番組', icon: Tv },
  { to: '/recordings', label: '録画', icon: ListVideo },
  { to: '/reservations', label: '予約', icon: CalendarClock },
  { to: '/live', label: 'ライブ', icon: Radio },
  { to: '/search', label: '検索', icon: Search },
  { to: '/rules', label: 'ルール', icon: Settings2 },
]

/**
 * モバイルのボトムタブに常時出す項目数。残りは `MoreMenu`（「その他」）に畳む。
 *
 * 一等地の 2 個（番組・録画）に加えて予約も常時タブに出す。理由: 予約は
 * 「今夜これで録れているか」を録画そのものと同じくらいの頻度で確認する
 * 対象という仮説を置いた（ライブは番組ほど毎回開かないと見て「その他」側）。
 * この仮説も上と同じく実測ではない。
 */
const MOBILE_PRIMARY_COUNT = 3
const mobilePrimaryItems = navItems.slice(0, MOBILE_PRIMARY_COUNT)
/** モバイルで「その他」に畳む項目（ライブ・検索・ルール）。 */
const moreNavItems = navItems.slice(MOBILE_PRIMARY_COUNT)

function useActivePath(): string {
  return useRouterState({ select: (s) => s.location.pathname })
}

function isActive(pathname: string, to: string): boolean {
  return to === '/' ? pathname === '/' : pathname.startsWith(to)
}

/**
 * MoreMenu はボトムタブの「その他」。頻度が低い項目（`moreNavItems`）を
 * ポップオーバーに畳んで、ボトムタブの本数を親指の届く 4 個に抑える。
 *
 * シートではなくポップオーバーを選んだ理由: 中身がリンク 3 個だけの単純な
 * リストで、フォーム操作や長いコンテンツを持たない。全画面/半画面を覆う
 * シートはここでは過剰で、トリガーの直上に浮かせるポップオーバーの方が
 * 「タブの延長」に見える。
 *
 * 固定されたボトムバーの上に浮くオーバーレイなので、画面端でのはみ出し・
 * バーの上に出るか・safe-area との重なりは jsdom では測れない
 * （docs/frontend/shell.md）。実ブラウザでの合否は `e2e/design.mjs` の
 * 「その他」判定が担う。
 *
 * トリガー自身のアクティブ表示は、配下の項目（ライブ・検索・ルール）の
 * いずれかが現在地のときに立てる。個々のリンクは自身のページに居るときだけ
 * `aria-current="page"` を持つのに対し、トリガーはページそのものではなく
 * 「その他」という集合の現在地を表すので `aria-current="true"` を使う
 * （両方 `undefined` にならないよう、両方向をテストで固定する）。
 */
function MoreMenu({ pathname }: { pathname: string }) {
  const [open, setOpen] = useState(false)
  const active = moreNavItems.some((item) => isActive(pathname, item.to))

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        aria-current={active ? 'true' : undefined}
        // min-h-14 で最小タップ領域（44px 以上）を確保する。他タブと高さを揃える
        className={cn(
          'flex min-h-14 w-full flex-col items-center justify-center gap-0.5 text-xs transition-colors',
          active ? 'text-primary' : 'text-muted-foreground',
        )}
      >
        <MoreHorizontal className="size-5" />
        その他
      </PopoverTrigger>
      <PopoverContent
        side="top"
        align="end"
        sideOffset={8}
        aria-label="その他のナビゲーション"
        className="w-44 p-1"
      >
        <ul className="flex flex-col gap-0.5">
          {moreNavItems.map(({ to, label, icon: Icon }) => {
            const itemActive = isActive(pathname, to)
            return (
              <li key={to}>
                <Link
                  to={to}
                  aria-current={itemActive ? 'page' : undefined}
                  onClick={() => setOpen(false)}
                  className={cn(
                    'flex items-center gap-2 rounded-md px-3 py-2.5 text-sm transition-colors',
                    itemActive
                      ? 'bg-muted font-medium text-foreground'
                      : 'text-foreground hover:bg-muted/60',
                  )}
                >
                  <Icon className="size-4 shrink-0" />
                  {label}
                </Link>
              </li>
            )
          })}
        </ul>
      </PopoverContent>
    </Popover>
  )
}

/**
 * BottomTabs はモバイル向けの主ナビゲーション。
 *
 * 下端にぴったり付けず `env(safe-area-inset-bottom)` + 最低 8px の余白を空ける。
 * iOS のホームインジケータと重なるのを防ぎ、Android のジェスチャーナビの
 * 「下端から上スワイプでホーム」領域からも離す。
 *
 * 常時表示するのは `mobilePrimaryItems`（頻度の一等地〜中間の一部）+
 * 「その他」の 4 個まで。決めた理由は誤タップ対策ではなく、頻度の低い
 * 3 項目（ライブ・検索・ルール）を畳んで一等地のタブを広く取ること
 * （実測 @390px: 4 タブ = 97.5px/個、6 タブなら 65px/個。どちらも
 * `min-h-14` が確保する最小タップ領域 44px は満たす）。
 */
function BottomTabs() {
  const pathname = useActivePath()

  return (
    <nav
      aria-label="主ナビゲーション"
      className="fixed inset-x-0 bottom-0 z-20 border-t border-border bg-background/95 pb-[var(--bottom-nav-inset)] backdrop-blur md:hidden"
    >
      <ul className="flex">
        {mobilePrimaryItems.map(({ to, label, icon: Icon }) => {
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
        <li className="flex-1">
          <MoreMenu pathname={pathname} />
        </li>
      </ul>
    </nav>
  )
}

/**
 * Sidebar はデスクトップ向けの主ナビゲーション。
 *
 * `sticky top-0` + `h-dvh` でビューポートに固定する。`position: fixed` にして
 * 本文側に手動 margin を持たせる形は取らない —— サイドバーは flex のフローに
 * 残ったままなので、本文の左オフセットは flex レイアウトが自動で確保する。
 * 中身（ロゴ行 + ナビ項目）は `flex flex-col` にして、ナビ項目側だけ
 * `overflow-y-auto` にする。画面が低くて項目が入りきらない場合に、
 * ロゴ・トグル行を固定したままナビ項目だけ縦スクロールできるようにするため。
 *
 * ハンバーガートグルで幅 `w-48`（展開）⟷ `w-[52px]`（アイコンのみのレール）
 * を切り替える。**完全に隠さない**のは、トグル自体をレールの先頭に常駐させて
 * どのページからでも展開に戻せるようにするため。
 * `pages/reservation-detail.tsx` は他ページと違い `components/page.tsx` の
 * `PageHeader` を使わず独自の `<header>` を持つため、トグルをヘッダー側に
 * 置く設計だとこのページだけサイドバーを戻す手段が無くなる。ページ側に
 * 「トグルを置いてもらう」協力を要求しない形として、サイドバー自身に
 * トグルを持たせている。
 *
 * 畳んだ状態でもラベルは DOM から消さず `sr-only` にする。アイコンだけの
 * 見た目でも `getByRole('link', { name: label })` が引ける（スクリーン
 * リーダーの読み上げ名も失わない）。マウス向けには `title` を補う。
 */
function Sidebar() {
  const pathname = useActivePath()
  const [collapsed, setCollapsed] = useState(() => loadSidebarCollapsed())

  useEffect(() => {
    saveSidebarCollapsed(collapsed)
  }, [collapsed])

  const toggleLabel = collapsed ? 'ナビゲーションを開く' : 'ナビゲーションを畳む'

  return (
    <nav
      aria-label="主ナビゲーション"
      className={cn(
        'sticky top-0 hidden h-dvh shrink-0 flex-col border-r border-border md:flex',
        collapsed ? 'w-[52px]' : 'w-48',
      )}
    >
      <div
        className={cn(
          'flex shrink-0 items-center gap-2 py-4',
          collapsed ? 'justify-center px-0' : 'px-3',
        )}
      >
        <button
          type="button"
          onClick={() => setCollapsed((v) => !v)}
          aria-expanded={!collapsed}
          aria-label={toggleLabel}
          title={toggleLabel}
          className="flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted/60 hover:text-foreground"
        >
          <Menu className="size-5" />
        </button>
        {!collapsed && (
          <span className="truncate text-lg font-semibold tracking-tight">録番</span>
        )}
      </div>
      <ul className="flex flex-1 flex-col gap-1 overflow-y-auto px-2 pb-2">
        {navItems.map(({ to, label, icon: Icon }) => {
          const active = isActive(pathname, to)
          return (
            <li key={to}>
              <Link
                to={to}
                aria-current={active ? 'page' : undefined}
                title={collapsed ? label : undefined}
                className={cn(
                  'flex items-center gap-2 rounded-lg px-3 py-2 text-sm transition-colors',
                  collapsed && 'justify-center px-2',
                  active
                    ? 'bg-muted font-medium text-foreground'
                    : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
                )}
              >
                <Icon className="size-4 shrink-0" />
                <span className={cn(collapsed && 'sr-only')}>{label}</span>
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
