import { Link, useRouterState } from '@tanstack/react-router'
import {
  CalendarClock,
  Home,
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
import { useLiveEnabled } from '@/lib/capabilities'
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
  /**
   * この項目を出すためにサーバー側で有効になっている必要がある機能
   * （`GET /api/capabilities`）。省略なら常に出す。
   */
  requires?: 'live'
}

/**
 * 主ナビゲーションの行き先。モバイルのボトムタブとデスクトップのサイドバーで
 * 同じ定義を使う（ルート定義は 1 つで、レイアウトだけが切り替わる）。
 *
 * 並びは実装順ではなく**触る頻度**で決める（docs/frontend/design.md §頻度 3 段）。
 * 単一世帯の運用を前提にした仮説であり、実測ではない:
 *
 * - 一等地: ホーム（毎回の起点。M8-3）・番組（これから録るものを眺める）・
 *   録画（視聴のたび）
 * - 中間: 予約（差分・重複の確認は毎日ではないが週次では触る）・
 *   ライブ（視聴のたびではないが番組ほど反復しない）
 * - 端: 検索（意図して探すときだけ）・ルール（set-and-forget、月数回以下）
 *
 * ホーム（`/`）の新設（M8-3, issue #242）で番組表は `/programs` へ移設した。
 */
const navItems: NavItem[] = [
  { to: '/', label: 'ホーム', icon: Home },
  { to: '/programs', label: '番組', icon: Tv },
  { to: '/recordings', label: '録画', icon: ListVideo },
  { to: '/reservations', label: '予約', icon: CalendarClock },
  { to: '/live', label: 'ライブ', icon: Radio, requires: 'live' },
  { to: '/search', label: '検索', icon: Search },
  { to: '/rules', label: 'ルール', icon: Settings2 },
]

/**
 * モバイルのボトムタブに常時出す項目数。残りは `MoreMenu`（「その他」）に畳む。
 *
 * 一等地の 3 個（ホーム・番組・録画）を常時タブに出し、予約は「その他」側へ
 * 下がる（M8-3）。**予約を常時タブに置いていた理由（「今夜これで録れているか」を
 * 録画と同じ頻度で確認する対象という仮説）は、ホームの新設でホーム自身が
 * その確認そのもの（今夜〜明日の予約セクション）を持つようになったため、
 * 確認の頻度はホームに移った。** この判断も元の仮説と同じく実測ではない
 * （docs/frontend/home.md 参照）。
 */
const MOBILE_PRIMARY_COUNT = 3

/**
 * useNavItems はこのデプロイで出してよいナビ項目を頻度順で返す。
 *
 * `live.enabled: false` のサーバーでは「ライブ」を落とす（issue #209）---
 * ライブのルートが登録されていないので、開いてもプレイリストが 404 になる
 * だけの「機能しない導線」になる。判断はサーバーの能力 API に一本化する
 * （`lib/capabilities.ts`。番組行の「ライブで見る」導線も同じ入口を使う）。
 *
 * **常時項目とその他の切れ目は「除外後の並び」に対して取る。** ただし今の
 * 並びでは差は出ない --- ライブは既に「その他」側（5 番目）にいるので、
 * 除外前に切っても結果は同じである（切り方を入れ替えて `app-shell.test.tsx`
 * を回し、20 件とも通ることを確認済み）。差が出るのは、この形の項目が将来
 * 常時タブ側（先頭 3 個）に来たとき: 除外前に切るとボトムタブが 3 個 +
 * その他に痩せるが、除外後に切れば常時 3 個は保たれる。
 */
function useNavItems(): { items: NavItem[]; primary: NavItem[]; more: NavItem[] } {
  const liveEnabled = useLiveEnabled()
  const items = navItems.filter((item) => item.requires !== 'live' || liveEnabled)
  return {
    items,
    primary: items.slice(0, MOBILE_PRIMARY_COUNT),
    more: items.slice(MOBILE_PRIMARY_COUNT),
  }
}

function useActivePath(): string {
  return useRouterState({ select: (s) => s.location.pathname })
}

function isActive(pathname: string, to: string): boolean {
  return to === '/' ? pathname === '/' : pathname.startsWith(to)
}

/**
 * MoreMenu はボトムタブの「その他」。頻度が低い項目（`useNavItems` の `more`）を
 * ポップオーバーに畳んで、ボトムタブの本数を親指の届く 4 個に抑える。
 *
 * シートではなくポップオーバーを選んだ理由: 中身がリンク 4 個だけの単純な
 * リストで、フォーム操作や長いコンテンツを持たない。全画面/半画面を覆う
 * シートはここでは過剰で、トリガーの直上に浮かせるポップオーバーの方が
 * 「タブの延長」に見える。
 *
 * 固定されたボトムバーの上に浮くオーバーレイなので、画面端でのはみ出し・
 * バーの上に出るか・safe-area との重なりは jsdom では測れない
 * （docs/frontend/shell.md）。実ブラウザでの合否は `e2e/design.mjs` の
 * 「その他」判定が担う。
 *
 * トリガー自身のアクティブ表示は、配下の項目（予約・ライブ・検索・ルール）の
 * いずれかが現在地のときに立てる。個々のリンクは自身のページに居るときだけ
 * `aria-current="page"` を持つのに対し、トリガーはページそのものではなく
 * 「その他」という集合の現在地を表すので `aria-current="true"` を使う
 * （両方 `undefined` にならないよう、両方向をテストで固定する）。
 *
 * 中身は `useNavItems` が決める（`live.enabled: false` なら「ライブ」は
 * 落ちて予約・検索・ルールの 3 個になる。issue #209）。
 */
function MoreMenu({ pathname, items }: { pathname: string; items: NavItem[] }) {
  const [open, setOpen] = useState(false)
  const active = items.some((item) => isActive(pathname, item.to))

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
          {items.map(({ to, label, icon: Icon }) => {
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
 * 常時表示するのは `useNavItems` の `primary`（頻度の一等地）+
 * 「その他」の 4 個まで。決めた理由は誤タップ対策ではなく、頻度の低い
 * 4 項目（予約・ライブ・検索・ルール）を畳んで一等地のタブを広く取ること
 * （実測 @390px: 4 タブ = 97.5px/個。`min-h-14` が確保する最小タップ領域
 * 44px は満たす）。**畳まずに全項目を常時タブへ並べた場合の値は M8-3 で
 * 項目数が 6→7 に増えたため、以前測った「6 タブ = 65px/個」という比較対象
 * 自体が無くなった --- 7 タブでの値は測り直していない。** 「畳む」という
 * 結論は項目数が増えても変わらない（畳まなければ一等地が今より狭くなる
 * 一方なので）が、根拠として持ち出していた数字が古くなっていたぶんは
 * 削った（測っていない数字を書かない）。
 */
function BottomTabs() {
  const pathname = useActivePath()
  const { primary, more } = useNavItems()

  return (
    <nav
      aria-label="主ナビゲーション"
      // `data-testid`: `web/e2e/programs-bottom-nav.mjs` がこのタブの実際の
      // 描画位置（border 込みの上端）を直接測るための目印。`main` の
      // `padding-bottom`（`--bottom-nav-height`）は border を含めて計算して
      // あるので、これと実測が一致するかどうかがそのまま回帰確認になる。
      data-testid="bottom-nav"
      className="fixed inset-x-0 bottom-0 z-20 border-t border-border bg-background/95 pb-[var(--bottom-nav-inset)] backdrop-blur md:hidden"
    >
      <ul className="flex">
        {primary.map(({ to, label, icon: Icon }) => {
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
          <MoreMenu pathname={pathname} items={more} />
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
  const { items } = useNavItems()
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
        {items.map(({ to, label, icon: Icon }) => {
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
