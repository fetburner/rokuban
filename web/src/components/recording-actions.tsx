import { useQueryClient } from '@tanstack/react-query'
import { MoreVertical, Trash2 } from 'lucide-react'
import { useState } from 'react'

import { ApiError } from '@/api/client'
import {
  restoreRecording as restoreRecordingRequest,
  useAddRecordingEncodeProfiles,
  useDeleteRecording,
  useListEncodeProfiles,
  usePurgeRecording,
  type Recording,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { Button } from '@/components/ui/button'
import { useToast } from '@/components/toaster'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { mutationErrorMessage } from '@/lib/mutation-error-message'

export function RecordingActions({ recording, trash }: { recording: Recording; trash: boolean }) {
  const recordingId = recording.id
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const [restoring, setRestoring] = useState(false)
  const queryClient = useQueryClient()
  const toast = useToast()
  const deleteRecording = useDeleteRecording()
  const purgeRecording = usePurgeRecording()

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  }

  const restore = () => {
    setRestoring(true)
    restoreRecordingRequest(recordingId)
      .then(() => invalidate())
      .catch((err: unknown) =>
        toast({ message: mutationErrorMessage('復元に失敗しました', err), kind: 'error' }),
      )
      .finally(() => setRestoring(false))
  }

  const busy = deleteRecording.isPending || restoring || purgeRecording.isPending

  if (!trash) {
    return (
      <div className="flex flex-col gap-2">
        <div className="flex flex-wrap gap-2">
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={busy}
            onClick={() => {
              deleteRecording.mutate(
                { id: recordingId },
                {
                  onSuccess: () => {
                    invalidate()
                    toast({
                      message: 'ごみ箱に移しました',
                      actions: [{ label: '元に戻す', onClick: () => restore() }],
                    })
                  },
                  onError: (err) =>
                    toast({ message: mutationErrorMessage('削除に失敗しました', err), kind: 'error' }),
                },
              )
            }}
          >
            <Trash2 data-icon="inline-start" />
            ごみ箱へ
          </Button>
        </div>
        <AddEncodeProfilesAction recording={recording} />
      </div>
    )
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Button type="button" variant="secondary" size="sm" disabled={busy} onClick={() => restore()}>
        復元
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={<Button type="button" variant="ghost" size="icon" aria-label="録画のその他の操作" />}
        >
          <MoreVertical />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem
            variant="destructive"
            disabled={busy}
            onClick={() => setPurgeConfirmOpen(true)}
          >
            今すぐ完全削除
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <AlertDialog open={purgeConfirmOpen} onOpenChange={setPurgeConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>今すぐ完全削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              この録画の原本・変換後のファイル・サムネイルを削除します。取り消せません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => {
                purgeRecording.mutate(
                  { id: recordingId },
                  {
                    onSuccess: () => {
                      invalidate()
                      toast({ message: '完全削除を予約しました' })
                    },
                    onError: (err) =>
                      toast({
                        message: mutationErrorMessage('完全削除の予約に失敗しました', err),
                        kind: 'error',
                      }),
                  },
                )
              }}
            >
              完全削除を予約する
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function AddEncodeProfilesAction({ recording }: { recording: Recording }) {
  const hasOriginal = recording.sizeBytes !== undefined
  const profilesQuery = useListEncodeProfiles()
  const profiles = unwrap(profilesQuery.data) ?? []
  const alreadyRequested = recording.encodeProfiles ?? []
  const alreadyRequestedSet = new Set(alreadyRequested)
  const addable = profiles.filter((p) => !alreadyRequestedSet.has(p.name))
  const [selected, setSelected] = useState<string[]>([])
  const queryClient = useQueryClient()
  const toast = useToast()
  const addProfiles = useAddRecordingEncodeProfiles()

  if (!hasOriginal) {
    return (
      <p className="text-xs text-muted-foreground">
        この録画には再生可能な原本がありません。追加のエンコードは依頼できません。
      </p>
    )
  }
  if (profilesQuery.isError) {
    return <p className="text-xs text-destructive">プロファイル一覧の取得に失敗しました</p>
  }
  if (profilesQuery.isPending || profiles.length === 0) return null

  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border p-2">
      <span className="text-xs text-muted-foreground">事後エンコードの追加</span>
      {alreadyRequested.length > 0 && (
        <p className="text-xs text-muted-foreground">追加済み: {alreadyRequested.join(', ')}</p>
      )}
      {addable.length === 0 ? (
        <p className="text-xs text-muted-foreground">すべてのエンコードプロファイルが追加済みです。</p>
      ) : (
        <>
          <ul
            role="group"
            aria-label="追加するエンコードプロファイル"
            className="flex flex-col gap-1"
          >
            {addable.map((p) => {
              const checked = selected.includes(p.name)
              return (
                <li key={p.name}>
                  <label className="flex min-h-8 cursor-pointer items-center gap-2 text-sm text-foreground">
                    <input
                      type="checkbox"
                      className="size-4 accent-primary"
                      checked={checked}
                      disabled={addProfiles.isPending}
                      onChange={() =>
                        setSelected((s) =>
                          checked ? s.filter((n) => n !== p.name) : [...s, p.name],
                        )
                      }
                    />
                    <span>{p.name}</span>
                  </label>
                </li>
              )
            })}
          </ul>
          <Button
            type="button"
            size="sm"
            disabled={selected.length === 0 || addProfiles.isPending}
            onClick={() => {
              addProfiles.mutate(
                { id: recording.id, data: { profiles: selected } },
                {
                  onSuccess: () => {
                    setSelected([])
                    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
                    toast({ message: 'エンコードを依頼しました' })
                  },
                  onError: (err) =>
                    toast({
                      message:
                        err instanceof ApiError && err.status === 409
                          ? '原本の状態が変わったため追加できませんでした（削除済み・削除処理中・未取り込みのいずれか）。画面を更新してから再度お試しください。'
                          : (apiErrorMessage(err) ?? 'エンコードの依頼に失敗しました'),
                      kind: 'error',
                    }),
                },
              )
            }}
          >
            {addProfiles.isPending ? '依頼中…' : '追加エンコードを依頼'}
          </Button>
        </>
      )}
    </div>
  )
}
