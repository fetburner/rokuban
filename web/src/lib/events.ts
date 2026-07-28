import { useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

/**
 * SSE のトピックと、それによって無効化するクエリキーの接頭辞の対応。
 *
 * サーバーが配るのは「どのデータが変わったか」のヒントだけで、変更内容は載っていない。
 * 受け取ったら該当クエリを invalidate し、真実は REST から取り直す（レベルトリガー）。
 * プッシュの中身を信じて手元の状態を書き換えることはしない。
 */
const topicQueryKeys: Record<string, string[]> = {
  // 容量超過（チューナー不足）は予約集合からの導出値なので、予約が変わったら
  // 一緒に取り直す（docs/data.md §6.5）。専用のトピックは無い --- 導出値に
  // 独自の通知を持たせると、元データと導出値が別々の鮮度で並ぶことになる
  reservations: ['/api/reservations', '/api/capacity/overages'],
  recordings: ['/api/recordings'],
  epg: ['/api/programs', '/api/services'],
  breakers: ['/api/breakers'],
}

/**
 * useServerEvents は /api/events を購読し、届いたトピックに対応するクエリを
 * invalidate する。取りこぼしと接続断は staleTime 経過後の再取得で自然回復するので、
 * ここでは再接続を EventSource の既定動作に任せる。
 */
export function useServerEvents() {
  const queryClient = useQueryClient()

  useEffect(() => {
    const source = new EventSource('/api/events')

    const listeners = Object.entries(topicQueryKeys).map(([topic, prefixes]) => {
      const handler = () => {
        for (const prefix of prefixes) {
          // orval のクエリキーは [url, params] の形なので、URL 接頭辞で照合する
          queryClient.invalidateQueries({
            predicate: (query) => {
              const first = query.queryKey[0]
              return typeof first === 'string' && first.startsWith(prefix)
            },
          })
        }
      }
      source.addEventListener(topic, handler)
      return { topic, handler }
    })

    return () => {
      for (const { topic, handler } of listeners) {
        source.removeEventListener(topic, handler)
      }
      source.close()
    }
  }, [queryClient])
}
