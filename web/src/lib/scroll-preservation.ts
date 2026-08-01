/**
 * 遡行（前の時間窓の読み込み）の直前に「画面上端に見えている行」を控えるための
 * 純関数だけを置く。
 *
 * ## 経緯 2: 「画面上端」は sticky 要素の裏では成立しない（4 回目の修正で追記）
 *
 * 画面上部には sticky な `PageHeader` と、sticky にした「前を読み込む」ボタンが
 * 居座っており、その下端（`--page-header-height` + `--load-previous-height`。
 * `components/program-list.tsx` 参照）より上はユーザーの目に映らない。
 * 以前の判定（`getBoundingClientRect().bottom > 0`）は viewport 上端（y=0）だけを
 * 基準にしていたため、sticky の裏に隠れている行（`top` が負でも `bottom` が
 * わずかに 0 を超える行）を「見えている」と誤判定していた。実機で確認したところ、
 * 遡行後の復元先（後述 `findAnchorProgramId` の `stickyBottomPx` 引数）もこの
 * 誤判定に引きずられて sticky の裏に着地し、ユーザーが実際に見る先頭行が
 * 別の番組にすり替わっていた。
 *
 * 判定は「sticky の下端より下に最初に現れる行」（`top >= stickyBottomPx`）に
 * 直した。`stickyBottomPx` は実行時に変わる値（フィルタ行の増減やボタンラベルの
 * 折返しで `--page-header-height` / `--load-previous-height` 自体が変わる）なので、
 * `captureAnchor` が呼ばれるたびに CSS 変数から実測する（下記参照）。
 *
 * 併せて、選んだ行の実際の `top`（キャプチャ時点でどれだけ sticky の下端から
 * 離れて見えていたか）も一緒に控えるようにした（`AnchorSnapshot.topPx`）。
 * 復元側（`components/program-list.tsx`）は `virtualizer.scrollToIndex` で
 * おおまかにその行へジャンプした後、この `topPx` との差分だけ追加でスクロール
 * する --- `align: 'start'` は行の上端を常に viewport の y=0 に揃えるだけで、
 * sticky の下端（画面をどこまで覆っているか）にも、キャプチャ時点でユーザーが
 * どれだけスクロールしていたか（sticky の下端ちょうどとは限らない）にも関知
 * しない。`stickyBottomPx` のような固定値へ揃えるのではなく実測した `topPx` を
 * そのまま使うのは、押した瞬間のスクロール量次第で行の見え方が sticky の下端
 * ぴったりとは限らないため（実機で確認: 51px の余白がある状態で押すと、
 * 固定値に揃える方式では許容ズレ（40px）を超える）。
 *
 * ## 経緯 1: DOM アンカー方式は仮想化と両立しなかった（3 回目の修正で置き換え）
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
 * サブピクセルの丸め誤差を許容するための許容量（px）。
 *
 * `stickyBottomPx` は `offsetHeight`（整数 px）由来の CSS 変数から来るが、
 * `top` は `getBoundingClientRect()`（浮動小数）由来なので、行の上端が
 * sticky の下端にちょうど揃っているときに 1px 未満の差で誤って弾かれるのを防ぐ。
 */
const visibilityTolerancePx = 1

/**
 * findAnchorProgramId は、候補（DOM 上の出現順 = リストの表示順）から
 * 「sticky 要素の下端より下に最初に現れる行」の programId を返す。
 *
 * `stickyBottomPx` は画面上部に居座る sticky 要素（`PageHeader` + 「前を
 * 読み込む」ボタン）の合計高さ。省略時は 0（sticky 要素が無い前提）。
 *
 * 判定は `top >= stickyBottomPx`（行の上端が sticky の裏から完全に出ている）
 * で行う。`bottom > stickyBottomPx`（行の下端だけが覗いている）ではない ---
 * sticky の裏から下端だけ覗いている行は、ユーザーには「次の行の一部」としてしか
 * 見えておらず、「見ている先頭行」ではない（実機の `top` 実測で確認済み。
 * 上記コメント「経緯 2」参照）。
 *
 * 候補が空、または全行が sticky の裏に隠れきっている場合は null。
 */
export function findAnchorProgramId(
  candidates: readonly AnchorCandidate[],
  stickyBottomPx = 0,
): number | null {
  return candidates.find((c) => c.top >= stickyBottomPx - visibilityTolerancePx)?.programId ?? null
}

/** anchorSelector は行の目印。components/program-list.tsx の `<li>` に立つ。 */
const anchorSelector = '[data-program-id]'

/**
 * cssPixelVar は、要素に効いている CSS カスタムプロパティを解決済みの px 数値
 * として読む。未設定（空文字列）や単位のない値は 0 として扱う。
 */
function cssPixelVar(el: Element, name: string): number {
  const raw = getComputedStyle(el).getPropertyValue(name).trim()
  const parsed = Number.parseFloat(raw)
  return Number.isFinite(parsed) ? parsed : 0
}

/**
 * measureStickyBottomPx は、画面上部の sticky 要素の合計高さを実測する。
 *
 * `--page-header-height`（`components/page.tsx`）と `--load-previous-height`
 * （`components/program-list.tsx`）はどちらも `<main>`（両者の共通の親）に
 * 書き出されており、子孫にしか継承されない。そのため `document.body` のような
 * 祖先ではなく、リストの行（`<main>` の子孫）を経由して読む必要がある --- 呼び出し
 * 側が実際に見つけた行要素（または `<ul>` 自身）をそのまま渡す。
 *
 * フィルタ行の増減やボタンラベルの折返し（「読み込み中…」）で高さ自体が変わる
 * ため、呼ぶたびに実測する（値をキャッシュしない）。`captureAnchor` からのみ
 * 使う（DOM 読み取りの副作用なので単体テスト対象は純関数側の
 * `findAnchorProgramId` に寄せてある。上記コメント参照）。
 */
function measureStickyBottomPx(el: Element): number {
  return cssPixelVar(el, '--page-header-height') + cssPixelVar(el, '--load-previous-height')
}

/** captureAnchor が返す、控えた行の識別と位置。 */
export type AnchorSnapshot = {
  programId: number
  /** キャプチャ時点の `getBoundingClientRect().top`。復元側が戻す目標位置。 */
  topPx: number
}

/**
 * captureAnchor は「sticky 要素の下端より下に見えている先頭行」の programId と、
 * その時点の `top` を実際の DOM から読む。
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
export function captureAnchor(): AnchorSnapshot | null {
  const candidates: AnchorCandidate[] = []
  let probe: HTMLElement | null = null
  for (const el of document.querySelectorAll<HTMLElement>(anchorSelector)) {
    probe ??= el
    const programId = Number(el.dataset.programId)
    if (Number.isNaN(programId)) continue
    const rect = el.getBoundingClientRect()
    candidates.push({ programId, top: rect.top, bottom: rect.bottom })
  }
  const programId = findAnchorProgramId(candidates, probe ? measureStickyBottomPx(probe) : 0)
  if (programId === null) return null
  const topPx = candidates.find((c) => c.programId === programId)?.top ?? 0
  return { programId, topPx }
}
