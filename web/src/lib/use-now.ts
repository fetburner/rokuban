import { useCallback, useSyncExternalStore } from 'react'

type ClockSubscriber = {
  intervalMs: number
  nextTickAtMs: number
  wakeAtMs: number | undefined
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
      if (subscriber.wakeAtMs !== undefined) {
        nextDeadlineMs = Math.min(nextDeadlineMs, subscriber.wakeAtMs)
      }
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
    // 壁時計が戻ると、絶対時刻の期限は戻った分だけ先になり時計が止まる。
    // 全員を期限到来扱いにして、この tick で通知し直す。
    if (nowMs < snapshot) {
      for (const subscriber of subscribers) subscriber.nextTickAtMs = nowMs
    }
    snapshot = nowMs
    for (const subscriber of subscribers) {
      const intervalDue = nowMs >= subscriber.nextTickAtMs
      const wakeDue = subscriber.wakeAtMs !== undefined && nowMs >= subscriber.wakeAtMs
      if (!intervalDue && !wakeDue) continue
      if (intervalDue) subscriber.nextTickAtMs = nowMs + subscriber.intervalMs
      if (wakeDue) subscriber.wakeAtMs = undefined
      subscriber.notify()
    }

    scheduleNextTick()
  }

  return {
    getSnapshot: () => snapshot,
    subscribe(intervalMs: number, wakeAtMs: number | undefined, notify: () => void): () => void {
      if (!Number.isFinite(intervalMs) || intervalMs <= 0) {
        throw new RangeError('useNow intervalMs must be a positive finite number')
      }
      if (wakeAtMs !== undefined && !Number.isFinite(wakeAtMs)) {
        throw new RangeError('useNow wakeAtMs must be a finite number')
      }

      // 最初の購読時に現在時刻を読み直す。前回の画面が閉じてから長時間経っていても、
      // 初回表示に古いスナップショットを使わない。
      if (subscribers.size === 0) snapshot = readNow()

      const subscriber: ClockSubscriber = {
        intervalMs,
        nextTickAtMs: snapshot + intervalMs,
        wakeAtMs,
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
 * useNow は一定間隔または指定時刻に更新される epoch ミリ秒を返す。
 *
 * 購読者がいる間だけストアにタイマーを 1 本置き、誰もいなければ止める。最も早い
 * 購読期限でそのタイマーを動かすが、短い間隔の購読者がいても、長い間隔を指定した
 * コンポーネントには自分の期限でだけ通知する。`wakeAtMs` を渡すと、その時刻にも一度
 * 通知する（`use-now.test.tsx`「指定時刻にも共有時計を更新する」）。追っかけ再生中の
 * 1 秒時計と番組境界のような予約時刻を、別タイマーにせず共有するためである。
 *
 * 購読者が 0 の間はスナップショットを更新しないため、購読前の初回描画には最後の
 * tick（またはモジュール読み込み時）の値が出る。購読直後に時刻を読み直して再描画
 * するので、初回描画の 1 回だけが古い。
 *
 * `active` を false にすると購読しない。表示されていない追っかけの録画端のように、
 * 時計の進行が不要な間はタイマーを共有ストアへ追加しない。
 */
export function useNow(intervalMs: number, active = true, wakeAtMs?: number): number {
  const subscribe = useCallback(
    (notify: () => void) =>
      active ? clockStore.subscribe(intervalMs, wakeAtMs, notify) : () => {},
    [active, intervalMs, wakeAtMs],
  )

  return useSyncExternalStore(subscribe, clockStore.getSnapshot, clockStore.getSnapshot)
}
