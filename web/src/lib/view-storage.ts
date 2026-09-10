/**
 * 番組表の表示形式を端末ごとの好みとして localStorage に保存する。
 *
 * URL の `view` は共有リンクが指定した表示形式なのでページ側で優先し、ここは
 * URL が指定されていないときの初期値にだけ使う。localStorage の値は利用者が
 * 書き換えられるため、未知の値は既定値へ戻れるよう読み捨てる。
 */

const KEY = 'rokuban:programs:view'

export type PreferredView = 'list' | 'grid'

function isPreferredView(value: unknown): value is PreferredView {
  return value === 'list' || value === 'grid'
}

/** loadPreferredView は保存済みの表示形式を返す。無い・壊れているなら undefined。 */
export function loadPreferredView(): PreferredView | undefined {
  try {
    const value = localStorage.getItem(KEY)
    return isPreferredView(value) ? value : undefined
  } catch {
    // private mode などで localStorage 自体が使えない場合は既定値へ戻る
    return undefined
  }
}

/** savePreferredView は表示形式を保存する。不正な値は保存しない。 */
export function savePreferredView(view: PreferredView): void {
  try {
    if (!isPreferredView(view)) return
    localStorage.setItem(KEY, view)
  } catch {
    // ignore
  }
}
