import { useToast } from '@/components/toaster'

/** useCopyLink は同一オリジンのパスを絶対 URL にしてクリップボードへ写し、結果をトーストで知らせる。 */
export function useCopyLink(path: string) {
  const toast = useToast()
  return async () => {
    try {
      await navigator.clipboard.writeText(new URL(path, window.location.origin).href)
      toast({ message: 'リンクをコピーしました' })
    } catch {
      toast({ message: 'リンクをコピーできませんでした', kind: 'error' })
    }
  }
}
