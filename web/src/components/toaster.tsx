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

/**
 * 成功・情報トーストの表示時間。
 *
 * hover / focus-within の間は一時停止するので、WCAG 2.2.1（タイミング調整可能）の
 * 「自動で消える通知は利用者が延長・停止できる」を満たす。
 */
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

/**
 * ToastProvider は画面下部に短命の通知を出す。
 *
 * 予約のワンタップ実行と組み合わせて「予約しました [取消]」を出すことで、
 * 確認ダイアログを挟まずに誤タップを取り返せるようにする。
 *
 * **失敗（`kind: 'error'`）は自動で消えない。** 読み終える前に消えるべきで
 * ないため、閉じるボタンを押すまで残る。成功・情報だけがタイマーで消える。
 */
export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  // タイマーは state ではなく ref に持つ。toasts 配列を依存に含む useEffect で
  // 再スケジュールする形にすると、1 件足すたびに全トーストのタイマーが
  // 巻き戻ってしまう（罠を参照）。
  const timers = useRef(new Map<number, PendingTimer>())

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

  /** hover / focus-within の間、進行中の全タイマーを残り時間を覚えたまま止める。 */
  const pause = useCallback(() => {
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

  /** hover / focus-within を離れたら、残り時間で再開する。 */
  const resume = useCallback(() => {
    for (const [id, timer] of timers.current) {
      if (timer.timeoutId !== undefined) continue
      schedule(id, timer.remainingMs)
    }
  }, [schedule])

  const show = useCallback(
    (toast: ToastInput) => {
      const id = Date.now() + Math.random()
      const kind = toast.kind ?? 'info'
      setToasts((current) => [...current, { ...toast, kind, id }])
      if (kind !== 'error') {
        schedule(id, toast.action ? actionDurationMs : infoDurationMs)
      }
    },
    [schedule],
  )

  const value = useMemo(() => show, [show])

  return (
    <ToastContext.Provider value={value}>
      {children}
      {/* ボトムタブの上に出す。position は fixed だが、タブに被らないよう
          --bottom-nav-height ぶん持ち上げる */}
      <div
        aria-live="polite"
        onMouseEnter={pause}
        onMouseLeave={resume}
        onFocus={pause}
        onBlur={resume}
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
              <button
                type="button"
                aria-label="閉じる"
                onClick={() => dismiss(toast.id)}
                className="rounded p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                <X className="size-4" aria-hidden="true" />
              </button>
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
