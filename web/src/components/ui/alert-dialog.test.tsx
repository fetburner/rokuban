import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'

/**
 * Harness は circuit-breaker-banner.tsx / recordings.tsx の使い方（open state を
 * 自前で持ち、確定ボタンの onClick で副作用を実行する）を最小構成で再現する。
 * このコンポーネント自身が「呼び出し側の個別クローズ処理を削っても閉じるか」を
 * 確認する対象（issue #131 の判定手段）。
 */
function Harness({ onConfirm }: { onConfirm: () => void }) {
  const [open, setOpen] = useState(false)
  return (
    <AlertDialog open={open} onOpenChange={setOpen}>
      <AlertDialogTrigger render={<Button>開く</Button>} />
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>確認</AlertDialogTitle>
          <AlertDialogDescription>実行しますか？</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>キャンセル</AlertDialogCancel>
          <AlertDialogAction onClick={onConfirm}>実行する</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

describe('AlertDialogAction', () => {
  it('クリックで onClick を呼び、呼び出し側が個別にクローズ処理を書かなくてもダイアログが閉じる', async () => {
    const user = userEvent.setup()
    const onConfirm = vi.fn()
    render(<Harness onConfirm={onConfirm} />)

    await user.click(screen.getByRole('button', { name: '開く' }))
    // 開いている状態を確認してからクリックする（非同期の空虚な成功を避ける）。
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '実行する' }))

    expect(onConfirm).toHaveBeenCalledTimes(1)
    // ダイアログが DOM にマウントされたまま残っていないことを確認する
    // （jsdom で観測できる「閉じ忘れ」の壊れ方そのもの）。
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
  })
})
