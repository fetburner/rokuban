import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'

/**
 * operationalRefreshIntervalMs は運用状態（予約・録画・ブレーカー・容量超過）を
 * SSE 抜きで取り直す周期（ミリ秒）。
 *
 * 応答が小さく変化が速いグループなので短くできる。SSE の通知を全部落としても、
 * 前面タブで開いている画面はこの周期で REST から収束する
 * （テスト: events.test.tsx「SSE が来なくても運用状態のクエリは 60 秒周期で取り直す」）。
 */
export const operationalRefreshIntervalMs = 60_000

/**
 * epgRefreshIntervalMs は EPG（番組リスト・サービス一覧・重なり）を SSE 抜きで
 * 取り直す周期（ミリ秒）。
 *
 * 番組表は数十チャンネル x 24 時間の大きな時間窓を 1 回で取るうえ、EPG 同期
 * ジョブ自体が分オーダーでしか動かない。運用状態と同じ周期で回すと、得るものが
 * 無いまま最大の応答を繰り返し引くことになるので分ける
 * （テスト: events.test.tsx「EPG は運用状態より長い周期でしか取り直さない」）。
 */
export const epgRefreshIntervalMs = 600_000

/**
 * storageRefreshIntervalMs はストレージ残高（`/api/storage`）を取り直す周期
 * （ミリ秒、5 分）。
 *
 * `/api/storage` はディスクの statfs 観測の射影であって、worker の観測ループが
 * 書き換えたときにしか値が変わらない。運用状態（60 秒）と同じ周期で回しても、
 * 同じ値を余分に引くだけで得るものが無い。一方で EPG（10 分）ほど長くする理由も
 * ない（番組表グリッドのような大きな応答ではないので、短くする側のコストが
 * 小さい）。その 2 つの間に挟んで 5 分を選ぶ
 * （テスト: events.test.tsx「SSE が来なくてもストレージ残高は専用の周期で
 * 取り直す」）。
 *
 * worker の観測周期（`internal/worker/storage.go`）から輸入はしない ---
 * `lib/storage-forecast.ts` の `observationStaleAfterMs` と同じ立場で、
 * `GET /api/storage` の契約に入っていない実装詳細に結合すると、worker 側だけが
 * 変わってフロントが追随できなくなる。
 *
 * SSE トピックからの invalidate は持たない。`storage_sync` は行トリガーの対象に
 * していない（observed_at は毎パス全量 upsert されるだけで、行トリガーにすると
 * statfs の頻度そのものに通知量が結合する）ため。収束はこの定期 invalidate に
 * 加えて、再接続時の全グループ invalidate（{@link useServerEvents} 参照。topic が
 * `null` かどうかを見ずに queryGroups 全体を回す）と mount / window focus に依る
 * （テスト: events.test.tsx「再接続したら切断中の変更を全グループ取り直す」。
 * `docs/api/sse.md` 参照）。
 */
export const storageRefreshIntervalMs = 5 * 60_000

/**
 * QueryGroup は SSE トピック（無い場合もある）と、それによって無効化するクエリ
 * キーの接頭辞、そして SSE が届かなかったときに取り直す周期の組。
 */
type QueryGroup = {
  /**
   * SSE のトピック名。`null` は「対応する SSE トピックを持たず、定期 invalidate
   * だけで収束させる」グループ（`storage` がこれ。理由は
   * {@link storageRefreshIntervalMs} 参照）。
   *
   * optional にはしない --- optional だと「トピックを書き忘れた」グループが型でも
   * テストでも通り、SSE の購読を静かに失う。`null` を明示的に書かせることで、
   * 省略は型エラーのまま、意図した「トピック無し」だけが書ける。
   */
  topic: string | null
  /** invalidate 対象のクエリキーの接頭辞（orval のキーは `[url, params]`）。 */
  prefixes: string[]
  /** SSE が 1 通も届かなくても、この周期で invalidate する。 */
  refreshIntervalMs: number
}

/**
 * queryGroups は SSE のトピックと、それによって無効化するクエリキーの接頭辞の対応。
 * トピックを持たないグループ（`storage`）は SSE を購読せず、定期 invalidate
 * （加えて再接続時の全グループ invalidate）だけで収束させる。
 *
 * サーバーが配るのは「どのデータが変わったか」のヒントだけで、変更内容は載っていない。
 * 受け取ったら該当クエリを invalidate し、真実は REST から取り直す（レベルトリガー）。
 * プッシュの中身を信じて手元の状態を書き換えることはしない。
 *
 * ヒントは落ちうる（notifier はクライアントのバッファが埋まっていたら通知を捨てる）
 * ので、各グループは「SSE が 1 通も来なくても取り直す周期」を併せ持つ。
 */
const queryGroups: QueryGroup[] = [
  {
    topic: 'reservations',
    // 容量超過（チューナー不足）は予約集合からの導出値なので、予約が変わったら
    // 一緒に取り直す（docs/data.md §6.5）。専用のトピックは無い --- 導出値に
    // 独自の通知を持たせると、元データと導出値が別々の鮮度で並ぶことになる
    prefixes: ['/api/reservations', '/api/capacity/overages'],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'recordings',
    prefixes: ['/api/recordings'],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'breakers',
    prefixes: ['/api/breakers'],
    refreshIntervalMs: operationalRefreshIntervalMs,
  },
  {
    topic: 'epg',
    // programs / services / overlaps はすべて /api/sites/{site}/... 配下。
    // 番組リスト（pages/programs.tsx の useInfiniteQuery）だけは URL をキーに
    // できず（ページの形が「取得した半開区間」なので）手書きの
    // ['/api/programs', 'infinite', ...] を使う。ここに書かないと、番組リストは
    // SSE の epg イベントでも定期 invalidate でも取り直されない
    prefixes: ['/api/sites/', '/api/programs'],
    refreshIntervalMs: epgRefreshIntervalMs,
  },
  {
    // トピックを持たない --- storageRefreshIntervalMs の doc コメント参照。
    topic: null,
    prefixes: ['/api/storage'],
    refreshIntervalMs: storageRefreshIntervalMs,
  },
]

/** invalidateGroup は 1 グループ分のクエリキー接頭辞をまとめて invalidate する。 */
function invalidateGroup(queryClient: QueryClient, group: QueryGroup): void {
  for (const prefix of group.prefixes) {
    // orval のクエリキーは [url, params] の形なので、URL 接頭辞で照合する
    void queryClient.invalidateQueries({
      predicate: (query) => {
        const first = query.queryKey[0]
        return typeof first === 'string' && first.startsWith(prefix)
      },
    })
  }
}

/**
 * useServerEvents は /api/events を購読し、届いたトピックに対応するクエリを
 * invalidate する。加えて、SSE に依存しない 2 つのレベル経路を張る。
 *
 *   - グループごとの定期 invalidate（`refreshIntervalMs`）。接続が生きたまま
 *     個別の通知だけ捨てられたケースを回復する唯一の経路
 *   - 再接続時の全グループ invalidate。切断中に飛んだ通知は再送されないので、
 *     周期を待たずに取り直す
 *
 * `staleTime` は「stale と判定する期限」であって再取得を起こすタイマーではない
 * ので、これらが無いと再 mount・focus・別操作のどれも起きない画面は古いまま残る。
 */
export function useServerEvents() {
  const queryClient = useQueryClient()

  useEffect(() => {
    const source = new EventSource('/api/events')
    const listeners: { type: string; handler: () => void }[] = []
    const listen = (type: string, handler: () => void) => {
      source.addEventListener(type, handler)
      listeners.push({ type, handler })
    }

    for (const group of queryGroups) {
      // topic が null のグループ（storage）は SSE を購読しない --- 定期 invalidate
      // だけで収束させる（storageRefreshIntervalMs の doc コメント参照）
      if (group.topic === null) continue
      listen(group.topic, () => invalidateGroup(queryClient, group))
    }

    // EventSource は切断されると自分で繋ぎ直すが、切断中に飛んだ通知は二度と
    // 来ない。error（= 切断ないし失敗）を見た後の open でだけ全部取り直す。
    // 初回接続で取り直すと、各クエリの mount 時の取得と二重になる
    let disconnected = false
    listen('error', () => {
      disconnected = true
    })
    listen('open', () => {
      if (!disconnected) return
      disconnected = false
      for (const group of queryGroups) invalidateGroup(queryClient, group)
    })

    return () => {
      for (const { type, handler } of listeners) source.removeEventListener(type, handler)
      source.close()
    }
  }, [queryClient])

  useEffect(() => {
    const intervals = [...new Set(queryGroups.map((group) => group.refreshIntervalMs))]
    const timers = intervals.map((intervalMs) =>
      setInterval(() => {
        // 背面タブでは投げない。復帰時は refetchOnWindowFocus が拾う
        // （main.tsx の QueryClient 既定）
        if (document.visibilityState === 'hidden') return
        for (const group of queryGroups) {
          if (group.refreshIntervalMs === intervalMs) invalidateGroup(queryClient, group)
        }
      }, intervalMs),
    )

    return () => {
      for (const timer of timers) clearInterval(timer)
    }
  }, [queryClient])
}
