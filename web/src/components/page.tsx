import { useLayoutEffect, useRef } from 'react'

import { cn } from '@/lib/utils'

/**
 * PageHeader はページ上部の見出し。モバイルではスクロールに追従させる。
 *
 * 自身の高さを `--page-header-height` に書き出す。リスト内の sticky な小見出し
 * （番組リストの日付ヘッダ等）はこれを `top` に使う。フィルタ行の有無・行数や
 * フォント・文字サイズでヘッダ高さは変わるので、実測しないとずれる。
 */
export function PageHeader({
  title,
  actions,
  children,
}: {
  title: string
  /** タイトル行の右端に置くコントロール。 */
  actions?: React.ReactNode
  children?: React.ReactNode
}) {
  const ref = useRef<HTMLElement>(null)

  useLayoutEffect(() => {
    const header = ref.current
    const parent = header?.parentElement
    if (!header || !parent) return

    // CSS 変数は子孫にしか継承されないので、ヘッダ自身ではなく親に書く。
    // 日付ヘッダはヘッダの兄弟なので、共通の親を経由しないと値が届かない。
    const publish = () => {
      parent.style.setProperty('--page-header-height', `${header.offsetHeight}px`)
    }
    publish()

    const observer = new ResizeObserver(publish)
    observer.observe(header)
    return () => {
      observer.disconnect()
      parent.style.removeProperty('--page-header-height')
    }
  }, [])

  return (
    <header
      ref={ref}
      // top はサーキットブレーカーの通知バナー（components/circuit-breaker-banner.tsx）
      // の高さぶんずらす。バナーは全ページ共通の居座り表示で、これも sticky top-0
      // なので、ずらさないと両者が同じ位置で重なる（バナー未発動時は 0px）。
      className="sticky z-10 border-b border-border bg-background/95 backdrop-blur"
      style={{ top: 'var(--breaker-banner-height, 0px)' }}
    >
      <div className="flex items-center gap-3 px-4 py-3">
        <h1 className="shrink-0 text-base font-semibold tracking-tight">{title}</h1>
        {actions && <div className="ml-auto flex min-w-0 items-center gap-2">{actions}</div>}
      </div>
      {children}
    </header>
  )
}

/**
 * EmptyState はデータが 0 件のときの表示。
 *
 * `<div>` にする（`<p>` にしない）。issue #137 で「条件をクリア」ボタンのような
 * ブロック要素を子に持つ呼び出し側が出てきたため --- `<p>` の中に `<div>` /
 * 別の `<p>` を置くと無効な HTML になり、React が hydration エラーの警告を出す。
 */
export function EmptyState({ children }: { children: React.ReactNode }) {
  return (
    <div className="px-4 py-12 text-center text-sm text-muted-foreground">{children}</div>
  )
}

/** ErrorState は取得に失敗したときの表示。 */
export function ErrorState({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-4 py-12 text-center text-sm text-destructive">{children}</p>
  )
}

/** Skeleton は読み込み中のプレースホルダ。 */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('animate-pulse rounded bg-muted', className)} />
}

/** ListSkeleton は一覧の読み込み中プレースホルダ。 */
export function ListSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-3 px-4 py-4">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-14" />
      ))}
    </div>
  )
}
