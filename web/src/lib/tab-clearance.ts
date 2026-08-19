/**
 * モバイルのボトムタブ（`fixed`）は、`main` の `padding-bottom`
 * （`--bottom-nav-height`。`components/app-shell.tsx`）によって
 * **ドキュメント最下端まで実際にスクロールしたときだけ**コンテンツと重ならない
 * ことが保証される（ネイティブの最大スクロール位置は `scrollHeight` を
 * 超えないようクランプされるため、その位置では常にコンテンツの下端が
 * ちょうど予約領域の直前に揃う）。
 *
 * しかし `/programs` はページ全体スクロール + 仮想化リストで、初回表示時点
 * （まだ 1px もスクロールしていない）でも 1 時間窓ぶんの番組がすでに
 * ビューポートより長いことがある。この場合、たまたま行の境界がタブの上端と
 * ずれた位置に来ると、時刻や「予約」ボタンを含む行がタブの裏に半分だけ
 * 隠れた状態でユーザーの目に入る（issue #303）。
 *
 * ここにあるのは「初回表示でこの重なりが起きているか」を判定する純関数だけ。
 * 実際の DOM 読み取り・補正スクロールは `components/program-list.tsx` 側が行う
 * （jsdom はレイアウトを計算しないため、その部分は単体テストで検証できない。
 * `lib/scroll-preservation.ts` の `captureAnchor` と同じ切り分け）。
 */

/** 行 1 件ぶんの viewport 座標（`getBoundingClientRect()` の一部）。 */
export type RowRect = {
  top: number
  bottom: number
}

/**
 * computeInitialTabClearanceScroll は、初回表示でボトムタブの裏に「上端は
 * 見えているが下端だけ隠れている」行があれば、それを完全に上へ追い出すために
 * 必要な下方向スクロール量（px）を返す。無ければ 0。
 *
 * `tabTopPx` はタブの上端（`window.innerHeight - reservedBottomPx`。
 * `reservedBottomPx` は `main` の実測 `padding-bottom` で、`md` 以上では 0
 * になるため呼び出し側は viewport 幅を分岐しなくてよい）。
 *
 * 判定は `top < tabTopPx && bottom > tabTopPx`（上端はタブより上、下端は
 * タブに食い込んでいる）に限る --- 完全にタブの裏（`top >= tabTopPx`）や
 * 完全に画面外（`top >= innerHeight`）の行は、ユーザーには最初から
 * 「見えていない」だけなので、切れて見える問題を起こさない
 * （`lib/scroll-preservation.ts` の `findAnchorProgramId` が「sticky の裏から
 * 下端だけ覗いている行は見ている行ではない」としているのと対になる判断）。
 *
 * 複数行が該当する場合は最大の重なり量を返す --- ボトムタブより高い行が
 * 万一あっても、最も深く食い込んでいる行を追い出せば残りも連動して追い出される
 * （後続の行は先行する行の直後に続くため）。
 */
export function computeInitialTabClearanceScroll(
  rows: readonly RowRect[],
  tabTopPx: number,
): number {
  let delta = 0
  for (const row of rows) {
    if (row.top < tabTopPx && row.bottom > tabTopPx) {
      delta = Math.max(delta, row.bottom - tabTopPx)
    }
  }
  return delta
}
