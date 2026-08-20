/**
 * サービス名の表示補助（検索・ルールのサービスチップ用）。
 *
 * `epg-grid.ts` は「時刻 → px の写像」が本体の番組表グリッドの座標系モジュール
 * なので、グリッドの描画に関係しないこのヘルパはそちらに置かない
 * （唯一の利用者は `condition-fields.tsx` のサービスチップ）。
 */

import type { Service } from '@/api/generated'
import { channelTypeLabel } from '@/lib/epg-grid'

/**
 * serviceKey はサービスチップの identity（`condition-fields.tsx` の `key` と同じ組）。
 * `Service` の値は再取得のたびに別オブジェクトになるので、引き当てはこの組で行う。
 */
const serviceKey = (s: Service) => `${s.networkId}-${s.serviceId}`

/**
 * disambiguationParts はサービスを区別する候補の材料。上から順に足していき、
 * 名前が重複するグループ内で一意になったところで止める。programId を分解して
 * 作れる値（networkId・serviceId を逆算するもの)ではなく、API が `Service`
 * として既に返している値だけを使う（issue #306）。ワンセグ / サブサービスは
 * 主サービスと同じリモコン番号・物理チャンネルで並ぶことがあり、その場合だけ
 * 最後の `networkId` + `serviceId` まで進んで区別する。最後の材料はチップの
 * identity と同じ組なので、名前が重複するグループ内では必ず一意になる。
 */
const disambiguationParts: ((s: Service) => string)[] = [
  (s) =>
    // リモコン番号は地上波の資源同定（channel-picker.tsx のバッジと同じ判定）。
    // BS/CS には意味を持たない番号なので channelType でも絞る
    // （テスト「BS がリモコン番号を持っていても番号を出さない」）。地上波でも
    // 0 は存在しないリモコン番号（mirakc が返さなかったときのゼロ値。
    // `internal/mirakc/types.go` の素の int → `epg_services.remote_control_key_id`
    // は NOT NULL → `internal/api/epg.go` がそのまま 0 を返す）なので、
    // 「地上波 0」と書かずに種別だけを出す
    // （テスト「リモコン番号 0 の地上波は種別だけを出す」）。
    s.channelType === 'GR' && s.remoteControlKeyId > 0
      ? `${channelTypeLabel(s.channelType)} ${s.remoteControlKeyId}`
      : channelTypeLabel(s.channelType),
  (s) => s.channel,
  // チップの identity（`condition-fields.tsx` の key）と同じ組を最後の材料にする。
  // これにより「グループ内で一意になったら止める」が構造的に必ず真になる ---
  // (channelType, remoteControlKeyId, channel, serviceId) が全部一致しても
  // networkId が違うサービス（別ネットワークの同名同 serviceId）を、
  // `#${serviceId}` だけでは区別できない穴が残っていた。
  (s) => `#${serviceKey(s)}`,
]

/**
 * serviceDisambiguator は名前が重複するサービスに補助ラベルを与える。
 *
 * 検索・ルールのサービスチップは名前だけを表示していたため、ワンセグ /
 * サブサービスが同じ名前で複数並ぶとどれを選んでいるか分からなかった
 * （issue #306）。名前が重複していないサービスには何も返さない ---
 * 区別が要らない大多数のチップに常時ラベルを付けて読みにくくしないため。
 *
 * 引き当ては `serviceKey`（= チップの identity）なので、渡した配列と同じ
 * オブジェクトで呼ぶ必要はない（テスト「渡した配列とは別オブジェクトでも
 * 同じ (networkId, serviceId) なら引ける」）。
 *
 * `hasPrograms: false` のサブサービスは番組に一度もマッチしないルールを
 * 組める（`openapi.yaml` の `Service.hasPrograms` の意図どおり、絞り込まず
 * 一覧に残す）。同名グループのうち番組を持たない側だけ「番組なし」を足すのは、
 * これがなければ「一意になったがどちらが主サービスか分からない」状態のまま
 * 50% の確率で踏むため（issue #306 の実害）。ただし `hasPrograms` はマルチ
 * 編成が始まれば反転する状態で identity ではないので、`disambiguationParts`
 * には入れない --- 一意性の判定（`new Set(labels).size === group.length`）は
 * 識別子だけで完結させ、この段は判定後に**表示のヒントとしてだけ**上乗せする。
 */
export function serviceDisambiguator(
  services: readonly Service[],
): (service: Service) => string | undefined {
  const groups = new Map<string, Service[]>()
  for (const s of services) {
    const group = groups.get(s.name)
    if (group) group.push(s)
    else groups.set(s.name, [s])
  }

  const labelOf = new Map<string, string>()
  for (const group of groups.values()) {
    if (group.length <= 1) continue
    // 段を配列に積み、空の段だけを落として連結する。連結の判定を
    // 「ここまでのラベルが空文字か」で代理すると、ある段が空文字を返したときに
    // 区切りだけが残る（`地上波 5 ・  ・ #32736-1024`）。今の API 契約では
    // `channel` も `channelType` も required なので空にはならないが、
    // 段の有無で判定しておけば形が崩れない
    // （テスト「材料が空文字の段は区切りごと飛ばす」）。
    const parts = group.map<string[]>(() => [])
    let labels = group.map(() => '')
    for (const part of disambiguationParts) {
      group.forEach((s, i) => parts[i].push(part(s)))
      labels = parts.map((ps) => ps.filter((p) => p !== '').join(' ・ '))
      if (new Set(labels).size === group.length) break
    }
    // 識別子で一意になった後に「番組なし」を上乗せする。段の有無判定
    // （空文字を飛ばす）を識別子側と揃えるので、ここでも空文字は起きない
    // （group.length > 1 なので識別子の段は必ず 1 つ以上通る）。
    group.forEach((s, i) => {
      labelOf.set(serviceKey(s), s.hasPrograms ? labels[i] : `${labels[i]} ・ 番組なし`)
    })
  }

  return (service) => labelOf.get(serviceKey(service))
}
