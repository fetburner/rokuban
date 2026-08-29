/**
 * URL クエリの検証を openapi.yaml 由来の zod スキーマ（`src/api/zod.ts`）へ
 * 委ねるための薄いアダプタ。
 *
 * **制約を手で書き写さない。** `minimum` / `pattern` / `enum` は openapi.yaml が
 * 権威で、orval が zod スキーマに落とす。ここが受け持つのは、その制約を URL の
 * 都合（文字列で届く / 単一値と配列が混ざる / 壊れていても画面は開く）に
 * 繋ぐことだけ。
 *
 * ## URL 固有の 3 つの都合
 *
 * 1. **値は文字列で届く。** `?limit=50` は `'50'`。数値スキーマに渡す前に
 *    数値化する（`asNumber`）。値域の判定は zod 側（openapi 由来の
 *    `min`/`max`）が担うので、ここでは変換だけを行う。
 * 2. **単一値と配列が混ざる。** `?service=1` は文字列、`?service=1&service=2`
 *    は配列。`toValues` が常に配列へ正規化する。
 * 3. **要素ごとに落とす。** zod の `.array()` は 1 要素でも不正なら配列ごと
 *    落とすが、ここでは**不正な要素だけ**を捨てて残りを活かす
 *    （`?service=400101&service=bad` を「絞り込みなし」にするより
 *    「400101 に絞る」の方が意図に近い）。壊れたリンク（手入力・古い
 *    ブックマーク・共有リンク）を踏んでも画面が開く、という契約。
 *
 * ## 返り値は常に `undefined` を明示する
 *
 * TanStack Router の非 strict モードは `validateSearch` の戻り値を**生の
 * `location.search` の上に重ねる**ので、キーを省略すると検証前の値がそのまま
 * 残って漏れる（issue #194 で実際に踏んだ）。`validArray` / `validValue` は
 * 通らない値に `undefined` を返すので、呼び出し側が**全キーを書いたオブジェクト
 * リテラルを返す限り**その値は確実に上書きされる。キーを書くこと自体は
 * 呼び出し側の責任のままなので、`parseProgramsSearch` /
 * `parseRecordingsSearch` はキーを省略しない。
 */

import type { ZodTypeAny } from 'zod'

/** toValues は URL の生の値（単一値 / 配列 / 欠落）を配列に正規化する。 */
function toValues(raw: unknown): unknown[] {
  if (raw === undefined || raw === null) return []
  return Array.isArray(raw) ? raw : [raw]
}

/**
 * asNumber は URL の値を数値化する。
 *
 * 空文字は `Number('') === 0` で「0」という具体的な値に化けるので先に弾く
 * （`?limit=` を「0」と読むのは欠落であるべき、という omit-on-invalid の意図に
 * 反する）。数値化できない値は `NaN` にして zod 側に落とさせる。
 */
function asNumber(raw: unknown): unknown {
  if (typeof raw === 'number') return raw
  if (typeof raw !== 'string' || raw.trim() === '') return NaN
  return Number(raw)
}

/**
 * asInteger は URL の値を整数として数値化する。整数でなければ `NaN`。
 *
 * **`type: integer` は zod スキーマに現れない。** orval は OpenAPI の
 * `type: integer` を `zod.number()` に落とし、`.int()` を付けない（実測:
 * 生成された `limit` は `zod.number().min(1).max(200)`、`service` は
 * `zod.number().min(1).max(6553565535)`。いずれも `.int()` が無く `1.5` が
 * 通る）。値域は spec が持つので、整数性だけをここで足す。
 *
 * 安全整数の判定も兼ねる。ただし**現在の呼び出し元では単独では効かない** ---
 * `genre`（max 15）も `service`（max 6553565535）も zod 側の上限が
 * `Number.MAX_SAFE_INTEGER` よりはるかに小さいので、丸めが起きる値は
 * どのみち上限で落ちる（issue #345 の入力もそちらで落ちている）。`max` を
 * 持たない数値の軸を足したときに効く保険として残す。整数性の判定
 * （`1.5` を落とす）は zod が `.int()` を出さないぶん、ここが唯一の防壁。
 */
export function asInteger(raw: unknown): unknown {
  const n = asNumber(raw)
  return typeof n === 'number' && Number.isSafeInteger(n) ? n : NaN
}

type Options<T> = {
  /** coerce は safeParse の前に適用する変換（数値パラメータなら `asNumber`）。 */
  coerce?: (raw: unknown) => unknown
  /** sort を渡すと重複を除いて正準化する。順序が揺れると queryKey が変わるため。 */
  sort?: (a: T, b: T) => number
}

/**
 * validArray は要素スキーマを 1 要素ずつ適用し、通ったものだけを返す。
 * 結果が空なら `undefined`（「すべて」を空配列という意味を持たない値で
 * 表現しない。不変条件 10 の精神）。
 */
export function validArray<T>(item: ZodTypeAny, raw: unknown, opts: Options<T> = {}): T[] | undefined {
  const values: T[] = []
  for (const value of toValues(raw)) {
    const parsed = item.safeParse(opts.coerce ? opts.coerce(value) : value)
    if (parsed.success) values.push(parsed.data as T)
  }
  const unique = [...new Set(values)]
  if (opts.sort) unique.sort(opts.sort)
  return unique.length > 0 ? unique : undefined
}

/** validValue は単一値にスキーマを適用する。通らなければ `undefined`。 */
export function validValue<T>(item: ZodTypeAny, raw: unknown, opts: Options<T> = {}): T | undefined {
  const first = Array.isArray(raw) ? raw[0] : raw
  const parsed = item.safeParse(opts.coerce ? opts.coerce(first) : first)
  return parsed.success ? (parsed.data as T) : undefined
}

/** ascending は数値の昇順（`validArray` の `sort` に渡す既定）。 */
export const ascending = (a: number, b: number): number => a - b

/**
 * parseEnum は openapi.yaml 由来の zod スキーマを持たない画面ローカルの列挙
 * （`recording-search.ts` の `order`、`programs-search.ts` の `view` 等）を
 * 検証する。許可された文字列のいずれかでなければ `undefined`。
 */
export function parseEnum<T extends string>(raw: unknown, allowed: readonly T[]): T | undefined {
  return typeof raw === 'string' && (allowed as readonly string[]).includes(raw) ? (raw as T) : undefined
}
