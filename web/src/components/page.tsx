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
      // top は居座り通知バナーの合計高さぶんずらす（接続断 + サーキットブレーカー。
      // app-shell.tsx の StickyBanners が publish する）。バナーは全ページ共通の
      // 居座り表示で、これも sticky top-0 なので、ずらさないと両者が同じ位置で
      // 重なる（バナー未発動時は 0px）。
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
 *
 * **走査線は 3 箇所限定の使用箇所の 1 つ**（空状態。docs/frontend/design.md
 * 「走査線は 3 箇所限定」）。「まだ何も映っていないブラウン管」の質感を出す。
 * 文字色は `text-foreground` を使う --- `scanlines` の間隙は `text-muted-foreground`
 * （= `--scanline`）と近く、その組み合わせだと 2 値に近い衝突を起こす
 * （`index.css` の `.scanlines` コメント参照）。
 */
export function EmptyState({ children }: { children: React.ReactNode }) {
  return (
    <div className="scanlines px-4 py-12 text-center text-sm text-foreground">{children}</div>
  )
}

/** ErrorState は取得に失敗したときの表示。 */
export function ErrorState({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-4 py-12 text-center text-sm text-destructive">{children}</p>
  )
}

/**
 * Skeleton は読み込み中のプレースホルダ。
 *
 * **走査線は 3 箇所限定の使用箇所の 1 つ**（読み込み中。docs/frontend/design.md
 * 「走査線は 3 箇所限定」）。地の塗り（旧 `bg-muted`）を `scanlines` に差し替えて
 * あるが、形は変えていない（呼び出し側は高さ・角丸を `className` で指定する）。
 */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('scanlines animate-pulse rounded', className)} />
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
