import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'

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
 * useReservationActions は予約 / 取消の実行を組み立てる。
 *
 * リストとグリッドの両方が同じ経路を通るようページ側に持ち上げてある
 * （予約の見え方が表示形式で分岐すると、M2-9 の受け入れ条件「リストとグリッドで
 * 予約状態が一致する」がコード上で担保されない）。
 *
 * 意図（PUT/DELETE .../intent）は reservations 行を同期的に作らない（issue #29
 * の決定: reservations の書き手は ruler だけにする）。ruler_pass ヒントで実質
 * 秒オーダーではあるが、invalidate して取り直すだけでは一覧の反映がその間
 * 遅れる。**楽観的更新**で見た目を即時反映し、サーバー値（serverReservedIds）が
 * 追いついたら上書きを外す（自己修復。SSE の invalidate で最終的に一致する）。
 *
 * `reserve` は `overrides` を受け取ると intent の PUT に続けて
 * overrides の PATCH も呼ぶ（issue #132）。#29 の決定どおり intent の
 * ボディは `action` のみのまま変えず、overrides は別リクエストにする ---
 * ただし UI からは「予約」ボタン 1 回の操作に見える。overrides の PATCH が
 * 失敗しても予約自体（intent）は成立しているので、その旨を分けてトーストで示す。
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

  // mutation の isPending は全行で共有されるため、操作中の番組だけを覚えておく。
  // これがないと 1 件予約する間にリスト全行のボタンが無効化される。
  const [busyProgramIds, setBusyProgramIds] = useState<ReadonlySet<string>>(new Set())
  // site:programId → 楽観的に見せたい予約状態（true=予約済み / false=未予約）。
  const [optimistic, setOptimistic] = useState<ReadonlyMap<string, boolean>>(new Map())

  // サーバー値が楽観的な予想に追いついたものは、描画時に導出から外す。
  // effect で state を掃除すると、サーバー値が届いた直後に一度だけ古い予約状態を
  // 描いてから再描画する。state 自体を同期的に書き換えず、現在のサーバー値に
  // 一致する上書きだけを表示計算から除外するので、見た目も即時に収束する。
  const effectiveOptimistic = useMemo(() => {
    const next = new Map<string, boolean>()
    for (const [programId, want] of optimistic) {
      if (serverReservedIds.has(programId) !== want) next.set(programId, want)
    }
    return next
  }, [optimistic, serverReservedIds])

  const reservedProgramIds = useMemo(() => {
    const set = new Set(serverReservedIds)
    for (const [programId, want] of effectiveOptimistic) {
      if (want) set.add(programId)
      else set.delete(programId)
    }
    return set
  }, [serverReservedIds, effectiveOptimistic])

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
    // 容量超過は予約集合からの導出値なので、予約が増減すれば作り直させる。
    // 帯を古いまま残すと「予約したのに不足が消えない / 出ない」になる
    void queryClient.invalidateQueries({ queryKey: [capacityOveragesQueryKeyPrefix] })
  }

  // revive は取消トーストの「元に戻す」から呼ぶ（issue #453）。`reserve` と
  // 同じ楽観更新の経路（setOptimisticReserved → catch で undefined に戻す）
  // を通す。
  //
  // **`mutateAsync` + try/catch にする（`.mutate(vars, {onSuccess, onError})`
  // は使わない）。** トーストは 6 秒生きる一方、番組表は行 1 件ぶんの
  // busy/optimistic state しか持たない別ページの component state なので、
  // 「取消 → 別ページへ移動 → まだ出ているトーストの『元に戻す』を押す」の
  // 間にこのページ自体がアンマウントされうる。`.mutate` の第 2 引数
  // コールバックは `MutationObserver`（`@tanstack/query-core`）に購読者
  // （＝マウント中のコンポーネント）が居るときしか呼ばれない
  // （`#notify()` の `hasListeners()` 判定。実測: アンマウント後は PUT/DELETE
  // 自体は飛ぶが `onSuccess`/`onError` が一度も呼ばれず、成功も失敗も無音に
  // なる）。`mutateAsync` は `Mutation#execute` の Promise をそのまま返すので
  // この判定を経由しない（`pages/reservation-detail.tsx` の `revive` と同じ
  // 理由。詳細はそちらのコメント）。
  //
  // `source` で分岐する: 手動予約の逆操作は `PUT intent{record}` でよいが、
  // ルール由来の予約に同じ PUT を送ると `program_intents` に record 行が残り、
  // 以後ルールがマッチしなくなっても予約が残る「種別: 手動」の予約に恒久的に
  // 変わってしまう（`internal/api/handler.go` の source 導出、
  // `TestGetReservation_SourceManualDespiteRuleMatch`）。ルール由来の厳密な
  // 逆操作は `DELETE .../intent`（明示的な意見を取り下げ、ルール評価に戻す）。
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

  // cancel も同じ理由（トーストの「取消」action・`revive` からの「元に戻す」
  // action、双方が遷移をまたぎうる）で `mutateAsync` + try/catch にする。
  const cancel = (program: SiteProgram) => {
    const key = programIdentity(program.site, program.programId)
    // Undo の分岐に使う。取消の瞬間の source を捕まえておく --- 取消後は
    // サーバー値（`sourceByProgramId` の元になる `reservations.data`）が
    // 変わりうるため、クリック時点の値を閉じ込める。
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
        // 予約のワンタップ + トースト「取消」と対称にする（issue #453）。
        // 誤タップの被害は「録れない」側に出るので、取消にも同じ取り返し
        // 手段を置く。
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

  // 予約作成（PUT .../intent）自体は action のみのまま変更しない（issue #29 の
  // 決定）。overrides は別 PATCH のまま呼ぶが、ProgramRow の展開パネルで
  // encodeProfiles / keepOriginal を触っていなければ overrides は
  // `undefined` で渡ってくるので、この場合は PATCH 自体を呼ばない ---
  // 呼ぶと「既定のまま」という意味の無い override 行を作ってしまう
  // （不変条件 10）。UI 上は「予約」ボタン 1 回の操作に見せる（issue #132）。
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
          // 確認ダイアログを挟まない代わりに、直後に取り返せるようにする
          toast({
            message: `予約しました: ${program.name}`,
            actions: reservationToastActions(program),
          })
          return
        }

        // overrides の PATCH は `program_snapshots (site, programId)` への FK を
        // 要求する。EPG プロジェクションに無い番組（想定上は起こりにくいが、
        // ここに来る時点で番組表に出ているので通常は満たされる）だと 400 になり
        // うるので、予約自体の成功とは切り離してハンドルする ---
        // 予約は成立しているので「予約に失敗しました」にはしない。
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
