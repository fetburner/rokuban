import { useEffect, useState } from 'react'

import { disconnectedBannerDelayMs, useConnectionState } from '@/lib/events'
import { formatTime } from '@/lib/format'

/**
 * ConnectionBanner は SSE（`/api/events`）が切断中であることを知らせる居座り
 * バナー（issue: U-4）。
 *
 * UI は SSE + 定期 invalidate（`lib/events.ts`）で自動更新される前提だが、
 * 接続が切れている間は「最後に見た状態」が新しく見え続ける。api ロールの
 * ダウン・プロキシのタイムアウト・ネットワーク断のどれでも起きうるので、
 * 切れていること自体を画面に出す。
 *
 * **`navigator.onLine` は使わない** --- false のときだけ確実で、true は
 * 「オンラインかもしれない」程度の保証しかない（プロキシ側の障害を見逃す）。
 * 判定は SSE の `error` / `open` イベントだけを見る（`lib/events.ts` の
 * `ConnectionStatus`。`EventSource.readyState` は再接続中もずっと
 * `CONNECTING` を返すので使えない）。
 *
 * `disconnectedBannerDelayMs`（10 秒）続けて切断中でなければ帯は出さない ---
 * EventSource は瞬断でも自分で再接続するので、遅延なしで出すと瞬断のたびに
 * 点滅する。この effect は `status` の値そのものに依存しており、
 * `useConnectionState` は status が変わらない限り再レンダーを起こさないので
 * （`lib/events.ts` の `setConnectionState` 参照）、切断が続く間に追加の
 * `error` が来てもタイマーは再セットされない（最初の `error` からの経過だけを
 * 見る）。
 *
 * 背面タブでは運用状態の定期 invalidate を止めているが、この判定は SSE の
 * 状態だけを見るので特別扱いは要らない --- 背面で `error` が来ても、状態が
 * `connecting` に戻る（次の `open`）かタブ復帰時の再レンダーで自然に判定し
 * 直される。
 *
 * 色は `bg-muted` + `text-foreground`（信号色を使わない）。壊れたのではなく
 * 自動更新が止まっているだけであり、サーキットブレーカーのバナー
 * （`components/circuit-breaker-banner.tsx`、`destructive`）と混同させない。
 */
export function ConnectionBanner() {
  const { status, lastConnectedAt } = useConnectionState()
  const [showBanner, setShowBanner] = useState(false)

  useEffect(() => {
    if (status !== 'disconnected') {
      setShowBanner(false)
      return
    }
    const timer = setTimeout(() => setShowBanner(true), disconnectedBannerDelayMs)
    return () => clearTimeout(timer)
  }, [status])

  if (!showBanner) return null

  return (
    <div role="status" className="border-b border-border bg-muted px-4 py-2 text-xs text-foreground">
      自動更新が止まっています。再接続中…
      {lastConnectedAt && `（最終更新 ${formatTime(lastConnectedAt)}）`}
    </div>
  )
}
