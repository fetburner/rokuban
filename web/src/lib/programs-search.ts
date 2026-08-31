/**
 * 番組表（`/programs`。ホーム新設（M8-3）前は `/` だった）のチャンネル絞り込み
 * （URL の `search`）の型と純関数（issue #231）。
 *
 * チャンネル選択は `service=<Service.id>` の数値配列で運ぶ。URL 化するのは
 * チャンネル選択・選択日・`at`・`view`（表示形式）。スクロールから導出する
 * 「いま見ている日」等、それ以外の表示状態は component state のまま。
 *
 * React に依存しないのはテストのため（`lib/recording-search.ts` と同じ理由）。
 */

import { ServiceChannelType, type Service } from '@/api/generated'
import { ListProgramsQueryParams } from '@/api/zod'
import { dayOrigin } from '@/lib/day-offset'
import { ascending, asInteger, parseEnum, validArray } from '@/lib/url-search'

/**
 * serviceIdSchema は `?service=` の 1 要素。**openapi.yaml から生成した
 * スキーマをそのまま使う**（`minimum` / `maximum` を手で書き写さない）。
 *
 * `/live` の `?service=`（`routes.tsx` の `LivePageSearch`）も同じ id 空間なので
 * この 1 本を共有する --- 別々に書くと同じパラメータ名で値域が食い違う。
 */
export const serviceIdSchema = ListProgramsQueryParams.shape.service.unwrap().element

/** ProgramsPageSearch は `/programs` の URL クエリパラメータ（検証済み）。 */
export type ProgramsPageSearch = {
  /** 選択日（ローカル暦日の `YYYY-MM-DD`）。既定の今日は `undefined`。 */
  day?: string
  /** `Service.id`。複数可、OR。 */
  service?: number[]
  /**
   * ジャンプ先の時刻（epoch ms）。容量不足バッジ（`components/capacity-shortfall-badge.tsx`）
   * が「この時間帯」への導線として付ける（issue #233 M6-5）。グリッド（`lg` 以上）では
   * 初期スクロール位置に使い、それ以外（リスト表示・`lg` 未満）では日付ジャンプの
   * フォールバックにする（`pages/programs.tsx` 参照）。番組の識別子ではなく
   * 「この瞬間を見る」という要求そのものなので、`programId` のような外部キーを
   * 経由せず時刻を直接運ぶ。
   */
  at?: number
  /**
   * 表示形式（グリッド / リスト）。画面ローカルの状態だが、容量不足バッジが
   * 「グリッドで見せたい」を明示するために URL へ載せる（issue #437）。
   * openapi.yaml 由来の zod スキーマには無い画面ローカルの列挙なので、
   * `recording-search.ts` の `order` と同じ `lib/url-search.ts` の `parseEnum`
   * で検証する。
   */
  view?: 'grid' | 'list'
}

/** programsViewValues は `view` の取りうる値。 */
const programsViewValues = ['grid', 'list'] as const

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

/** programsSelectableDays は `DayStrip` が今日を起点に表示する日数。 */
export const programsSelectableDays = 8

/** dayKeyForOffset は日付 offset をローカル暦日の `YYYY-MM-DD` にする。 */
function dayKeyForOffset(dayOffset: number, now: number): string {
  const date = dayOrigin(dayOffset, now)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

/** programsDayForOffset は URL に書く選択日を返す。既定の今日は省略する。 */
export function programsDayForOffset(dayOffset: number, now: number): string | undefined {
  return dayOffset === 0 ? undefined : dayKeyForOffset(dayOffset, now)
}

/** programsDayOffset は URL の選択日を offset にする。不正値と既定の今日は 0。 */
export function programsDayOffset(raw: unknown, now: number): number {
  if (typeof raw !== 'string') return 0
  for (let offset = 0; offset < programsSelectableDays; offset++) {
    if (dayKeyForOffset(offset, now) === raw) return offset
  }
  return 0
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
 * の `parseRecordingsSearch` / `routes.tsx` の `/live` の `service` と同型。
 * issue #194）。`{ ...x, k: undefined }` はどちらの合成方式で見ても実際に
 * 上書きになるため、これで確実に消える --- ここでは常に全キーを持つ
 * オブジェクトリテラルを返すことでそれを満たす。
 *
 * `day` は `DayStrip` が表示する今日から 8 日間のローカル暦日だけを受け取る。
 * 既定の今日は `undefined` に正準化して URL に書かない。日付操作で履歴を汚さず、
 * 共有 URL を短く保つため。
 */
export function parseProgramsSearch(
  search: Record<string, unknown>,
  now = Date.now(),
): ProgramsPageSearch {
  const dayOffset = programsDayOffset(search.day, now)
  return {
    day: programsDayForOffset(dayOffset, now),
    service: validArray<number>(serviceIdSchema, search.service, {
      coerce: asInteger,
      sort: ascending,
    }),
    at: parseAt(search.at),
    view: parseEnum(search.view, programsViewValues),
  }
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
function placeholderService(id: number): Service {
  return {
    id,
    networkId: Math.floor(id / 100_000),
    serviceId: id % 100_000,
    name: `チャンネル #${id}`,
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
 * `filterable.filter(s => selected.has(s.id))` から作るため、
 * `filterable` に無い id で開くと選択が 0 件（「すべて」）に見えてしまい、
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
  const map = new Map<number, Service>(filterable.map((s) => [s.id, s]))
  for (const id of selected) {
    if (map.has(id)) continue
    map.set(id, serviceById.get(id) ?? placeholderService(id))
  }
  return [...map.values()]
}
