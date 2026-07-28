/**
 * ApiError は非 2xx のレスポンス。ステータスと**ボディ**を保持する。
 *
 * ボディを捨てると、サーバーが 400 に添えた理由（ErrorResponse.error）が
 * UI に届かない。検索の正規表現のように「書き方が悪い」のか「該当なし」なのかを
 * ユーザーが区別できないと直せない種類の失敗があるため、ここで運ぶ。
 * 取り出しは `api/unwrap.ts` の `apiErrorMessage`。
 */
export class ApiError extends Error {
  readonly status: number
  readonly body: unknown

  constructor(status: number, statusText: string, body: unknown) {
    super(`${status} ${statusText}`.trimEnd())
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

// orval が生成するレスポンス型は { data, status, headers } を持つため、
// fetch レスポンスをその形に整形して返す
export const customInstance = async <T>(
  url: string,
  options?: RequestInit,
): Promise<T> => {
  const response = await fetch(url, options)

  if (!response.ok) {
    // JSON でないエラー（プロキシの HTML エラーページ等）もあるので、
    // 解析の失敗はボディ無しとして扱う。ここで例外にすると
    // ステータスすら伝わらなくなる。
    const body = await response.json().catch(() => undefined)
    throw new ApiError(response.status, response.statusText, body)
  }

  // 204 (No Content) はボディを持たない。DELETE や resume 系のエンドポイントが
  // これを返すため、空ボディに response.json() を呼んで例外にしない
  // （呼ぶと SyntaxError で reject され、成功しているのに onError に落ちる）。
  const data = response.status === 204 ? undefined : await response.json()
  return { data, status: response.status, headers: response.headers } as T
}
