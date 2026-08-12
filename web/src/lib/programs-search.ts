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

/** ProgramsPageSearch は `/`（番組表）の URL クエリパラメータ（検証済み）。 */
export type ProgramsPageSearch = {
  /** 絞り込み中のチャンネル（サービス）。複数可、OR。空集合（＝すべて）は `undefined`。 */
  serviceId?: number[]
}

/** emptyProgramsSearch は絞り込みを何も指定していない状態。 */
export function emptyProgramsSearch(): ProgramsPageSearch {
  return {}
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
