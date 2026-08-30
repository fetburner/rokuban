import { apiErrorMessage } from '@/api/unwrap'

/**
 * mutationErrorMessage は mutation の `onError` 向けトースト文言を組み立てる。
 *
 * サーバー本文（`apiErrorMessage`）があれば「<generic>: <本文>」を返し、
 * 無ければ（ネットワーク断など）`generic` だけを返す --- 末尾に空の
 * 「: 」を残さない。
 *
 * サーバーの本文は英語のことがある（Go 側の `fmt.Errorf` 由来）が翻訳しない。
 * 運用者向けなので理由が追えることを日本語化より優先する（issue #457）。
 */
export function mutationErrorMessage(generic: string, err: unknown): string {
  const body = apiErrorMessage(err)
  return body ? `${generic}: ${body}` : generic
}
