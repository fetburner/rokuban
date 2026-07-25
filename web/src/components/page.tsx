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
  children,
}: {
  title: string
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
      className="sticky top-0 z-10 border-b border-border bg-background/95 backdrop-blur"
    >
      <div className="flex items-center gap-3 px-4 py-3">
        <h1 className="text-base font-semibold tracking-tight">{title}</h1>
      </div>
      {children}
    </header>
  )
}

/** EmptyState はデータが 0 件のときの表示。 */
export function EmptyState({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-4 py-12 text-center text-sm text-muted-foreground">{children}</p>
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
