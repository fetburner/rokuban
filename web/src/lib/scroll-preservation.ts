/**
 * 遡行（前の時間窓の読み込み）の直前に「画面上端に見えている行」を控えるための
 * 純関数だけを置く。
 *
 * ## 経緯: DOM アンカー方式は仮想化と両立しなかった（3 回目の修正で置き換え）
 *
 * 以前はここに「挿入後、同じ programId の行を DOM から再度探して
 * `getBoundingClientRect` で位置を測り直し、差分だけ `window.scrollBy` する」
 * 関数（`locateAnchorTop` / `scrollAdjustmentToRestoreTop` / `shouldStopFollowing`）
 * も置いていた。実機で検証したところ、これは**仮想化と構造的に両立しない**
 * ---
 * 先頭に前の窓（6 時間ぶん、約 79 番組・約 5700px）を差し込んだ瞬間、
 * アンカーだった行はオーバースキャン（8 行 ≒ 576px）の外へ弾き出されて DOM
 * から消える。消えた要素を `document.querySelector` で探しても見つからず
 * `locateAnchorTop` は `null` を返し、`reconcile` は「アンカーが見つからない」
 * 分岐で即座に `stop()` していた。つまり `window.scrollBy` は一度も呼ばれず、
 * 「スクロール位置が変わらないまま可視範囲だけが再計算され、同じ位置に
 * 別の（新しく差し込まれた過去の）番組が来る」という壊れ方をしていた。
 *
 * 修正（`components/program-list.tsx`）は、消える DOM を探す代わりに
 * **控えた programId から仮想化ライブラリ上の新しい添字を求め、
 * `virtualizer.scrollToIndex(newIndex, { align: 'start' })` を呼ぶ**方式に
 * 置き換えた。仮想化ライブラリ自身が座標系（見積もり→実測の遷移も含めて）を
 * 持っているため、DOM が可視範囲外にあっても位置合わせができる。
 *
 * ここに残っているのは、**挿入前**（まだ全ての行が実際に画面へ描かれている
 * 時点）で「どの行が画面上端に見えているか」を読む部分だけ。この部分は
 * 上記の壊れ方とは無関係（挿入前に一度だけ、実際にレイアウトされている DOM を
 * 読むだけなので、仮想化の可視範囲が再計算される前に完了する）。
 */

/** アンカー候補 1 件ぶんの、viewport 座標系での位置。 */
export type AnchorCandidate = {
  programId: number
  /** `getBoundingClientRect().top` */
  top: number
  /** `getBoundingClientRect().bottom` */
  bottom: number
}

/**
 * findAnchorProgramId は、候補（DOM 上の出現順 = リストの表示順）から
 * 「画面上端に見えている最初の行」の programId を返す。
 *
 * 「見えている」は `bottom > 0`（行の下端がまだ viewport 上端より上に隠れ
 * きっていない）で判定する。sticky な PageHeader に隠れて `top` が負に
 * なっていても構わない --- この後の位置合わせは同じ行を挿入後に添字で
 * 引き直すだけなので、隠れているかどうか自体は補正の正しさに影響しない。
 *
 * 候補が空、または全行が viewport より上に隠れきっている場合は null。
 */
export function findAnchorProgramId(candidates: readonly AnchorCandidate[]): number | null {
  return candidates.find((c) => c.bottom > 0)?.programId ?? null
}

/** anchorSelector は行の目印。components/program-list.tsx の `<li>` に立つ。 */
const anchorSelector = '[data-program-id]'

/**
 * captureAnchor は「画面上端に見えている行」の programId を実際の DOM から読む。
 *
 * 副作用そのもの（DOM 読み取り）なので `findAnchorProgramId` とは分離して
 * ある。jsdom はレイアウトを計算しないため `getBoundingClientRect` が常に
 * 0 を返し、この関数の実際の効果は単体テストで検証できない
 * （`web/src/lib/list-virtualization.ts` の `probeDomLayout` と同じ事情）。
 *
 * ここで読むのは挿入**前**の DOM（まだ全行が実際にレイアウトされている
 * 時点）なので、挿入後に同じ要素を探す旧方式（上記コメント参照）とは違い
 * 仮想化の可視範囲の再計算に影響されない。
 */
export function captureAnchor(): number | null {
  const candidates: AnchorCandidate[] = []
  for (const el of document.querySelectorAll<HTMLElement>(anchorSelector)) {
    const programId = Number(el.dataset.programId)
    if (Number.isNaN(programId)) continue
    const rect = el.getBoundingClientRect()
    candidates.push({ programId, top: rect.top, bottom: rect.bottom })
  }
  return findAnchorProgramId(candidates)
}
