/**
 * 遡行（前の時間窓の読み込み）でリストの先頭に行を差し込むと、何もしなければ
 * 挿入した高さぶんだけ画面内の内容が下にずれる（＝スクロール位置が変わったように
 * 見える）。Safari はスクロールアンカリングを実装していないため、ブラウザ任せに
 * できない。
 *
 * ## なぜ「高さの差分」ではなくアンカー要素の位置合わせか
 *
 * 以前は挿入前後の `document.documentElement.scrollHeight` の差分をそのまま
 * `scrollBy` に渡していた。しかし挿入直後の時点では、差し込んだ行はまだ実測
 * されておらず見積もり高さ（`estimatedRowHeightPx`, components/program-list.tsx）
 * でしかない。実測は行ごとの `ResizeObserver`（TanStack Virtual の
 * `measureElement`）経由で**その後**届き、そのたびに総高さが変わって再びずれる。
 * 一度きりの高さ差分の補正では、この見積もり→実測の遷移を捉えられない。
 *
 * また高さの差分方式には別の問題もあった: 遡行中に進行方向の自動読み込みが
 * 同時に走ると、下側に追加された高さまで差分に含めてしまい過補正していた
 * （`scrollHeight` はリスト全体の高さで、どちら側の変化かを区別しない）。
 *
 * アンカー要素（画面上端に見えている行）の viewport 上の `top` を直接測って
 * 揃える方式は、比較する量がそのアンカー要素 1 つの位置だけなので、リストの
 * 下側で何が増減しても影響を受けない。
 *
 * ## `getItemKey`（components/program-list.tsx）との関係
 *
 * アンカーは `programId` で DOM から再取得する（`document.querySelector`）。
 * `programId` を仮想化の `getItemKey` にも使っているのは同じ理由 --- 添字は
 * 先頭への挿入でずれるが、`programId` は行の実体と結びついたまま変わらない。
 *
 * ## 見積もり→実測の遷移への追従と、いつやめるか
 *
 * 挿入直後の 1 回の補正だけでは、その後に届く実測でまた位置がずれる。
 * `document.body` を `ResizeObserver` で監視し、リストの高さが変わるたびに
 * アンカーの `top` を測り直して補正し続ける（呼び出し側 `pages/programs.tsx`）。
 * rAF による毎フレームのポーリングではなく `ResizeObserver` を選んだのは、
 * 実測が届くタイミングが不定なため、無条件にフレームごとポーリングし続ける
 * より、実際にレイアウトが変化したときにだけ動く方が無駄がないため。
 *
 * 際限なく追従し続けると、無関係な要因（ユーザーの手動スクロール等）で
 * `document.body` のサイズが変わり続けた場合に止まらなくなる。
 * `shouldStopFollowing` で「補正回数」と「経過時間」の両方に上限を持たせ、
 * どちらかに達したら打ち切る。
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
 * なっていても構わない --- この後の位置合わせは同じ要素の `top` を挿入前後で
 * 比較するだけなので、隠れているかどうか自体は補正の正しさに影響しない。
 *
 * 候補が空、または全行が viewport より上に隠れきっている場合は null。
 */
export function findAnchorProgramId(candidates: readonly AnchorCandidate[]): number | null {
  return candidates.find((c) => c.bottom > 0)?.programId ?? null
}

/**
 * scrollAdjustmentToRestoreTop は、アンカー要素の viewport 上の `top` を
 * 挿入前の値（`topBeforePx`）へ戻すために `window.scrollBy` へ渡すべき量を返す。
 *
 * `window.scrollBy(0, delta)` は正の `delta` で下にスクロールし、その結果
 * 要素の `getBoundingClientRect().top` は `delta` だけ小さくなる。挿入・実測
 * 反映後の現在値が `topAfterPx` なので、`topAfterPx - delta === topBeforePx`
 * を満たす `delta = topAfterPx - topBeforePx` を返す。
 */
export function scrollAdjustmentToRestoreTop(topBeforePx: number, topAfterPx: number): number {
  return topAfterPx - topBeforePx
}

/** followState は追従ループのここまでの経過。 */
export type FollowState = {
  /** これまでに行った補正（`scrollBy`）の回数。 */
  corrections: number
  /** 追従を開始してからの経過時間（ms）。 */
  elapsedMs: number
}

/** followConfig は追従を打ち切る上限。 */
export type FollowConfig = {
  /** これ以上は補正しない回数の上限。 */
  maxCorrections: number
  /** これ以上は経過時間で打ち切る上限（ms）。 */
  maxElapsedMs: number
}

/**
 * shouldStopFollowing は、見積もり→実測の遷移に追従するループを打ち切るかを
 * 判定する。
 *
 * 実測は行ごとの `ResizeObserver` で不定回数届くため、際限なく追従し続ける
 * 形にはしない --- 補正回数（`maxCorrections`）か経過時間（`maxElapsedMs`）の
 * どちらかが上限に達したら打ち切る（どちらか片方だけを見ると、もう片方の
 * 経路で際限なく続くケースを取りこぼす。例えば経過時間だけを見ると、実測が
 * 極端に短い間隔で届き続けたとき回数の上限に意味がなくなる）。
 */
export function shouldStopFollowing(state: FollowState, config: FollowConfig): boolean {
  return state.corrections >= config.maxCorrections || state.elapsedMs >= config.maxElapsedMs
}

/** anchorSelector は行の目印。components/program-list.tsx の `<li>` に立つ。 */
const anchorSelector = '[data-program-id]'

/**
 * captureAnchor は「画面上端に見えている行」の programId と現在の viewport
 * 上の `top` を実際の DOM から読む。
 *
 * 副作用そのもの（DOM 読み取り）なので `findAnchorProgramId` とは分離して
 * ある。jsdom はレイアウトを計算しないため `getBoundingClientRect` が常に
 * 0 を返し、この関数の実際の効果は単体テストで検証できない
 * （`web/src/lib/list-virtualization.ts` の `probeDomLayout` と同じ事情）。
 */
export function captureAnchor(): { programId: number; topPx: number } | null {
  const candidates: AnchorCandidate[] = []
  for (const el of document.querySelectorAll<HTMLElement>(anchorSelector)) {
    const programId = Number(el.dataset.programId)
    if (Number.isNaN(programId)) continue
    const rect = el.getBoundingClientRect()
    candidates.push({ programId, top: rect.top, bottom: rect.bottom })
  }
  const programId = findAnchorProgramId(candidates)
  if (programId === null) return null
  const topPx = candidates.find((c) => c.programId === programId)?.top
  return topPx === undefined ? null : { programId, topPx }
}

/**
 * locateAnchorTop は、指定した programId の行を DOM から再度見つけて
 * viewport 上の `top` を読む。挿入直後や、その後の実測反映のたびに呼び直す。
 *
 * 行が見つからない場合（描画されていない・スクロールで既に外れた等）は null。
 */
export function locateAnchorTop(programId: number): number | null {
  const el = document.querySelector<HTMLElement>(`[data-program-id="${programId}"]`)
  return el ? el.getBoundingClientRect().top : null
}
