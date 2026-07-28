// orval が生成するレスポンス型は { data, status, headers } を持つため、
// fetch レスポンスをその形に整形して返す
export const customInstance = async <T>(
  url: string,
  options?: RequestInit,
): Promise<T> => {
  const response = await fetch(url, options)

  if (!response.ok) {
    throw new Error(`${response.status} ${response.statusText}`)
  }

  // 204 (No Content) はボディを持たない。DELETE や resume 系のエンドポイントが
  // これを返すため、空ボディに response.json() を呼んで例外にしない
  // （呼ぶと SyntaxError で reject され、成功しているのに onError に落ちる）。
  const data = response.status === 204 ? undefined : await response.json()
  return { data, status: response.status, headers: response.headers } as T
}
