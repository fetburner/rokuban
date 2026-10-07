import { dayOrigin } from '@/lib/day-offset'
import { cn } from '@/lib/utils'

/** 曜日の日本語 1 文字。`Date.getDay()` のインデックス(0=日)に対応する。 */
const weekdayChars = ['日', '月', '火', '水', '木', '金', '土']

/**
 * DayStrip は「いま見ている日」の表示 + ジャンプ先の指定。
 *
 * 2 つの概念を分けている: `current`（ハイライト。スクロール位置から導出した
 * 「いま見ている日」）と `onSelect` で伝える「ジャンプ先」（タップした日）。
 * ジャンプ先を別途表示しない —— ハイライトは常に「いま見ている日」だけを示す
 * （タップ直後は一致するが、その後リストをスクロールすればハイライトだけが動く）。
 *
 * 選択肢は `days` 日ぶんで有界なので、横スクロールなしの等幅グリッドに並べて
 * 必ず 1 画面に収める。横スクロールを避けるのは、選択中の値が画面外に出て
 * 読めなくなることと、画面端からの横スワイプが Android のジェスチャーナビの
 * 「戻る」と衝突すること（`docs/frontend.md`）の 2 つの実害があるため。
 *
 * 各セルのフォーカスは `Button` と同じ明示リングを使い、`outline-none` で
 * ブラウザ既定の outline と二重にならないようにする。
 */
export function DayStrip({
  current,
  days,
  onSelect,
  now,
  matchCounts,
}: {
  /** いま見ている日の offset（ハイライト対象）。スクロール位置から導出する。 */
  current: number
  /** 出す日数。 */
  days: number
  onSelect: (dayOffset: number) => void
  /** テストから現在時刻を固定するための注入口。省略時は内部で `Date.now()`。 */
  now?: number
  /** 条件検索が成功したときの、開始日のローカル暦日ごとの一致件数。 */
  matchCounts?: readonly number[]
}): React.ReactElement {
  const offsets = Array.from({ length: days }, (_, i) => i)

  return (
    <div
      role="group"
      aria-label="日付"
      className="grid gap-1 px-4 pb-2 pointer-coarse:gap-0 pointer-coarse:px-0"
      style={{ gridTemplateColumns: `repeat(${days}, minmax(0, 1fr))` }}
    >
      {offsets.map((offset) => (
        <DayCell
          key={offset}
          dayOffset={offset}
          isCurrent={current === offset}
          onSelect={onSelect}
          now={now}
          matchCount={matchCounts?.[offset]}
        />
      ))}
    </div>
  )
}

function DayCell({
  dayOffset,
  isCurrent,
  onSelect,
  now,
  matchCount,
}: {
  dayOffset: number
  isCurrent: boolean
  onSelect: (dayOffset: number) => void
  now?: number
  matchCount?: number
}) {
  const date = dayOrigin(dayOffset, now)
  const weekday = date.getDay()
  const dateLabel = `${date.getMonth() + 1}月${date.getDate()}日(${weekdayChars[weekday]})`
  // 0 = 日、6 = 土。
  const isWeekend = weekday % 6 === 0

  return (
    <button
      type="button"
      // スクロールで変わる「いま見ている日」は押下状態ではないので aria-pressed
      // ではなく aria-current="date" を使う。ハイライト中のセルにだけ付ける。
      aria-current={isCurrent ? 'date' : undefined}
      // 数値だけだと読み上げが「1」になる。完全な形を aria-label に持たせ、
      // 見える側（2 行）は aria-hidden にして二重読みを避ける
      // （components/capacity-shortfall-badge.tsx と同じ手法）。
      aria-label={
        matchCount === undefined ? dateLabel : `${dateLabel}, 条件に一致 ${matchCount}件`
      }
      onClick={() => onSelect(dayOffset)}
      // 週末は色ではなく濃さで立てる（墨 = 週末 / 走査線グレー = 平日）。
      // カレンダー慣習の「日 = 赤 / 土 = 青」は使わない --- 赤は「いま電波に
      // 乗っている」専用（タリー）で、曜日に赤を置くと画面の中で赤が 2 つの意味を
      // 持つ（docs/frontend/design.md「色は信号のみ」）。土と日のどちらかは
      // 文字自身が言うので、色で区別する必要はない
      className={cn(
        'flex h-11 min-w-0 pointer-coarse:min-w-11 flex-col items-center justify-center rounded-md border text-xs transition-[color,background-color] outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring',
        isCurrent
          ? 'border-primary bg-primary text-primary-foreground'
          : isWeekend
            ? 'border-border font-medium text-foreground hover:bg-muted'
            // hover:text-foreground は hover:bg-muted と対（合成後コントラスト対策。
            // docs/frontend/design.md「コントラストは毎回測る」）。
            : 'border-border text-muted-foreground hover:bg-muted hover:text-foreground',
      )}
    >
      <span aria-hidden="true" className="flex flex-col items-center leading-tight">
        <span>{date.getDate()}</span>
        <span>{weekdayChars[weekday]}</span>
        {matchCount !== undefined && (
          <span data-testid="day-match-count" className="text-[9px] leading-none">
            {matchCount}件
          </span>
        )}
      </span>
    </button>
  )
}
