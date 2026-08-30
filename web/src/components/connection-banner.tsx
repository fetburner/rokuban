import { useEffect, useState } from 'react'

import { disconnectedBannerDelayMs, useConnectionState } from '@/lib/events'
import { formatTime } from '@/lib/format'

/**
 * ConnectionBanner は SSE（`/api/events`）が切断中であることを知らせる居座り
 * バナー（issue #456）。判断の根拠は `docs/frontend/shell.md` §切断中を画面に
 * 出す --- ここにはコードを読むだけでは追えない実装細部だけを残す。
 *
 * `disconnected` は `error` の後・次の `open` の前で定義する（`lib/events.ts`
 * の `ConnectionStatus`）。背面タブでの定期 invalidate 停止とは独立で、この
 * 判定は SSE の状態だけを見る --- 背面タブでの `setTimeout` の発火間隔（ブラウザの
 * スロットリング挙動）が実際にどう振る舞うかは未検証。
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
      更新通知が止まっています。再接続中…
      {lastConnectedAt && `（最終接続 ${formatTime(lastConnectedAt)}）`}
    </div>
  )
}
