/**
 * サービスの identity。
 *
 * `Service.id`（`networkId * 100000 + serviceId`。Mirakurun / mirakc と同じ
 * 合成規則）が唯一の identity で、選択・絞り込み・キャッシュキー・URL の
 * `?service=` はすべてこの値を使う。
 *
 * **SI の `serviceId` 単独は network をまたぐと一意でない**（BS 101 と
 * 110度CS 101 は実在する衝突）。かつては画面ごとに `${networkId}:${serviceId}` /
 * `${networkId}-${serviceId}` / `${site}:${networkId}:${serviceId}` / 素の
 * `serviceId` と 4 通りの複合キーを組み立てていて、区切り文字すら揃って
 * いなかった。合成 id を API が返すようになったので、この関数 1 本に集約する。
 *
 * `Service` は `id` を持つのでそのまま使えばよい。この関数が要るのは、
 * `id` を持たない値（`ProgramListItem` は networkId / serviceId しか持たない）
 * からサービスを引き当てるときだけ。
 */

/** serviceIdMagicNumber は合成の基数（`internal/mirakc.idMagicNumber` と同じ）。 */
const serviceIdMagicNumber = 100_000

/**
 * composeServiceId は networkId と serviceId から `Service.id` を組み立てる。
 * サーバー側の権威は `internal/mirakc.ServiceID`。
 */
export function composeServiceId(networkId: number, serviceId: number): number {
  return networkId * serviceIdMagicNumber + serviceId
}
