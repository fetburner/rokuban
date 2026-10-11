import { useCallback, useSyncExternalStore } from 'react'

type ClockSubscriber = {
  intervalMs: number
  nextTickAtMs: number
  notify: () => void
}

/** createClockStore は購読者の期限をまとめ、単一タイマーで現在時刻を配る。 */
function createClockStore(readNow: () => number) {
  let snapshot = readNow()
  const subscribers = new Set<ClockSubscriber>()
  let timer: number | undefined
  let timerDeadlineMs: number | undefined

  function clearTimer(): void {
    if (timer === undefined) return
    window.clearTimeout(timer)
    timer = undefined
    timerDeadlineMs = undefined
  }

  function scheduleNextTick(): void {
    if (subscribers.size === 0) {
      clearTimer()
      return
    }

    let nextDeadlineMs = Number.POSITIVE_INFINITY
    for (const subscriber of subscribers) {
      nextDeadlineMs = Math.min(nextDeadlineMs, subscriber.nextTickAtMs)
    }
    if (timer !== undefined && timerDeadlineMs === nextDeadlineMs) return

    clearTimer()
    timerDeadlineMs = nextDeadlineMs
    timer = window.setTimeout(tick, Math.max(0, nextDeadlineMs - readNow()))
  }

  function tick(): void {
    timer = undefined
    timerDeadlineMs = undefined

    const nowMs = readNow()
    if (nowMs !== snapshot) {
      snapshot = nowMs
      for (const subscriber of subscribers) {
        if (nowMs < subscriber.nextTickAtMs) continue
        subscriber.nextTickAtMs = nowMs + subscriber.intervalMs
        subscriber.notify()
      }
    }

    scheduleNextTick()
  }

  return {
    getSnapshot: () => snapshot,
    subscribe(intervalMs: number, notify: () => void): () => void {
      if (!Number.isFinite(intervalMs) || intervalMs <= 0) {
        throw new RangeError('useNow intervalMs must be a positive finite number')
      }

      // 最初の購読時に現在時刻を読み直す。前回の画面が閉じてから長時間経っていても、
      // 初回表示に古いスナップショットを使わない。
      if (subscribers.size === 0) snapshot = readNow()

      const subscriber: ClockSubscriber = {
        intervalMs,
        nextTickAtMs: snapshot + intervalMs,
        notify,
      }
      subscribers.add(subscriber)
      scheduleNextTick()

      return () => {
        subscribers.delete(subscriber)
        scheduleNextTick()
      }
    },
  }
}

const clockStore = createClockStore(() => Date.now())

/**
 * useNow は一定間隔で更新される epoch ミリ秒を返す。
 *
 * 購読者がいる間だけストアにタイマーを 1 本置き、誰もいなければ止める。最も早い
 * 購読期限でそのタイマーを動かすが、短い間隔の購読者がいても、長い間隔を指定した
 * コンポーネントには自分の期限でだけ通知する。追っかけ再生中の 1 秒時計で、番組表や
 * 一覧の行まで毎秒再描画しないためである。
 *
 * `active` を false にすると購読しない。表示されていない追っかけの録画端のように、
 * 時計の進行が不要な間はタイマーを共有ストアへ追加しない。
 */
export function useNow(intervalMs: number, active = true): number {
  const subscribe = useCallback(
    (notify: () => void) =>
      active ? clockStore.subscribe(intervalMs, notify) : () => {},
    [active, intervalMs],
  )

  return useSyncExternalStore(subscribe, clockStore.getSnapshot, clockStore.getSnapshot)
}
