import { useQueryClient } from '@tanstack/react-query'

import {
  getGetRecordingChapterReviewQueryKey,
  getGetRecordingChaptersQueryKey,
  useRetryRecordingCMDetection,
  type Recording,
} from '@/api/generated'
import { useToast } from '@/components/toaster'
import { useCMDetectEnabled } from '@/lib/capabilities'
import { isAwaitingAdoption } from '@/lib/cm-detect-stage'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { mutationErrorMessage } from '@/lib/mutation-error-message'

/** useRetryCMDetection は録画に対する再検出の可否と、再検出後の再取得を共通に扱う。 */
export function useRetryCMDetection(recording: Recording | undefined) {
  const queryClient = useQueryClient()
  const toast = useToast()
  const cmDetectEnabled = useCMDetectEnabled()
  const mutation = useRetryRecordingCMDetection({
    mutation: {
      onSuccess: async (_response, { id }) => {
        toast({ message: 'CM 検出を再試行します' })
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] }),
          queryClient.invalidateQueries({ queryKey: getGetRecordingChaptersQueryKey(id) }),
          queryClient.invalidateQueries({ queryKey: getGetRecordingChapterReviewQueryKey(id) }),
        ])
      },
      onError: (err) =>
        toast({ message: mutationErrorMessage('CM 検出の再試行に失敗しました', err), kind: 'error' }),
    },
  })
  const show = recording !== undefined &&
    (recording.cmDetection.state === 'detected' || recording.cmDetection.state === 'failed') &&
    !isAwaitingAdoption(recording.cmDetection)
  // 検出は原本を読む。原本が無いかサーバーで機能が無効なら API は 409 を返す。
  const enabled = show && cmDetectEnabled && recording.sizeBytes !== undefined
  return { cmDetectEnabled, show, enabled, pending: mutation.isPending, run: (id: number) => mutation.mutate({ id }) }
}
