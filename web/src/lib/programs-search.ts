/**
 * 番組表（`/programs`。ホーム新設（M8-3）前は `/` だった）のチャンネル絞り込み
 * （URL の `search`）の型と純関数（issue #231）。
 *
 * 新しい選択は `service=<networkId>:<serviceId>` の文字列配列で運ぶ。`serviceId`
 * 単独と `networkId + serviceId` は既存 URL の後方互換入力として残す。URL 化するのは
 * チャンネル選択と `at` だけで、`dayOffset` 等の表示状態は component state のまま。
 *
 * React に依存しないのはテストのため（`lib/recording-search.ts` と同じ理由）。
 */

import { ServiceChannelType, type Service } from '@/api/generated'
import { parsePositiveIntId } from '@/lib/positive-id'

/** ProgramsPageSearch は `/programs` の URL クエリパラメータ（検証済み）。 */
export type ProgramsPageSearch = {
  /** 後方互換の単一 network 指定。serviceId と組み合わせればその network 内で絞る。 */
  networkId?: number
  /** 厳密なチャンネル組（`<networkId>:<serviceId>`）。複数可、OR。 */
  service?: string[]
  /** 後方互換の serviceId 単独指定。network を問わない。 */
  serviceId?: number[]
  /**
   * ジャンプ先の時刻（epoch ms）。容量不足バッジ（`components/capacity-shortfall-badge.tsx`）
   * が「この時間帯」への導線として付ける（issue #233 M6-5）。グリッド（`lg` 以上）では
   * 初期スクロール位置に使い、それ以外（リスト表示・`lg` 未満）では日付ジャンプの
   * フォールバックにする（`pages/programs.tsx` 参照）。番組の識別子ではなく
   * 「この瞬間を見る」という要求そのものなので、`programId` のような外部キーを
   * 経由せず時刻を直接運ぶ。
   */
  at?: number
}

function toRawValues(raw: unknown): unknown[] {
  if (raw === undefined) return []
  return Array.isArray(raw) ? raw : [raw]
}

const maxInt32Id = 2_147_483_647

function parseInt32Id(raw: unknown): number | undefined {
  const n = parsePositiveIntId(raw)
  return n !== undefined && n <= maxInt32Id ? n : undefined
}

/** programServiceKey は番組表で使う厳密なサービスキーを返す。 */
export function programServiceKey(networkId: number, serviceId: number): string {
  return `${networkId}:${serviceId}`
}

/** parseProgramServiceKey は `<networkId>:<serviceId>` を検証して分解する。 */
export function parseProgramServiceKey(
  value: string,
): { networkId: number; serviceId: number } | undefined {
  const match = /^([1-9][0-9]*):([1-9][0-9]*)$/.exec(value)
  if (match === null) return undefined
  const networkId = parseInt32Id(match[1])
  const serviceId = parseInt32Id(match[2])
  return networkId !== undefined && serviceId !== undefined ? { networkId, serviceId } : undefined
}

function parseProgramServices(raw: unknown): string[] | undefined {
  const refs = new Map<string, { networkId: number; serviceId: number }>()
  for (const value of toRawValues(raw)) {
    if (typeof value !== 'string') continue
    const ref = parseProgramServiceKey(value)
    if (ref === undefined) continue
    refs.set(programServiceKey(ref.networkId, ref.serviceId), ref)
  }
  const sorted = [...refs.values()].sort(
    (a, b) => a.networkId - b.networkId || a.serviceId - b.serviceId,
  )
  return sorted.length > 0 ? sorted.map((ref) => programServiceKey(ref.networkId, ref.serviceId)) : undefined
}

/**
 * parseServiceIds は URL の値を検証済みの serviceId 配列にする。
 *
 * 要素ごとに `lib/positive-id.ts` の `parsePositiveIntId` を適用し、DB / Go の
 * `integer` と同じ int32 上限を重ねる。`parsePositiveIntId` だけでは safe integer
 * まで通すため、上限チェックを省くと Go 側へ渡すとき別の値へ切り詰められる。
 *
 * 不正な要素は配列ごとではなく要素だけ落とす --- 複数チャンネル絞り込みの一部が
 * 壊れたリンク由来でも、残りの有効な絞り込みは活かす（`?serviceId=abc,1024` を
 * 「絞り込みなし」に落とすより「1024 に絞る」の方が意図に近い）。重複を除き
 * 昇順にソートする --- `pages/programs.tsx` の `ChannelPicker` は `Set` で選択を
 * 持つため反復順が選び方の履歴に依存し、URL に手で書く順序も揃わない。順序が
 * 揺れると同じ選択でも queryKey / URL が変わって無限に再取得されるおそれがある
 * ため、パースの時点で正準形にする。結果が空なら `undefined`（「すべて」を
 * 空配列という意味を持たない値で表現しない。不変条件 10 の精神）。
 */
function parseServiceIds(raw: unknown): number[] | undefined {
  const values = toRawValues(raw)
    .map((v) => parseInt32Id(v))
    .filter((n): n is number => n !== undefined)
  const unique = [...new Set(values)].sort((a, b) => a - b)
  return unique.length > 0 ? unique : undefined
}

/**
 * ECMAScript の time value の定義域（[Time Values and Time Range]
 * https://tc39.es/ecma262/#sec-time-values-and-time-range）。±100,000,000 日
 * （ミリ秒換算で ±8,640,000,000,000,000）を超える値は `Date` が扱えず、
 * `new Date(ms)` は Invalid Date（`getTime()` が `NaN`）になる。
 *
 * これは「判定基準が無いから決めない」（不変条件 11）には当たらない ---
 * 基準は言語仕様に既にある。`at` を落とす／落とさないの選択の余地ではなく、
 * この範囲外は `Date` に載せられないという事実。
 */
const MAX_DATE_TIME_VALUE_MS = 8_640_000_000_000_000

/**
 * parseAt は URL の値を検証済みの `at`（epoch ms）にする。
 *
 * 数値に変換できない・有限でない値は落とす。`serviceId` と違い、`at` は
 * 0 以下や過去の時刻は落とさない（番組表側の消費者が「今日より前」を
 * 「今日」にクランプするなど、範囲内の値は自分で扱える）が、
 * **`Date` の time value の定義域外は落とす**。
 *
 * 定義域外を通すと `new Date(at)` が Invalid Date になり、その後 `dayOrigin` /
 * `dayOffsetForMs`（`lib/day-offset.ts`）が `.setHours()` で `NaN` を作り、
 * `Math.min`/`Math.max` が `NaN` を伝播させて `dayOffset` state に `NaN` が
 * 入る。そこから `dayOrigin(NaN)` → `originMs = NaN` → API 呼び出しの
 * `new Date(NaN).toISOString()` が `RangeError: Invalid time value` を投げ、
 * 番組表ページ全体がエラー境界に落ちる（実測: `/programs?at=1e30` 等。ここで
 * 定義域を検証しないと「壊れたリンクを踏んでも画面は開く」という
 * `parseProgramsSearch` 全体の契約が破れる）。
 */
function parseAt(raw: unknown): number | undefined {
  // 空文字は `Number('') === 0` で「0 時ちょうど」という具体的な値に化ける
  // （`Number(undefined)` は NaN になるので他の欠落値と同じ経路には乗らない）。
  // `?at=` のような壊れたリンクを「0 時にジャンプ」と読むのは
  // omit-on-invalid の意図（欠落は「無し」であるべき）に反するので、先に弾く。
  if (typeof raw === 'string' && raw.trim() === '') return undefined
  const n = typeof raw === 'number' ? raw : typeof raw === 'string' ? Number(raw) : NaN
  if (!Number.isFinite(n) || !Number.isInteger(n)) return undefined
  if (Math.abs(n) > MAX_DATE_TIME_VALUE_MS) return undefined
  return n
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
    networkId: parseInt32Id(search.networkId),
    service: parseProgramServices(search.service),
    serviceId: parseServiceIds(search.serviceId),
    at: parseAt(search.at),
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
function placeholderService(networkId: number, serviceId: number): Service {
  return {
    networkId,
    serviceId,
    name: networkId === 0 ? `チャンネル #${serviceId}` : `チャンネル #${networkId}:${serviceId}`,
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
  selected: ReadonlySet<string>,
  serviceByKey: ReadonlyMap<string, Service>,
): Service[] {
  const map = new Map<string, Service>()
  for (const service of filterable) {
    map.set(programServiceKey(service.networkId, service.serviceId), service)
  }
  for (const key of selected) {
    if (map.has(key)) continue
    const parts = key.split(':')
    if (parts.length !== 2) continue
    const networkId = parts[0] === '0' ? 0 : parseInt32Id(parts[0])
    const serviceId = parseInt32Id(parts[1])
    if (networkId === undefined || serviceId === undefined) continue
    map.set(key, serviceByKey.get(key) ?? placeholderService(networkId, serviceId))
  }
  return [...map.values()]
}
