/**
 * scrollAdjustmentForPrepend は、先頭に行を差し込む前後のページ全体の高さから、
 * 見た目の位置を保つために `scrollTop` へ加算すべき量を返す。
 *
 * 遡行（前の時間窓の読み込み）はリストの先頭に行を挿入するため、何もしないと
 * 挿入した高さぶんだけ画面内の内容が下にずれる（＝スクロール位置が変わったように
 * 見える）。Safari はスクロールアンカリングを実装していないため、ブラウザ任せに
 * できず、挿入前後の `document.documentElement.scrollHeight` の差分を明示的に
 * `scrollTop` に足し戻す必要がある。
 *
 * 計算そのもの（この関数）と、DOM から高さを読んで `scrollBy` を呼ぶ副作用は
 * 分離してある（`lib/list-virtualization.ts` の `isDomLayoutMeasurable` /
 * `probeDomLayout` と同じ方針）。副作用側は jsdom がレイアウトを計算しないため
 * 単体テストで検証できない（実機確認が要る）。
 */
export function scrollAdjustmentForPrepend(heightBeforePx: number, heightAfterPx: number): number {
  return heightAfterPx - heightBeforePx
}
