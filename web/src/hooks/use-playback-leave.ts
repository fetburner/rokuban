import { useEffect, useRef } from 'react'

type SendLeaveHint = (url: string) => void

/** usePlaybackLeave は pagehide・hidden・cleanup の離脱ヒントを URL ごとに送る。 */
export function usePlaybackLeave(url: string | null, send: SendLeaveHint): void {
  const sendRef = useRef(send)

  useEffect(() => {
    sendRef.current = send
  }, [send])

  useEffect(() => {
    if (url === null) return

    const leave = () => sendRef.current(url)
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
