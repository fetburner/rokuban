import { useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'

import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'

const Kbd = ({ children }: { children: string }) => (
  <kbd className="rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">
    {children}
  </kbd>
)

const nav: [string, string, string][] = [
  ['h', '/', 'ホーム'],
  ['p', '/programs', '番組'],
  ['r', '/recordings', '録画'],
  ['s', '/reservations', '予約'],
  ['l', '/live', 'ライブ'],
]

/** MockH7 は H-7 のモック用グローバルキー（/ ・ g 連鎖 ・ ?）。window.__mock.h7 のときだけ有効。 */
export function MockH7() {
  const enabled = (window as unknown as { __mock?: { h7?: boolean } }).__mock?.h7
  const navigate = useNavigate()
  const [help, setHelp] = useState(false)
  const [pending, setPending] = useState(false)
  useEffect(() => {
    if (!enabled) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      if (t.closest('input,textarea,select,[contenteditable]') || e.metaKey || e.ctrlKey) return
      if (e.key === '/') {
        e.preventDefault()
        document.querySelector<HTMLInputElement>('input[type=search]')?.focus()
      } else if (e.key === '?') setHelp(true)
      else if (e.key === 'g') {
        setPending(true)
        clearTimeout(timer)
        timer = setTimeout(() => setPending(false), 5000)
      } else if (pending) {
        const hit = nav.find(([k]) => k === e.key)
        if (hit) void navigate({ to: hit[1] as never })
        setPending(false)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      clearTimeout(timer)
    }
  }, [enabled, pending, navigate])
  if (!enabled) return null
  return (
    <>
      {pending && (
        <div className="fixed bottom-4 left-4 z-40 flex items-center gap-1.5 rounded-lg bg-popover px-3 py-2 text-sm text-popover-foreground ring-1 ring-foreground/10">
          <Kbd>g</Kbd>
          <span className="text-muted-foreground">に続けて h / p / r / s / l</span>
        </div>
      )}
      <Dialog open={help} onOpenChange={setHelp}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>キーボードショートカット</DialogTitle>
          </DialogHeader>
          <dl className="grid grid-cols-[5rem_1fr] items-center gap-x-4 gap-y-2 text-sm">
            <dt><Kbd>/</Kbd></dt><dd>検索欄へ</dd>
            {nav.map(([k, , label]) => (
              <div key={k} className="contents">
                <dt className="flex gap-1"><Kbd>g</Kbd><Kbd>{k}</Kbd></dt>
                <dd>{label}</dd>
              </div>
            ))}
          </dl>
          <h3 className="text-sm font-medium">選択モード</h3>
          <dl className="grid grid-cols-[5rem_1fr] items-center gap-x-4 gap-y-2 text-sm">
            <dt><Kbd>⌘A</Kbd></dt><dd>すべて選択</dd>
            <dt><Kbd>⇧⌘A</Kbd></dt><dd>選択を解除</dd>
            <dt><Kbd>Esc</Kbd></dt><dd>選択モードを終える</dd>
          </dl>
        </DialogContent>
      </Dialog>
    </>
  )
}
