import type { ComponentProps, ReactNode } from 'react'

import { cn } from '@/lib/utils'

/**
 * Field は 1 つの入力に見出しを付ける。
 *
 * `<label>` で包むので、`htmlFor` と `id` の対を作らなくてもアクセシブルな名前が
 * 付く（Field には入力を 1 つだけ入れる。2 つ入れると名前がどちらに付くか曖昧）。
 */
export function Field({
  label,
  className,
  children,
}: {
  label: string
  className?: string
  children: ReactNode
}) {
  return (
    <label className={cn('flex flex-col gap-1 text-xs text-muted-foreground', className)}>
      {label}
      {children}
    </label>
  )
}

/**
 * Input はテキスト・数値・時刻の素の入力。
 *
 * 見た目は `ui/button.tsx` の outline と同じ語彙（border-border / bg-background /
 * focus-visible の ring）に揃える。新しいデザイン言語を持ち込まない。
 */
export function Input({ className, ...props }: ComponentProps<'input'>) {
  return (
    <input
      className={cn(
        'h-8 w-full rounded-lg border border-border bg-background px-2.5 text-sm text-foreground outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50 dark:bg-input/30',
        className,
      )}
      {...props}
    />
  )
}

/**
 * Select は素の `<select>`。
 *
 * shadcn/ui の Select（listbox のポップアップ）は使わない。選択肢が数個の
 * 固定列挙なので、OS のピッカーに任せる方がモバイルで扱いやすい。
 */
export function Select({ className, ...props }: ComponentProps<'select'>) {
  return (
    <select
      className={cn(
        'h-8 w-full rounded-lg border border-border bg-background px-2 text-sm text-foreground outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50 dark:bg-input/30',
        className,
      )}
      {...props}
    />
  )
}
