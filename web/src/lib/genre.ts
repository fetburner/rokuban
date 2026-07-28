/**
 * ARIB のジャンル（大分類 = lv1）のラベルと色。
 *
 * グリッドのセルは高さが放送時間に比例するため短い番組では文字がほとんど読めない。
 * 色は「タイトルを読まずに帯の並びを掴む」ための補助であり、意味の本体は
 * ラベル（aria-label に入れる）側に置く。
 */

/**
 * genreLabels は lv1 のコード → 日本語ラベル。値の権威は放送波（ARIB STD-B10）で、
 * 12・13 は「予備」として実際に運用されている。
 */
const genreLabels: Record<number, string> = {
  0: 'ニュース・報道',
  1: 'スポーツ',
  2: '情報・ワイドショー',
  3: 'ドラマ',
  4: '音楽',
  5: 'バラエティ',
  6: '映画',
  7: 'アニメ・特撮',
  8: 'ドキュメンタリー・教養',
  9: '劇場・公演',
  10: '趣味・教育',
  11: '福祉',
  14: '拡張',
  15: 'その他',
}

/**
 * genreTints は lv1 → セルの背景と左罫線のクラス。
 *
 * Tailwind はソースを文字列として走査するので、クラス名は組み立てずに定数で持つ。
 * 明度は 50 / 950 に寄せてあり、テーマの無彩色な面の上に載る「淡い着色」に留める
 * （新しいデザイン言語を持ち込まない）。
 */
const genreTints: Record<number, string> = {
  0: 'bg-sky-50 border-l-sky-400 dark:bg-sky-950/50 dark:border-l-sky-700',
  1: 'bg-emerald-50 border-l-emerald-400 dark:bg-emerald-950/50 dark:border-l-emerald-700',
  2: 'bg-orange-50 border-l-orange-400 dark:bg-orange-950/50 dark:border-l-orange-700',
  3: 'bg-rose-50 border-l-rose-400 dark:bg-rose-950/50 dark:border-l-rose-700',
  4: 'bg-violet-50 border-l-violet-400 dark:bg-violet-950/50 dark:border-l-violet-700',
  5: 'bg-amber-50 border-l-amber-400 dark:bg-amber-950/50 dark:border-l-amber-700',
  6: 'bg-indigo-50 border-l-indigo-400 dark:bg-indigo-950/50 dark:border-l-indigo-700',
  7: 'bg-pink-50 border-l-pink-400 dark:bg-pink-950/50 dark:border-l-pink-700',
  8: 'bg-cyan-50 border-l-cyan-400 dark:bg-cyan-950/50 dark:border-l-cyan-700',
  9: 'bg-fuchsia-50 border-l-fuchsia-400 dark:bg-fuchsia-950/50 dark:border-l-fuchsia-700',
  10: 'bg-lime-50 border-l-lime-400 dark:bg-lime-950/50 dark:border-l-lime-700',
  11: 'bg-teal-50 border-l-teal-400 dark:bg-teal-950/50 dark:border-l-teal-700',
}

/** 未知・未設定のジャンルは着色しない（無彩色の面に戻す）。 */
const neutralTint = 'bg-card border-l-border'

/**
 * genreLabel は lv1 のラベルを返す。知らないコードは undefined
 * （「その他」に丸めない — 分類の失敗を分類済みに見せない）。
 */
export function genreLabel(lv1: number | undefined): string | undefined {
  return lv1 === undefined ? undefined : genreLabels[lv1]
}

/** genreTint はグリッドのセルに与える背景・左罫線のクラスを返す。 */
export function genreTint(lv1: number | undefined): string {
  if (lv1 === undefined) return neutralTint
  return genreTints[lv1] ?? neutralTint
}
