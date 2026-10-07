import { ChevronDown, ChevronRight } from 'lucide-react'

/**
 * PickerTriggerContent はトリガーボタンの中身。見える側は値だけなので、
 * 何のコントロールかは読み上げ側（sr-only）に置く。
 */
export function PickerTriggerContent({
  label,
  value,
  visibleValue = value,
}: {
  label: string
  value: string
  /** 見える側だけ値と変えたいとき（例: 未選択を「すべてのチャンネル」と出す）。 */
  visibleValue?: string
}) {
  return (
    <>
      <span className="sr-only">
        {label}: {value}
      </span>
      <span aria-hidden="true" className="min-w-0 truncate">
        {visibleValue}
      </span>
      <ChevronDown className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
    </>
  )
}

/** PickerFilterRow は md 未満の絞り込みシートで、選択画面へ進む行。 */
export function PickerFilterRow({
  label,
  value,
  onOpen,
}: {
  label: string
  value: string
  onOpen?: () => void
}) {
  return (
    <button
      type="button"
      aria-label={`${label}: ${value}`}
      onClick={onOpen}
      className="flex min-h-11 w-full items-center justify-between gap-3 rounded-lg px-2 text-sm text-foreground transition-colors hover:bg-muted"
    >
      <span>{label}</span>
      <span className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
        <span className="truncate">{value}</span>
        <ChevronRight className="size-4 shrink-0" aria-hidden="true" />
      </span>
    </button>
  )
}
