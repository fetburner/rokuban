/**
 * 番組表（`/`）のチャンネル絞り込み（URL の `search`）の型と純関数（issue #231）。
 *
 * `serviceId` は `/recordings` と同じ形（`number[]`。複数可・OR・空集合は
 * 「すべて」で `undefined`）を使う（`lib/recording-search.ts` の
 * `RecordingsPageSearch.serviceId` に前例）。URL 化するのはこの 1 次元だけ ---
 * `dayOffset` 等の他の状態（ジャンプ先の日・表示形式）は component state のまま
 * 残す（issue #231 の決定。載せるかどうかは別の判断で、今回のスコープではない）。
 *
 * React に依存しないのはテストのため（`lib/recording-search.ts` と同じ理由）。
 */

import { ServiceChannelType, type Service } from '@/api/generated'

/** ProgramsPageSearch は `/`（番組表）の URL クエリパラメータ（検証済み）。 */
export type ProgramsPageSearch = {
  /** 絞り込み中のチャンネル（サービス）。複数可、OR。空集合（＝すべて）は `undefined`。 */
  serviceId?: number[]
}

function toRawValues(raw: unknown): unknown[] {
  if (raw === undefined) return []
  return Array.isArray(raw) ? raw : [raw]
}

/**
 * parseServiceIds は URL の値を検証済みの serviceId 配列にする。
 *
 * 数値に変換できない要素・0 以下の要素は落とす（丸めない。`serviceId` は
 * `services.service_id` の PK で正の整数）。重複を除き昇順にソートする ---
 * `pages/programs.tsx` の `ChannelPicker` は `Set` で選択を持つため反復順が
 * 選び方の履歴に依存し、URL に手で書く順序も揃わない。順序が揺れると同じ選択でも
 * queryKey / URL が変わって無限に再取得されるおそれがあるため、パースの時点で
 * 正準形にする。結果が空なら `undefined`（「すべて」を空配列という意味を持たない
 * 値で表現しない。不変条件 10 の精神）。
 */
function parseServiceIds(raw: unknown): number[] | undefined {
  const values = toRawValues(raw)
    .map((v) => (typeof v === 'number' ? v : typeof v === 'string' ? Number(v) : NaN))
    .filter((n) => Number.isFinite(n) && Number.isInteger(n) && n > 0)
  const unique = [...new Set(values)].sort((a, b) => a - b)
  return unique.length > 0 ? unique : undefined
}

/**
 * parseProgramsSearch は URL の生の値を検証済みの検索条件にする
 * （`routes.tsx` の `validateSearch`）。
 *
 * 不正な値（型が違う・数値化できない・0 以下）は例外にせず落として
 * 「絞り込みなし」にする。壊れたリンク（手入力・古いブックマーク）を踏んでも
 * 画面は開く。
 *
 * **落とした次元も `undefined` を明示的に代入する（キーを省略しない）。**
 * TanStack Router の既定（非 strict）モードは、実際のルートマッチでもビルド
 * ロケーション用の軽量マッチでも、`validateSearch` の戻り値を「生の（未検証の）
 * `location.search` の上に重ねる」形で合成する。戻り値からキーを省略すると、
 * そのキーは上書きされず生の不正な値（`?serviceId=abc` の文字列そのもの等）が
 * 「検証済みのつもり」の結果へそのまま残って漏れる（`lib/recording-search.ts`
 * の `parseRecordingsSearch` / `routes.tsx` の `/live` の `serviceId` と同型。
 * issue #194）。`{ ...x, k: undefined }` はどちらの合成方式で見ても実際に
 * 上書きになるため、これで確実に消える --- ここでは常に `serviceId` キーを持つ
 * オブジェクトリテラルを返すことでそれを満たす。
 */
export function parseProgramsSearch(search: Record<string, unknown>): ProgramsPageSearch {
  return {
    serviceId: parseServiceIds(search.serviceId),
  }
}

/**
 * serviceIdsToSet は URL の `serviceId`（検証済み）を `ChannelPicker` が扱う
 * `Set` にする。未指定（＝すべて）は空集合。
 */
export function serviceIdsToSet(serviceId: number[] | undefined): ReadonlySet<number> {
  return new Set(serviceId ?? [])
}

/**
 * serviceIdsFromSet は `ChannelPicker` の選択（`Set`）を URL / API へ渡す配列にする。
 *
 * 空集合は「すべて」なのでキーごと落とす（`undefined` を返す）。ソートするのは
 * `parseServiceIds` と同じ理由（`Set` の反復順は選び方の履歴に依存する）。
 */
export function serviceIdsFromSet(selected: ReadonlySet<number>): number[] | undefined {
  if (selected.size === 0) return undefined
  return [...selected].sort((a, b) => a - b)
}

/**
 * placeholderService は `serviceById` にも無い serviceId（EPG から消えた局・
 * 実在しない id を含む古いブックマーク・共有リンク）の代わりに `ChannelPicker`
 * へ渡すダミーの `Service`。名前は引けないが「何かで絞られている」ことは
 * 読める必要がある（`lib/recording-search.ts` の `describeRecordingsFilters`
 * が `チャンネル #<id>` で名前不明の serviceId を表す先例と同じ形）。
 *
 * `channelType` は型上いずれかの値を選ぶ必要があるため `GR` に固定する
 * （実体が無い局の分類は元々決めようがない。表示上どのグループに現れるかは
 * 些末なので、これ以上の判定基準は持たない）。`remoteControlKeyId: 0` で
 * `ChannelOption` のリモコン番号バッジ（`channelType === 'GR' &&
 * remoteControlKeyId > 0` のときだけ出す）を抑止する。
 */
function placeholderService(serviceId: number): Service {
  return {
    networkId: 0,
    serviceId,
    name: `チャンネル #${serviceId}`,
    channelType: ServiceChannelType.GR,
    channel: '',
    remoteControlKeyId: 0,
    hasLogoData: false,
    hasPrograms: false,
  }
}

/**
 * pickerServiceDomain は `ChannelPicker` が表示・列挙できる集合を返す
 * （issue #231 のレビュー指摘。must-fix）。
 *
 * **絞り込みが URL 化された時点で、選択（`selected`）は「外から入る値」になる。**
 * この PR 以前は選択の唯一の生成元がピッカー自身だったため
 * `selected ⊆ filterable` が構造的に成り立っていたが、URL 化するとこの前提が
 * 消える（閉世界 → 開世界）。`ChannelPicker` はトリガーのラベルを
 * `filterable.filter(s => selected.has(s.serviceId))` から作るため、
 * `filterable` に無い serviceId で開くと選択が 0 件（「すべて」）に見えてしまい、
 * かつ候補にも出ないので個別に外す手段が無い（「すべて」で全解除するしかない）。
 *
 * 候補（`filterable`）は `Service.hasPrograms` から作る
 * （docs/frontend.md「ピッカーの候補は `Service.hasPrograms` から作る」）が、
 * それは**検索結果から候補を導かない**という決定であって、**URL から来た
 * 選択値を候補に混ぜること**とは矛盾しない --- 混ぜているのは検索結果ではなく
 * 外部入力（URL）である。`serviceById`（EPG プロジェクション全体。
 * `hasPrograms` を問わない）に居れば実名を使い、それにも居なければ
 * `placeholderService` で `チャンネル #<id>` に落とす。
 */
export function pickerServiceDomain(
  filterable: readonly Service[],
  selected: ReadonlySet<number>,
  serviceById: ReadonlyMap<number, Service>,
): Service[] {
  const map = new Map<number, Service>()
  for (const s of filterable) map.set(s.serviceId, s)
  for (const id of selected) {
    if (map.has(id)) continue
    map.set(id, serviceById.get(id) ?? placeholderService(id))
  }
  return [...map.values()]
}
