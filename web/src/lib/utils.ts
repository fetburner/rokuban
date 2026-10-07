import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/**
 * auxActionClassName は、文字とアイコンだけで押せることを伝える補助導線の共通クラス。
 * 高さと文字サイズは置く場所ごとに cn() で足す（docs/frontend/design.md）。
 */
export const auxActionClassName =
  'inline-flex pointer-coarse:min-h-11 items-center gap-1 rounded px-1 text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring'
