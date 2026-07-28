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

import { ApiError } from '@/api/client'

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

/**
 * apiErrorMessage は失敗した呼び出しからサーバーのメッセージを取り出す。
 *
 * 非 2xx は `customInstance` が `ApiError` として throw するので、
 * 生成された型（`{ status: 400, data: ErrorResponse }`）には現れない。
 * ここが `unwrap` と対になるもう半分で、成功側と同じく「実行時に形を確かめてから
 * 絞り込む」規約に従う。
 *
 * メッセージが無ければ undefined を返す。呼び出し側は汎用の文言に落とす
 * （「失敗しました」だけを出す方が、サーバーが理由を言っているのに黙るより悪い）。
 */
export function apiErrorMessage(error: unknown): string | undefined {
  if (!(error instanceof ApiError)) return undefined
  const body: unknown = error.body
  if (typeof body !== 'object' || body === null) return undefined
  const message: unknown = (body as { error?: unknown }).error
  return typeof message === 'string' && message !== '' ? message : undefined
}
