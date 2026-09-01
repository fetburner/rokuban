import { useLayoutEffect, useRef } from 'react'

import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

/**
 * PageHeader はページ上部の見出し。モバイルではスクロールに追従させる。
 *
 * 自身の高さを `--page-header-height` に書き出す。リスト内の sticky な小見出し
 * （番組リストの日付ヘッダ等）はこれを `top` に使う。フィルタ行の有無・行数や
 * フォント・文字サイズでヘッダ高さは変わるので、実測しないとずれる。
 *
 * 詳細 2 ページ（録画・予約）もこれに乗る（issue #467）。「戻る」ボタンは
 * `leading` に置く --- `actions` は右端固定なので左端のスロットを別に持つ。
 */
export function PageHeader({
  title,
  leading,
  actions,
  children,
}: {
  title: string
  /** タイトル左に置く「戻る」ボタン等。 */
  leading?: React.ReactNode
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
      style={{ top: 'var(--sticky-banners-height, 0px)' }}
    >
      <div className="flex items-center gap-3 px-4 py-3">
        {leading}
        <h1 className="shrink-0 text-base font-semibold tracking-tight">{title}</h1>
        {actions && <div className="ml-auto flex min-w-0 items-center gap-2">{actions}</div>}
      </div>
      {children}
    </header>
  )
}

/** PageContent は一覧ページの本文幅を制限し、サイドバー側へ左寄せする。 */
export function PageContent({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      data-testid="bounded-page-content"
      className={cn('w-full max-w-5xl', className)}
      {...props}
    />
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

/**
 * ErrorState は取得に失敗したときの表示。
 *
 * `role="alert"`（WCAG 4.1.3）で読み上げに割り込む。`onRetry` を渡すと
 * 再試行ボタンを添える --- 一覧の初回読み込み失敗はこれに寄せる
 * （TNLAStation-frontend の共通 ErrorState 相当、issue #467）。
 *
 * **続き取得の失敗（`pages/recordings.tsx` / `pages/programs.tsx` の
 * 「さらに読み込む」フォールバック）はこれを使わない。** 初回とは違い
 * 「さらに読み込む」ボタン自身が既に再試行の手段であり、二重にボタンを
 * 出すことになる。**ライブのエラー文（`pages/live.tsx` の 2 種
 * ---能力 API の未確定 / チャンネル一覧の取得失敗--- と、
 * `components/live-player.tsx` の `LiveErrorMessage`（`LiveLoadError` を
 * 表示する、hls.js/ネイティブ経路の再生エラー）の計 3 種）も寄せない**
 * --- 経路ごとに原因説明が異なり、単純な再試行では原因の違いが伝わらない。
 */
export function ErrorState({
  children,
  onRetry,
}: {
  children: React.ReactNode
  /** 渡すと再試行ボタンを添える。省略すると文言だけを出す。 */
  onRetry?: () => void
}) {
  return (
    <div role="alert" className="flex flex-col items-center gap-3 px-4 py-12 text-center text-sm text-destructive">
      <p>{children}</p>
      {onRetry && (
        <Button type="button" variant="outline" size="sm" onClick={onRetry}>
          再試行
        </Button>
      )}
    </div>
  )
}

/**
 * Skeleton は読み込み中のプレースホルダ。
 *
 * **走査線は 3 箇所限定の使用箇所の 1 つ**（読み込み中。docs/frontend/design.md
 * 「走査線は 3 箇所限定」）。地の塗り（旧 `bg-muted`）を `scanlines` に差し替えて
 * あるが、形は変えていない（呼び出し側は高さ・角丸を `className` で指定する）。
 *
 * 装飾のみで支援技術には何も伝えない（`aria-hidden`）。読み上げは `ListSkeleton`
 * が 1 リージョンにつき 1 度だけ持つ（各行に重ねると同じ内容を行数ぶん読み上げる）。
 */
export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cn('scanlines animate-pulse rounded', className)} />
}

/**
 * ListSkeleton は一覧の読み込み中プレースホルダ。
 *
 * `role="status"` + sr-only の文言を 1 つだけ持つ（WCAG 4.1.3 ステータス
 * メッセージ）。中身の `Skeleton` は装飾のみなので個々には付けない。
 */
export function ListSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div role="status" className="flex flex-col gap-3 px-4 py-4">
      <span className="sr-only">読み込み中</span>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-14" />
      ))}
    </div>
  )
}
