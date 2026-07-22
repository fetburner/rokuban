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

  const data = await response.json()
  return { data, status: response.status, headers: response.headers } as T
}
