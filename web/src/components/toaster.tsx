import { createContext, useCallback, useContext, useMemo, useState } from 'react'

import { Button } from '@/components/ui/button'

type Toast = {
  id: number
  message: string
  action?: { label: string; onClick: () => void }
}

type ToastInput = Omit<Toast, 'id'>

const ToastContext = createContext<((toast: ToastInput) => void) | null>(null)

const toastDurationMs = 6000

/**
 * ToastProvider は画面下部に短命の通知を出す。
 *
 * 予約のワンタップ実行と組み合わせて「予約しました [取消]」を出すことで、
 * 確認ダイアログを挟まずに誤タップを取り返せるようにする。
 */
export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((t) => t.id !== id))
  }, [])

  const show = useCallback(
    (toast: ToastInput) => {
      const id = Date.now() + Math.random()
      setToasts((current) => [...current, { ...toast, id }])
      window.setTimeout(() => dismiss(id), toastDurationMs)
    },
    [dismiss],
  )

  const value = useMemo(() => show, [show])

  return (
    <ToastContext.Provider value={value}>
      {children}
      {/* ボトムタブの上に出す。position は fixed だが、タブに被らないよう
          --bottom-nav-height ぶん持ち上げる */}
      <div
        aria-live="polite"
        className="pointer-events-none fixed inset-x-0 bottom-0 z-30 flex flex-col items-center gap-2 px-4 pb-[calc(var(--bottom-nav-height)+0.5rem)] md:pb-4"
      >
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className="pointer-events-auto flex w-full max-w-sm items-center justify-between gap-3 rounded-lg border border-border bg-card px-4 py-3 text-sm shadow-lg"
          >
            <span className="min-w-0 truncate">{toast.message}</span>
            {toast.action && (
              <Button
                variant="ghost"
                size="sm"
                className="shrink-0"
                onClick={() => {
                  toast.action?.onClick()
                  dismiss(toast.id)
                }}
              >
                {toast.action.label}
              </Button>
            )}
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
