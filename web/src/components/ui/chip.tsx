import { cn } from '@/lib/utils'

/**
 * Chip はトグル可能な小さな選択肢。日付・サービス・表示形式の切り替えに使う。
 *
 * `aria-pressed` を持つトグルボタンであってリンクではない（選択は URL に出ない）。
 * 番組表（M2-9）と検索（M2-11）が並行実装で同じものを各ページに持ってしまったため、
 * マージ後にここへ引き上げた。
 */
export function Chip({
  active,
  onClick,
  disabled,
  children,
}: {
  active: boolean
  onClick: () => void
  disabled?: boolean
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      aria-pressed={active}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        // shrink-0 は隣接するチップを圧縮せず折り返しに回すためのもの。
        // それとは別に max-w-full が要る --- 1 つのチップの内容が親の幅そのものを
        // 超えると（長い局名 + 補助ラベル。issue #306）shrink-0 の flex-basis が
        // 内容の最大幅を要求し、ページ全体が横に伸びる。実ブラウザ 320px での実測は
        // `e2e/chip-overflow.mjs` の①（max-w-full 有り: documentElement の
        // scrollWidth 320 / clientWidth 320、外すと 448 / 320）。
        // break-words は入れていない --- 同じ実測で有無の差が出なかった（和文は
        // 文字間で折り返せるため）。長い ASCII 1 語での挙動は未検証。
        'max-w-full shrink-0 rounded-full border px-3 py-1.5 text-xs transition-colors disabled:pointer-events-none disabled:opacity-50',
        active
          ? 'border-primary bg-primary text-primary-foreground'
          // hover:text-foreground は hover:bg-muted と対（合成後コントラスト対策。
          // docs/frontend/design.md「コントラストは毎回測る」）。
          : 'border-border text-muted-foreground hover:bg-muted hover:text-foreground',
      )}
    >
      {children}
    </button>
  )
}
