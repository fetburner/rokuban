/**
 * orval は OpenAPI に書いたエラーレスポンスも含めて `{ status, data }` の判別可能
 * union として型を生成する。一方 `customInstance` は非 2xx で throw するため、
 * フックの `data` に届くのは常に成功バリアントである。
 *
 * この差を埋めるのがこのモジュール。status を実行時に確かめてから成功側のデータを
 * 返すことで、型と実行時の事実を一致させる。
 *
 * エラーレスポンスを OpenAPI から消して型を単純化する選択肢は採らない。
 * エラーの形は契約の一部であり、生成物の差分で破壊的変更を検知したい（issue #5）。
 */

type ApiResponse = { status: number; data: unknown }

/** SuccessOf は orval のレスポンス union から 2xx のバリアントを取り出す。 */
type SuccessOf<T extends ApiResponse> = Extract<T, { status: 200 | 201 | 204 }>

/**
 * unwrap は成功レスポンスのデータを返す。まだ取得できていない場合は undefined。
 *
 * status を実行時に検査してから絞り込んでいるので、キャストは安全。
 */
export function unwrap<T extends ApiResponse>(
  response: T | undefined,
): SuccessOf<T>['data'] | undefined {
  if (response === undefined || response.status < 200 || response.status >= 300) {
    return undefined
  }
  return response.data as SuccessOf<T>['data']
}
