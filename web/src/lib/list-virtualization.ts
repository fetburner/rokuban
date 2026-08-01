/**
 * 番組リストの仮想化のうち、TanStack Virtual（`useWindowVirtualizer`）に
 * モデルを任せられない 1 点だけをここに切り出す。
 *
 * リストは行の並びなので、座標系そのものを持つグリッド（`epg-grid.ts` /
 * 自前実装。docs/frontend.md「仮想化はライブラリを入れず自前」）と違い、
 * 可視判定はライブラリの模型にそのまま乗る。ここに残る判定は「DOM の
 * レイアウト計算ができない環境を検出して仮想化そのものをバイパスするか」
 * だけである。
 *
 * jsdom はレイアウトエンジンを持たず `getBoundingClientRect` が常に高さ 0 を
 * 返す。TanStack Virtual の動的計測（`measureElement`）はこの 0 をそのまま
 * 行の高さとして採用してしまうため、実際に確認したところ次の壊れ方をした:
 * 可視範囲より手前の行が「測定済み・高さ 0」になるたびに、その分だけ後続の
 * 行が可視範囲に収まると判定され、レンダーのたびに可視範囲（インデックス）が
 * 際限なく前進し続ける（0 件になって終わる「空のリストなのにテストが通る」
 * よりも悪い —— 際限のない再レンダーになる）。
 *
 * 対策は `visibleTimeWindow` / `visibleColumnRange` と同じ「未計測なら間引かない」
 * だが、計測対象がグリッドの viewport ではなく行の実レイアウトなので、判定は
 * 「既知のスタイルを与えた要素を実際にレイアウトさせて高さを読み返せるか」で行う。
 */

/**
 * isDomLayoutMeasurable は、`probedHeightPx`（既知の高さを与えた要素を実際に
 * レイアウトさせて読み返した高さ）から、その環境が DOM のレイアウトを計算
 * できるかを返す。
 *
 * 0 以下（未計測 = jsdom 等レイアウトエンジンを持たない環境）なら false。
 * 呼び出し側はこれが false のとき、`measureElement` による動的計測を使わず
 * 仮想化そのものをバイパスして全行を通常のフローで描く。
 */
export function isDomLayoutMeasurable(probedHeightPx: number): boolean {
  return probedHeightPx > 0
}

/** probeHeightPx は `probeDomLayout` が実際に要素へ与える高さ（px）。値そのものに意味はなく、0 と有意に区別できればよい。 */
const probeHeightPx = 1

/**
 * probeDomLayout は `isDomLayoutMeasurable` に渡す値を実測する。
 *
 * 既知の高さ（`probeHeightPx`）を持つ要素を document に一時的に差し込み、
 * `getBoundingClientRect` で読み返す。DOM に依存する副作用そのものなので
 * 純関数ではなく、判定（`isDomLayoutMeasurable`）と分離してある。
 */
export function probeDomLayout(): number {
  const probe = document.createElement('div')
  probe.style.cssText = `position:absolute;visibility:hidden;pointer-events:none;height:${probeHeightPx}px;width:${probeHeightPx}px;`
  document.body.appendChild(probe)
  try {
    return probe.getBoundingClientRect().height
  } finally {
    document.body.removeChild(probe)
  }
}

/**
 * 実測結果のキャッシュ。DOM がレイアウトを計算できるかは実行環境の性質であり
 * セッション中に変わらないため、コンポーネントのマウントごとに測り直さない。
 */
let cachedLayoutMeasurable: boolean | undefined

/** domLayoutMeasurable は `probeDomLayout` の結果をキャッシュして返す。 */
export function domLayoutMeasurable(): boolean {
  cachedLayoutMeasurable ??= isDomLayoutMeasurable(probeDomLayout())
  return cachedLayoutMeasurable
}
