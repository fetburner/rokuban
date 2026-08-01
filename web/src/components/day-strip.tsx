import { dayOrigin } from '@/lib/day-offset'
import { cn } from '@/lib/utils'

/** 曜日の日本語 1 文字。`Date.getDay()` のインデックス（0=日）に対応する。 */
const weekdayChars = ['日', '月', '火', '水', '木', '金', '土']

/**
 * DayStrip は番組タブの日付選択。
 *
 * 選択肢は「今」+ `days` 日ぶんで有界（呼び出し側は 8 を渡す）なので、
 * `days + 1` 個を横スクロールなしの等幅グリッドに並べて必ず 1 画面に収める。
 * 横スクロールを避けるのは、単一選択なのに選択中の値が画面外に出て読めなく
 * なることと、画面端からの横スワイプが Android のジェスチャーナビの「戻る」と
 * 衝突すること（`docs/frontend.md`）の 2 つの実害があるため。
 */
export function DayStrip({
  selected,
  days,
  onSelect,
  now,
}: {
  /** 選択中の dayOffset。null は「今」。 */
  selected: number | null
  /** 「今」に続けて出す日数。 */
  days: number
  onSelect: (dayOffset: number | null) => void
  /** テストから現在時刻を固定するための注入口。省略時は内部で `Date.now()`。 */
  now?: number
}): React.ReactElement {
  const offsets = Array.from({ length: days }, (_, i) => i)

  return (
    <div
      role="group"
      aria-label="日付"
      className="grid gap-1 px-4 pb-2"
      style={{ gridTemplateColumns: `repeat(${days + 1}, minmax(0, 1fr))` }}
    >
      <DayCell dayOffset={null} selected={selected === null} onSelect={onSelect} now={now} />
      {offsets.map((offset) => (
        <DayCell
          key={offset}
          dayOffset={offset}
          selected={selected === offset}
          onSelect={onSelect}
          now={now}
        />
      ))}
    </div>
  )
}

function DayCell({
  dayOffset,
  selected,
  onSelect,
  now,
}: {
  dayOffset: number | null
  selected: boolean
  onSelect: (dayOffset: number | null) => void
  now?: number
}) {
  const date = dayOrigin(dayOffset, now)
  const weekday = date.getDay()
  const dateLabel = `${date.getMonth() + 1}月${date.getDate()}日(${weekdayChars[weekday]})`

  return (
    <button
      type="button"
      aria-pressed={selected}
      // 数値だけだと読み上げが「1」になる。完全な形を aria-label に持たせ、
      // 見える側（2 行 or 「今」の 1 行）は aria-hidden にして二重読みを避ける
      // （components/capacity-shortfall-badge.tsx と同じ手法）。
      // 「今」は offset 0（今日 0 時〜24 時）ではなく now からのローリング窓なので、
      // 隣の offset 0 セル（`${dateLabel}`）と区別できる名前にする。
      aria-label={dayOffset === null ? '今' : dateLabel}
      onClick={() => onSelect(dayOffset)}
      className={cn(
        'flex h-11 min-w-0 flex-col items-center justify-center rounded-md border text-xs transition-colors',
        selected
          ? 'border-primary bg-primary text-primary-foreground'
          : 'border-border text-muted-foreground hover:bg-muted',
      )}
    >
      {dayOffset === null ? (
        <span aria-hidden="true">今</span>
      ) : (
        <span aria-hidden="true" className="flex flex-col items-center leading-tight">
          <span className="tabular-nums">{date.getDate()}</span>
          <span
            className={cn(
              !selected && weekday === 6 && 'text-blue-600 dark:text-blue-400',
              !selected && weekday === 0 && 'text-red-600 dark:text-red-400',
            )}
          >
            {weekdayChars[weekday]}
          </span>
        </span>
      )}
    </button>
  )
}
