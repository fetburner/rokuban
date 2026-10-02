import { useQueryClient } from '@tanstack/react-query'

import { restoreRecording as restoreRecordingRequest, useDeleteRecording } from '@/api/generated'
import { mutationErrorMessage } from '@/lib/mutation-error-message'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { useToast } from '@/components/toaster'

/**
 * 詳細画面と終端カードで同じごみ箱操作を使う。
 * 成功後は Undo 付き通知を出し、一覧と詳細のキャッシュを同じキーで捨てる。
 */
export function useMoveRecordingToTrash(recordingId: number) {
  const deleteRecording = useDeleteRecording()
  const queryClient = useQueryClient()
  const toast = useToast()

  const invalidate = () =>
    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  const restore = () => {
    restoreRecordingRequest(recordingId)
      .then(invalidate)
      .catch((err: unknown) =>
        toast({ message: mutationErrorMessage('復元に失敗しました', err), kind: 'error' }),
      )
  }
  const moveToTrash = () =>
    deleteRecording.mutate(
      { id: recordingId },
      {
        onSuccess: () => {
          invalidate()
          toast({
            message: 'ごみ箱に移しました',
            actions: [{ label: '元に戻す', onClick: restore }],
          })
        },
        onError: (err) =>
          toast({ message: mutationErrorMessage('削除に失敗しました', err), kind: 'error' }),
      },
    )
  return { moveToTrash, pending: deleteRecording.isPending }
}
