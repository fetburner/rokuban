import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react'
import { X } from 'lucide-react'

import { Button } from '@/components/ui/button'

type ToastKind = 'info' | 'error'

type Toast = {
  id: number
  message: string
  kind: ToastKind
  action?: { label: string; onClick: () => void }
}

type ToastInput = Omit<Toast, 'id' | 'kind'> & { kind?: ToastKind }

const ToastContext = createContext<((toast: ToastInput) => void) | null>(null)

/** 成功・情報トーストの表示時間。 */
const infoDurationMs = 6000

/**
 * action 付きトーストの表示時間。ボタンを読んで押す判断の分だけ、素の通知より
 * 長く残す（denpa の Toasts.svelte の設計を踏襲）。
 */
const actionDurationMs = 10000

/** タイマーの一時停止・再開のための、進行中トースト 1 件ぶんの状態。 */
type PendingTimer = {
  remainingMs: number
  timeoutId: ReturnType<typeof window.setTimeout> | undefined
  startedAt: number
}

/** 一時停止の要求元。hover と focus-within は独立に離れうるので OR で判定する。 */
type PauseReason = 'hover' | 'focus'

/**
 * ToastProvider は画面下部に短命の通知を出す。
 *
 * 予約のワンタップ実行と組み合わせて「予約しました [取消]」を出すことで、
 * 確認ダイアログを挟まずに誤タップを取り返せるようにする。
 *
 * **失敗（`kind: 'error'`）は自動で消えない。** 読み終える前に消えるべきで
 * ないため、閉じるボタンを押すまで残る。成功・情報だけがタイマーで消える。
 *
 * **機構のみ**: hover 中または focus-within の間はタイマーを止め、両方から
 * 離れたら残り時間で再開する。WCAG 2.2.1 の充足手段（Turn off / Adjust /
 * Extend）のいずれかを満たすと断定はしない（未検証。閉じるボタンへ 6 秒以内に
 * Tab で到達する経路が無く、キーボードのみでの充足は現時点で成立していない）。
 */
export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  // タイマーは state ではなく ref に持つ。toasts 配列を依存に含む useEffect で
  // 再スケジュールする形にすると、1 件足すたびに全トーストのタイマーが
  // 巻き戻ってしまう（罠を参照）。
  const timers = useRef(new Map<number, PendingTimer>())

  // hover / focus-within は独立に on/off するので、どちらか一方が終わっても
  // もう一方が続いていれば一時停止のままにする（OR）。
  const paused = useRef<{ hover: boolean; focus: boolean }>({ hover: false, focus: false })
  const isPaused = () => paused.current.hover || paused.current.focus

  const dismiss = useCallback((id: number) => {
    const timer = timers.current.get(id)
    if (timer?.timeoutId !== undefined) window.clearTimeout(timer.timeoutId)
    timers.current.delete(id)
    setToasts((current) => current.filter((t) => t.id !== id))
  }, [])

  const schedule = useCallback(
    (id: number, remainingMs: number) => {
      const timeoutId = window.setTimeout(() => dismiss(id), remainingMs)
      timers.current.set(id, { remainingMs, timeoutId, startedAt: Date.now() })
    },
    [dismiss],
  )

  /** 進行中の全タイマーを、残り時間を覚えたまま止める。 */
  const pauseTimers = useCallback(() => {
    for (const [id, timer] of timers.current) {
      if (timer.timeoutId === undefined) continue
      window.clearTimeout(timer.timeoutId)
      const elapsed = Date.now() - timer.startedAt
      timers.current.set(id, {
        remainingMs: Math.max(timer.remainingMs - elapsed, 0),
        timeoutId: undefined,
        startedAt: timer.startedAt,
      })
    }
  }, [])

  /** 一時停止中のまま残っているタイマー（＝止まっている全部）を残り時間で再開する。 */
  const resumeTimers = useCallback(() => {
    for (const [id, timer] of timers.current) {
      if (timer.timeoutId !== undefined) continue
      schedule(id, timer.remainingMs)
    }
  }, [schedule])

  const pause = useCallback(
    (reason: PauseReason) => {
      paused.current[reason] = true
      pauseTimers()
    },
    [pauseTimers],
  )

  const resume = useCallback(
    (reason: PauseReason) => {
      paused.current[reason] = false
      if (!isPaused()) resumeTimers()
    },
    [resumeTimers],
  )

  /** 進行中のタイマーを（あれば古いものを消してから）張り直す。 */
  const armTimer = useCallback(
    (id: number, durationMs: number) => {
      const timer = timers.current.get(id)
      if (timer?.timeoutId !== undefined) window.clearTimeout(timer.timeoutId)
      if (isPaused()) {
        // 届いた時点で既に hover / focus 中なら、動かさずに一時停止した
        // 状態で登録する（schedule すると即座に走り出してしまう）。
        timers.current.set(id, { remainingMs: durationMs, timeoutId: undefined, startedAt: Date.now() })
      } else {
        schedule(id, durationMs)
      }
    },
    [schedule],
  )

  const show = useCallback(
    (toast: ToastInput) => {
      const kind = toast.kind ?? 'info'
      const id = Date.now() + Math.random()

      // デデュープするのは error だけ（積み上がるのは自動で消えない error だけ
      // なので）。action は個体ごとにクロージャが違うので、action 付きは
      // デデュープしない --- 同じ文言でも別の対象を指すことがある
      // （例: 「予約しました: 番組名」+ 取消。EPG では再放送等で名前が
      // 一致するのは日常）。
      setToasts((current) =>
        kind === 'error' && current.some((t) => t.kind === 'error' && t.message === toast.message)
          ? current
          : [...current, { ...toast, kind, id }],
      )

      if (kind !== 'error') {
        armTimer(id, toast.action ? actionDurationMs : infoDurationMs)
      }
    },
    [armTimer],
  )

  const value = useMemo(() => show, [show])

  return (
    <ToastContext.Provider value={value}>
      {children}
      {/* ボトムタブの上に出す。position は fixed だが、タブに被らないよう
          --bottom-nav-height ぶん持ち上げる */}
      <div
        aria-live="polite"
        onMouseEnter={() => pause('hover')}
        onMouseLeave={() => resume('hover')}
        onFocus={() => pause('focus')}
        onBlur={() => resume('focus')}
        className="pointer-events-none fixed inset-x-0 bottom-0 z-30 flex flex-col items-center gap-2 px-4 pb-[calc(var(--bottom-nav-height)+0.5rem)] md:pb-4"
      >
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className="pointer-events-auto flex w-full max-w-sm items-start justify-between gap-3 rounded-lg border border-border bg-card px-4 py-3 text-sm shadow-lg"
          >
            <span className="min-w-0 line-clamp-3">{toast.message}</span>
            <div className="flex shrink-0 items-center gap-1">
              {toast.action && (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    toast.action?.onClick()
                    dismiss(toast.id)
                  }}
                >
                  {toast.action.label}
                </Button>
              )}
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label="閉じる"
                onClick={() => dismiss(toast.id)}
              >
                <X />
              </Button>
            </div>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

/** useToast はトーストを出す関数を返す。 */
export function useToast(): (toast: ToastInput) => void {
  const show = useContext(ToastContext)
  if (!show) {
    throw new Error('useToast must be used within a ToastProvider')
  }
  return show
}
