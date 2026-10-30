import { useCallback, useSyncExternalStore } from 'react'

type ClockSubscriber = {
  intervalMs: number | undefined
  nextTickAtMs: number
  wakeAt: WakeAt | undefined
  wakeAtMs: number | undefined
  notify: () => void
}

/** WakeAt は指定時刻か、現在時刻から次の通知時刻を求める関数。 */
export type WakeAt = number | ((nowMs: number) => number)

/** UseNowOptions は共有時計の購読条件。intervalMs と wakeAtMs の少なくとも一方を指定する。 */
export type UseNowOptions = {
  /** 定期的に更新する間隔。指定しない場合は wakeAtMs の時刻だけ更新する。 */
  intervalMs?: number
  /** 購読するかどうか。false の間はストアへ登録しない。既定は true。 */
  active?: boolean
  /** 一度だけ起きる時刻、または次の起床時刻を返す関数。 */
  wakeAtMs?: WakeAt
}

function resolveWakeAtMs(wakeAt: WakeAt, nowMs: number): number {
  if (typeof wakeAt === 'number') {
    if (!Number.isFinite(wakeAt)) {
      throw new RangeError('useNow wakeAtMs must be finite')
    }
    return wakeAt
  }

  const nextWakeAtMs = wakeAt(nowMs)
  if (!Number.isFinite(nextWakeAtMs) || nextWakeAtMs <= nowMs) {
    throw new RangeError('useNow wakeAtMs function must return a finite future time')
  }
  return nextWakeAtMs
}

/** createClockStore は購読者の期限をまとめ、単一タイマーで現在時刻を配る。 */
function createClockStore(readNow: () => number) {
  let snapshot = readNow()
  const subscribers = new Set<ClockSubscriber>()
  let timer: number | undefined
  let timerDeadlineMs: number | undefined
  let idleSnapshotHeld = false

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
    if (!Number.isFinite(nextDeadlineMs)) {
      clearTimer()
      return
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
    const clockMovedBackward = nowMs < snapshot
    // 壁時計が戻ると、絶対時刻の期限は戻った分だけ先になり時計が止まる。
    // 次のタイマーで全購読者を通知し、期限関数も新しい時刻から計算し直す。
    snapshot = nowMs
    for (const subscriber of subscribers) {
      const intervalDue =
        subscriber.intervalMs !== undefined &&
        (clockMovedBackward || nowMs >= subscriber.nextTickAtMs)
      const wakeDue = subscriber.wakeAtMs !== undefined && nowMs >= subscriber.wakeAtMs
      const wakeScheduleChanged = clockMovedBackward && subscriber.wakeAt !== undefined
      if (!intervalDue && !wakeDue && !wakeScheduleChanged) continue

      if (intervalDue && subscriber.intervalMs !== undefined) {
        subscriber.nextTickAtMs = nowMs + subscriber.intervalMs
      }
      if (wakeDue) {
        subscriber.wakeAtMs =
          typeof subscriber.wakeAt === 'function'
            ? resolveWakeAtMs(subscriber.wakeAt, nowMs)
            : undefined
      } else if (wakeScheduleChanged && typeof subscriber.wakeAt === 'function') {
        subscriber.wakeAtMs = resolveWakeAtMs(subscriber.wakeAt, nowMs)
      }
      subscriber.notify()
    }

    scheduleNextTick()
  }

  return {
    getSnapshot: () => snapshot,
    /** getCurrentSnapshot は購読者が 0 なら時刻を読み直し、同じ同期処理の間は保持する。 */
    getCurrentSnapshot(): number {
      if (subscribers.size === 0 && !idleSnapshotHeld) {
        snapshot = readNow()
        idleSnapshotHeld = true
        queueMicrotask(() => {
          idleSnapshotHeld = false
        })
      }
      return snapshot
    },
    subscribe(
      intervalMs: number | undefined,
      wakeAt: WakeAt | undefined,
      notify: () => void,
    ): () => void {
      if (intervalMs !== undefined && (!Number.isFinite(intervalMs) || intervalMs <= 0)) {
        throw new RangeError('useNow intervalMs must be a positive finite number')
      }
      if (intervalMs === undefined && wakeAt === undefined) {
        throw new RangeError('useNow requires intervalMs or wakeAtMs')
      }

      const subscriber: ClockSubscriber = {
        intervalMs,
        nextTickAtMs: intervalMs === undefined ? Number.POSITIVE_INFINITY : snapshot + intervalMs,
        wakeAt,
        wakeAtMs: wakeAt === undefined ? undefined : resolveWakeAtMs(wakeAt, snapshot),
        notify,
      }
      subscribers.add(subscriber)
      scheduleNextTick()

      return () => {
        subscribers.delete(subscriber)
        // 保持していた値は購読者がいた頃に読んだもの。0 になったら次の読み取りで読み直す。
        if (subscribers.size === 0) idleSnapshotHeld = false
        scheduleNextTick()
      }
    },
  }
}

const clockStore = createClockStore(() => Date.now())

/**
 * useNow は共有時計の epoch ミリ秒を返す。
 *
 * 購読者がいる間だけストアにタイマーを 1 本置き、誰もいなければ止める。最も早い
 * 購読期限でそのタイマーを動かすが、別の購読者が先に起きても、自分の期限が来るまで
 * 通知しない。`intervalMs` は定期更新、`wakeAtMs` の数値は一度だけ起きる絶対時刻、
 * 関数は起床時に次の絶対時刻を返す継続的な境界を表す。後者は、現在のスナップショット
 * から次の正時や 0 時を計算して再設定できるため、呼び出し側は render 中に `Date.now()` を呼ばず、
 * 同じタイマーで定期更新と時刻境界を扱える。
 *
 * `intervalMs` と `wakeAtMs` のどちらかは必須。購読者が 0 の間は、active な呼び出しの
 * getSnapshot が同期処理（次のマイクロタスクまで）の最初に時刻を読み直し、その処理中は
 * 同じ値を返す。購読前の初回描画（ここで作るクエリキーを含む）に、最後の tick の古い
 * 時刻を出さないためである。React の契約「store が変わらない間 getSnapshot は同じ値を
 * 返し、変わったら購読者へ通知する」に対し、この変化は通知先の購読者がいない間にだけ
 * 起き、同じ処理の中では値を変えない。inactive の呼び出しは読み直さない。購読しない読み手が
 * 値を変えると、購読で止まる先が無く、読むたびに変わり続けうるため。
 *
 * `active` を false にすると購読しない。表示されていない録画時間軸の端のように、
 * 時計の進行が不要な間はタイマーを共有ストアへ追加しない。
 */
export function useNow({
  intervalMs,
  active = true,
  wakeAtMs,
}: UseNowOptions): number {
  const subscribe = useCallback(
    (notify: () => void) =>
      active ? clockStore.subscribe(intervalMs, wakeAtMs, notify) : () => {},
    [active, intervalMs, wakeAtMs],
  )

  const getSnapshot = active ? clockStore.getCurrentSnapshot : clockStore.getSnapshot
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}
