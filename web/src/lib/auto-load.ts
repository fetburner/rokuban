/**
 * リスト末尾の進行方向読み込み（無限スクロール）の判定を、DOM / IntersectionObserver /
 * TanStack Query の状態から切り離して純関数にする。
 *
 * 番兵（sentinel）の可視判定そのものは jsdom で検証できない（IntersectionObserver
 * が無い）ので、ここに切り出した「可視だったら読むべきか」「ボタンを出すべきか」の
 * 判定だけを単体でテストする。
 */

/**
 * shouldAutoLoadNextPage は、番兵が可視になった瞬間に `fetchNextPage` を呼ぶべきかを返す。
 *
 * `autoLoadAvailable` が false（`lib/list-virtualization.ts` の `domLayoutMeasurable()`
 * が false の環境）なら常に false —— 計測できない環境で番兵が常時可視と判定されると
 * 際限なく呼び続けるおそれがあるため、そもそも呼び出し側は IntersectionObserver 自体を
 * 作らない。ここでの判定はその防波堤の 2 段目にすぎない。
 *
 * `autoLoadFailed` が true（直近の自動読み込みが失敗した）なら false —— 失敗後は
 * ボタンでの手動再試行に落とす（呼び出し側が `autoLoadFailed` を立てる）。
 */
export function shouldAutoLoadNextPage(params: {
  isIntersecting: boolean
  autoLoadAvailable: boolean
  autoLoadFailed: boolean
  hasNextPage: boolean
  isFetchingNextPage: boolean
}): boolean {
  return (
    params.isIntersecting &&
    params.autoLoadAvailable &&
    !params.autoLoadFailed &&
    params.hasNextPage &&
    !params.isFetchingNextPage
  )
}

/**
 * shouldShowLoadMoreButton は「さらに読み込む」ボタンを表示するかを返す。
 *
 * 自動読み込みが効いている通常時（`autoLoadAvailable` かつ失敗していない）は
 * 出さない。番兵が発火しない環境（`autoLoadAvailable` が false）と、直近の
 * 自動読み込みが失敗した後（`autoLoadFailed`）はボタンを受け皿として出す。
 */
export function shouldShowLoadMoreButton(params: {
  hasNextPage: boolean
  autoLoadAvailable: boolean
  autoLoadFailed: boolean
}): boolean {
  return params.hasNextPage && (!params.autoLoadAvailable || params.autoLoadFailed)
}
