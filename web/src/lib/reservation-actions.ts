import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useEffect, useMemo, useState } from 'react'

import {
  useDeleteProgramIntent,
  usePatchProgramOverrides,
  usePutProgramIntent,
  type ProgramOverridesInput,
  type Reservation,
} from '@/api/generated'
import { apiErrorMessage } from '@/api/unwrap'
import type { ReservationActions } from '@/components/program-list'
import { useToast } from '@/components/toaster'
import { programIdentity, type SiteProgram } from '@/lib/all-sites-services'
import {
  capacityOveragesQueryKeyPrefix,
  reservationsQueryKeyPrefix,
} from '@/lib/events'
import { mutationErrorMessage } from '@/lib/mutation-error-message'

/**
 * useReservationActions は番組表のリストとグリッドで共有する予約操作を組み立てる。
 *
 * 予約の意図はサーバー値が反映されるまで楽観的に表示し、取消後のトーストからの
 * Undo はページ遷移をまたいでも実行できるよう `mutateAsync` で処理する。
 */
export function useReservationActions(
  serverReservedIds: ReadonlySet<string>,
  sourceByProgramId: ReadonlyMap<string, Reservation['source']>,
): ReservationActions {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const toast = useToast()
  const putIntent = usePutProgramIntent()
  const deleteIntent = useDeleteProgramIntent()
  const patchOverrides = usePatchProgramOverrides()

  const [busyProgramIds, setBusyProgramIds] = useState<ReadonlySet<string>>(new Set())
  const [optimistic, setOptimistic] = useState<ReadonlyMap<string, boolean>>(new Map())

  useEffect(() => {
    setOptimistic((current) => {
      if (current.size === 0) return current
      let changed = false
      const next = new Map(current)
      for (const [programId, want] of current) {
        if (serverReservedIds.has(programId) === want) {
          next.delete(programId)
          changed = true
        }
      }
      return changed ? next : current
    })
  }, [serverReservedIds])

  const reservedProgramIds = useMemo(() => {
    const set = new Set(serverReservedIds)
    for (const [programId, want] of optimistic) {
      if (want) set.add(programId)
      else set.delete(programId)
    }
    return set
  }, [serverReservedIds, optimistic])

  const setBusy = (programId: string, busy: boolean) => {
    setBusyProgramIds((current) => {
      const next = new Set(current)
      if (busy) next.add(programId)
      else next.delete(programId)
      return next
    })
  }

  const setOptimisticReserved = (programId: string, reserved: boolean | undefined) => {
    setOptimistic((current) => {
      const next = new Map(current)
      if (reserved === undefined) next.delete(programId)
      else next.set(programId, reserved)
      return next
    })
  }

  const invalidateReservations = () => {
    void queryClient.invalidateQueries({ queryKey: [reservationsQueryKeyPrefix] })
    void queryClient.invalidateQueries({ queryKey: [capacityOveragesQueryKeyPrefix] })
  }

  const revive = (program: SiteProgram, source: Reservation['source'] | undefined) => {
    const key = programIdentity(program.site, program.programId)
    setBusy(key, true)
    setOptimisticReserved(key, true)
    void (async () => {
      try {
        if (source === 'rule') {
          await deleteIntent.mutateAsync({ site: program.site, programId: program.programId })
        } else {
          await putIntent.mutateAsync({
            site: program.site,
            programId: program.programId,
            data: { action: 'record' },
          })
        }
        invalidateReservations()
        toast({ message: '予約を元に戻しました' })
      } catch (err) {
        toast({ message: mutationErrorMessage('予約への復帰に失敗しました', err) })
        setOptimisticReserved(key, undefined)
      } finally {
        setBusy(key, false)
      }
    })()
  }

  const cancel = (program: SiteProgram) => {
    const key = programIdentity(program.site, program.programId)
    const source = sourceByProgramId.get(key)
    setBusy(key, true)
    setOptimisticReserved(key, false)
    void (async () => {
      try {
        await putIntent.mutateAsync({
          site: program.site,
          programId: program.programId,
          data: { action: 'skip' },
        })
        invalidateReservations()
        toast({
          message: '予約を取消しました',
          actions: [{ label: '元に戻す', onClick: () => revive(program, source) }],
        })
      } catch (err) {
        toast({ message: mutationErrorMessage('予約の取消に失敗しました', err), kind: 'error' })
        setOptimisticReserved(key, undefined)
      } finally {
        setBusy(key, false)
      }
    })()
  }

  const reservationToastActions = (program: SiteProgram) => [
    { label: '取消', onClick: () => cancel(program) },
    {
      label: '設定',
      onClick: () =>
        void navigate({
          to: '/reservations/$site/$programId',
          params: { site: program.site, programId: String(program.programId) },
        }),
    },
  ]

  const reserve = (program: SiteProgram, overrides?: ProgramOverridesInput) => {
    const key = programIdentity(program.site, program.programId)
    setBusy(key, true)
    setOptimisticReserved(key, true)
    void (async () => {
      try {
        await putIntent.mutateAsync({
          site: program.site,
          programId: program.programId,
          data: { action: 'record' },
        })
        invalidateReservations()

        if (overrides === undefined) {
          toast({
            message: `予約しました: ${program.name}`,
            actions: reservationToastActions(program),
          })
          return
        }

        try {
          await patchOverrides.mutateAsync({
            site: program.site,
            programId: program.programId,
            data: overrides,
          })
          toast({
            message: `予約しました（エンコード設定つき）: ${program.name}`,
            actions: reservationToastActions(program),
          })
        } catch (err) {
          toast({
            message: `予約はできましたが、エンコード設定の保存に失敗しました: ${
              apiErrorMessage(err) ?? '不明なエラー'
            }`,
            kind: 'error',
          })
        }
      } catch (err) {
        toast({ message: mutationErrorMessage('予約に失敗しました', err), kind: 'error' })
        setOptimisticReserved(key, undefined)
      } finally {
        setBusy(key, false)
      }
    })()
  }

  return {
    reserve,
    cancel,
    isBusy: (program) => busyProgramIds.has(programIdentity(program.site, program.programId)),
    reservedProgramIds,
  }
}
