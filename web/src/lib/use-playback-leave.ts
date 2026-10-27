import { useEffect } from 'react'

import { sendPlaybackLeaveHint } from '@/lib/live'

/**
 * usePlaybackLeave は url の生存期間に離脱ヒントを紐づけ、cleanup・pagehide・hidden で送る。
 * `unload` は使わない。モバイル Safari は bfcache のため発火せず、`pagehide` が届く終端イベントだからである。
 * hidden も聴くのは、タブが破棄されず hidden のまま放置される経路では `pagehide` すら来ないためである。
 * url には画質や再読み込みのキーを入れない。入れると切替のたびに離脱が飛ぶ
 * （`docs/frontend/live/quality.md` §切替と URL）。
 */
export function usePlaybackLeave(url: string | null): void {
  useEffect(() => {
    if (url === null) return
    const leave = () => sendPlaybackLeaveHint(url)
    const onVisibilityChange = () => {
      if (document.visibilityState === 'hidden') leave()
    }
    window.addEventListener('pagehide', leave)
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      window.removeEventListener('pagehide', leave)
      document.removeEventListener('visibilitychange', onVisibilityChange)
      leave()
    }
  }, [url])
}
