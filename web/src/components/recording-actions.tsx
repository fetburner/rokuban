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

/**
 * RecordingActions は論理削除 / 復元 / 即時 purge 印 + 追加エンコードの依頼。
 * 削除系はいずれも DB だけを触り、ファイルは消さない（M3-7）。
 */
export function RecordingActions({ recording, trash }: { recording: Recording; trash: boolean }) {
  const recordingId = recording.id
  const [purgeConfirmOpen, setPurgeConfirmOpen] = useState(false)
  const [restoring, setRestoring] = useState(false)
  const queryClient = useQueryClient()
  const toast = useToast()
  const deleteRecording = useDeleteRecording()
  const purgeRecording = usePurgeRecording()

  const invalidate = () => {
    // ライブラリとごみ箱の両方を捨てる（片側の操作がもう片側の集合を変える）。
    // 単体ページ（pages/recording-detail.tsx）はこのキーに前方一致する
    // クエリキー（recordingDetailQueryKey）を自ら使っているので、単体ページ
    // だけを別途再検証する配線はここには要らない（テスト:
    // recording-detail.test.tsx「ごみ箱へ移すと、ナビゲーションなしで…」）。
    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  }

  // restore は「復元」ボタン本体と、ごみ箱送りトーストの Undo（下記）の
  // 両方から呼ぶ。後者は、Undo を呼んだ時点で元のごみ箱送りを起こした
  // RecordingActions 自身がすでにアンマウントされていることがある --- トーストは
  // 別の画面へ遷移した後にも押せるので、そのとき詳細ページ（と RecordingActions）は
  // 画面に無い。`useRestoreRecording` の `mutate` はコンポーネントに束縛された
  // `useMutation` の内部状態を経由するため、渡した `onSuccess`/`onError` が
  // アンマウント後も確実に呼ばれる保証を前提にできない（実測: `recording-detail.test.tsx`
  // 「ごみ箱へ移すと Undo 付きトーストが出て、「元に戻す」でライブラリ表示に戻る」を
  // `useRestoreRecording` の `mutate` 経由に戻して壊すと、リクエストは飛ぶが渡した
  // onSuccess が呼ばれないまま表示が戻らず、アサーションで落ちる）。生成された
  // 素の関数（`restoreRecordingRequest`）を直接呼び、`queryClient`
  // （`useQueryClient()` はマウント状態に依存しない安定した参照）で
  // invalidate する形にして、この依存を断つ。
  //
  // 復元の効果は「復元」ボタン本体からの呼び出しでは必ず画面に見える ---
  // 単体ページ（recording-detail.tsx）で trash 判定が反転してボタン・削除日時
  // 表示が入れ替わる。追加で言うことも無いので成功トーストは無音化する
  // （issue #297）。
  //
  // **Undo 経由の呼び出しは事情が違う。** トーストは最大 6 秒後、かつ別の
  // 画面へ遷移した後にも押せるので、そのときは復元の効果を画面上で確認
  // できるとは限らない（対象の録画をもう見ていないことがある）うえ、
  // 成功トーストも出さないので追加のフィードバックも無い。ここでは
  // 「Undo ボタンを押した」こと自体（ボタンが消える）を操作の完了通知として
  // 扱い、割り切っている --- 失敗時は場所を問わず追える情報（`復元に
  // 失敗しました`）を出す。
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
          {/* 取り返せる操作（Undo あり）なので secondary --- 取り返しがつかない
              完全削除（⋮ の中）との強さの逆転を作らない（issue #467 レビュー）。 */}
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
                    // ごみ箱送りの効果（単体ページのボタン入れ替え）は
                    // restore と同じ理由で常に画面に見えるが、
                    // ごみ箱送りは復元で即座に取り消せる安価な操作なので、
                    // 素の成功通知の代わりに Undo 付きトーストにする
                    // （`pages/programs.tsx` の予約作成 + 取消と同じ形。
                    // issue #297 が指す理想形）。復元と違ってここは Undo を
                    // 提供する側なので silence だけでは終わらせない。
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
        {/*
          事後追加は凍結の例外（issue #133、docs/storage.md §6「凍結の例外:
          事後追加」）。ごみ箱に入った録画は削除 reconcile 対象なので出さない
          （下の trash 分岐と同じ理由）。
        */}
        <AddEncodeProfilesAction recording={recording} />
      </div>
    )
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      {/* 復元はごみ箱にいるときの主操作なので露出（issue #467 の 3 段）。 */}
      <Button type="button" variant="secondary" size="sm" disabled={busy} onClick={() => restore()}>
        復元
      </Button>
      {/* 完全削除は稀・破壊的な操作なので overflow へ（issue #467、rules.tsx と
          同じ dropdown-menu）。overflow に入れても確認 AlertDialog は残す
          （メニューに入れたから確認を省く、はしない）。 */}
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
                // ダイアログは AlertDialogAction（AlertDialogPrimitive.Close ラップ）が
                // クリックで自動的に閉じるので、ここでは実行の確定のみ行う。
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

/**
 * AddEncodeProfilesAction は録画完了後に事後的にエンコードを追加依頼するボタン
 * （issue #133、凍結の例外。docs/storage.md §6「凍結の例外: 事後追加」）。
 *
 * `GET /api/encode-profiles` の一覧から、この録画の `encodeProfiles`（凍結された
 * desired。pending なジョブのぶんも含む）に無いものだけを選択肢にする ---
 * 既に追加済み/完了済みのプロファイルを選ばせない（罠: `UniqueOpts` が二重投入を
 * 黙って握りつぶすため、UI 側で「追加済み」を出して二重依頼に見せない）。
 *
 * `sizeBytes` は一覧の射影（`recordingsFromJoins`、`internal/api/recordings_query.go`）
 * が `a.kind = 'original' AND a.state <> 'deleted'` の行から埋める。一方サーバー側の
 * 409 判定（`GetActiveOriginalMediaAsset`、`internal/db/queries/media_assets.sql`）は
 * `state = 'active'` だけを見る --- **2 つの条件は同じではない**。`state = 'deleting'`
 * （unlink 待ち）の原本は射影には出る（`sizeBytes` あり）が 409 判定には出ないため、
 * `sizeBytes` があってもボタンを押すと確定で 409 になることがある
 * （`internal/api/recordings.go` の `AddRecordingEncodeProfiles` が同じ理由でこの
 * ケースを踏まえて 409 文言を hedge している）。ここでの `hasOriginal` はこの近似の
 * 上に立つ先読みであり、409 を完全には避けられない。
 *
 * `sizeBytes` が省略される（`hasOriginal` が偽になる）録画は「原本が削除された」に
 * 限らない --- ingest がまだ完了していない/失敗中でリトライ待ちの録画（`media_assets`
 * に `kind=original` の行が 1 つも無い）も同じ形になる（issue #211: 実観測では
 * `/mnt/media` の権限不足で ingest が permission denied のままリトライ中だった録画に
 * 「原本が削除済み」と断定する文言が出て誤誘導になった）。区別する情報が API に
 * 無いので、断定しない中立文言に落とす。
 */
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
  if (profilesQuery.isPending || profiles.length === 0) {
    // 取得中、または設定にプロファイルが無い場合は何も出さない
    // （EncodeSettingsFields と違い、こちらは無くても他の操作に支障が無い）。
    return null
  }

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
                        // 409 はサーバー側のメッセージが英語かつ「削除済みとは
                        // 限らない」複数原因の hedge 文言（AddRecordingEncodeProfiles
                        // の doc コメント参照）なので、そのまま出さず 409 専用の
                        // 日本語文言に翻訳する。hasOriginal の近似が破れて
                        // 「原本あり」に見えるボタンを押しても 409 になりうる
                        // （上記 doc コメントの `state = 'deleting'` の説明）ため、
                        // ここで初めてこの状態を利用者に伝える。
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
