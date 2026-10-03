import { Link, useBlocker, useLocation, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useCallback, useRef, useState } from 'react'
import { ArrowLeft } from 'lucide-react'

import { useGetRecording } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { RecordingActions } from '@/components/recording-actions'
import { RecordingDetail } from '@/components/recording-detail-panel'
import type { ChapterEditorCommands, ChapterEditorStatus } from '@/components/recording-chapter-editor'
import { Button } from '@/components/ui/button'
import { recordingDetailQueryKey } from '@/lib/recording-detail-cache'
import { formatDateTime } from '@/lib/format'
import { hasLiveIngestProgress, ingestRefetchIntervalMs } from '@/lib/ingest'

/**
 * RecordingDetailPage は録画単体の着地先。
 *
 * 録画は一覧内展開でしか見られず単体の URL を持たなかったため、skip 理由
 * （「重複（録画 #345）」）や予約 → 録画の導線がリンクの終点を持てなかった。
 *
 * `/recordings/$id` を宛先にする（推奨案どおり）。「一覧内スクロール + 展開」
 * は無限リストで対象が読み込み済みとは限らず成立しない。
 *
 * 旧 `reservations.id` のような導出 id とは違い、`recordings.id` は
 * ingest（watcher）が一度作ったら変わらない不可逆な事実の id なので、
 * `/reservations/$site/$programId` と違って id をそのまま URL に使ってよい。
 *
 * 本体（プレイヤー・メタデータ・削除系操作）は
 * `components/recording-detail-panel.tsx` の `RecordingDetail` を使う。一覧は
 * インライン展開せず、行本体からこの単体ページへ移動する（issue #311）。
 */
export function RecordingDetailPage() {
  const { id } = useParams({ from: '/recordings/$id' })
  const location = useLocation()
  const search = useSearch({ from: '/recordings/$id' })
  const navigate = useNavigate({ from: '/recordings/$id' })
  const idNum = Number(id)
  // 編集モードに入った録画の id。録画を移ったら（次の回への遷移など）編集モードを抜ける。
  const [editingId, setEditingId] = useState<number | null>(null)
  // 別の録画へ移った瞬間に捨てる（id の比較だけだと、戻ってきたときに編集モードが復活する）。
  if (editingId !== null && editingId !== idNum) setEditingId(null)
  const chapterEditing = editingId === idNum
  const [confirmChapterExit, setConfirmChapterExit] = useState(false)
  const [chapterEditorStatus, setChapterEditorStatus] = useState<ChapterEditorStatus>({
    source: 'auto',
    dirty: false,
    stale: false,
  })
  const chapterEditorCommandsRef = useRef<ChapterEditorCommands | null>(null)
  const onChapterEditorStatusChange = useCallback((status: ChapterEditorStatus) => {
    setChapterEditorStatus((previous) =>
      previous.source === status.source && previous.dirty === status.dirty && previous.stale === status.stale
        ? previous
        : status,
    )
  }, [])
  const navigationBlocker = useBlocker({
    shouldBlockFn: () => chapterEditing && chapterEditorStatus.dirty,
    // 未保存の下書きがあるときだけ、リロード・タブを閉じる操作にブラウザ標準の確認を出す。
    enableBeforeUnload: () => chapterEditing && chapterEditorStatus.dirty,
    withResolver: true,
  })
  const navigationIsBlocked = navigationBlocker.status === 'blocked'

  const resetChapterEditStatus = () => {
    setChapterEditorStatus({ source: 'auto', dirty: false, stale: false })
  }
  const leaveChapterEditMode = () => {
    setEditingId(null)
    setConfirmChapterExit(false)
    resetChapterEditStatus()
  }
  const requestChapterExit = () => {
    if (chapterEditorStatus.dirty || chapterEditorStatus.stale) setConfirmChapterExit(true)
    else leaveChapterEditMode()
  }
  const discardAndExitChapterEdit = () => {
    chapterEditorCommandsRef.current?.discard()
    leaveChapterEditMode()
  }
  const saveAndExitChapterEdit = async () => {
    if (await chapterEditorCommandsRef.current?.save()) leaveChapterEditMode()
  }
  const resetAndExitChapterEdit = async () => {
    if (await chapterEditorCommandsRef.current?.reset()) leaveChapterEditMode()
  }
  const discardAndLeavePage = () => {
    chapterEditorCommandsRef.current?.discard()
    leaveChapterEditMode()
    if (navigationBlocker.status === 'blocked') navigationBlocker.proceed()
  }

  // 追っかけ再生の画質は `?liveProfile=` に持つ（issue #874）。**既定は URL に
  // 書き戻さない** --- 明示的に選んだ値だけを載せる（`/live` の `?profile=` と
  // 同じ規律。`docs/frontend/live.md`）。`replace` にするのは、切替のたびに
  // ブラウザ履歴が積み上がらないようにするためである。
  //
  // **`hash` を明示的に渡す。** `navigate` は指定しなかった部分を現在の
  // location から引き継がない（実測: `#chase` を渡さないと href が
  // `/recordings/3?liveProfile=sd` になり、下の `key` が変わって
  // `RecordingDetail` ごと作り直される = 追っかけが先頭から再生し直しになる）。
  const selectLiveProfile = (name: string) => {
    void navigate({ search: { ...search, liveProfile: name }, hash: location.hash, replace: true })
  }

  // 進捗の数字が動いている間だけ定期再取得する（issue #212。一覧側の
  // useInfiniteQuery と同じ判定・同じ間隔）。SSE はヒントなので、進捗は REST の
  // 再取得で収束させる（不変条件 5）。止めた後は lib/events.ts の 60 秒
  // invalidate が収束させる（hasLiveIngestProgress 参照）。
  const query = useGetRecording(idNum, {
    query: {
      queryKey: recordingDetailQueryKey(idNum),
      refetchInterval: (q) => {
        const rec = unwrap(q.state.data)
        return rec !== undefined && hasLiveIngestProgress(rec, Date.now())
          ? ingestRefetchIntervalMs
          : false
      },
    },
  })
  const recording = unwrap(query.data)
  // ごみ箱の録画（deletedAt 付き）も 200 で返る（getRecording の openapi.yaml
  // description）。この真偽で再生系を出さない規律（下記 RecordingDetail）を適用する。
  const trash = recording?.deletedAt != null

  return (
    <>
      <PageHeader
        title={chapterEditing ? (
          <div className="flex min-w-0 flex-col gap-0.5 md:flex-row md:items-baseline md:gap-3">
            <span className="shrink-0">チャプターを直す</span>
            {recording && (
              <span className="hidden min-w-0 truncate text-sm font-normal text-muted-foreground md:inline">
                {recording.title} · {formatDateTime(recording.startAt)} · {chapterEditorStatus.source === 'user' ? '確認済み' : '自動検出（未確認）'}
                {chapterEditorStatus.dirty ? ' · 未保存の変更があります' : ''}
                {chapterEditorStatus.stale ? ' · サーバー側の内容が変わりました' : ''}
              </span>
            )}
          </div>
        ) : '録画の詳細'}
        leading={chapterEditing ? (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="hidden md:inline-flex"
            aria-label="編集をやめる"
            onClick={requestChapterExit}
          >
            <ArrowLeft />
          </Button>
        ) : (
          // history.back ではなくリンク（一覧へ）。issue #467 で PageHeader に
          // 乗せてもこの挙動は変えない。
          <Button variant="ghost" size="icon" aria-label="戻る" render={<Link to="/recordings" />}>
            <ArrowLeft />
          </Button>
        )}
        actions={chapterEditing ? (
          <>
            <Button
              type="button"
              variant="ghost"
              className="hidden md:inline-flex"
              disabled={chapterEditorStatus.source === 'auto'}
              onClick={() => void resetAndExitChapterEdit()}
            >
              自動に戻す
            </Button>
            <Button type="button" variant="outline" onClick={requestChapterExit}>やめる</Button>
            <Button type="button" disabled={!chapterEditorStatus.dirty || chapterEditorStatus.stale} onClick={() => void saveAndExitChapterEdit()}>
              保存
            </Button>
          </>
        ) : recording ? <RecordingActions recording={recording} trash={trash} /> : undefined}
      >
        {chapterEditing && (confirmChapterExit || navigationIsBlocked) && (
          <div className="absolute inset-x-0 top-full flex flex-wrap items-center gap-2 border-b border-border bg-background px-4 py-2 text-sm shadow-md" role="alert" data-testid="chapter-exit-confirmation">
            <span>未保存の変更があります。変更を捨てて編集を終了しますか？</span>
            <Button type="button" size="sm" variant="destructive" onClick={navigationIsBlocked ? discardAndLeavePage : discardAndExitChapterEdit}>
              変更を捨てる
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => {
                if (navigationIsBlocked) navigationBlocker.reset()
                setConfirmChapterExit(false)
              }}
            >
              編集を続ける
            </Button>
          </div>
        )}
      </PageHeader>

      {query.isError ? (
        // onRetry は付けない: この文言は 404 と他の取得失敗を区別していない
        // （既存の仕様、この PR の対象外）ので、「見つかりません」と言い切った
        // 直後に再試行を勧めると、本当に無い場合の確信を弱めてしまう。
        // 区別を足すこと自体は別タスク（issue #467 レビューで判断を記録）。
        <ErrorState>録画が見つかりません</ErrorState>
      ) : query.isPending || !recording ? (
        <ListSkeleton rows={4} />
      ) : (
        <div className={chapterEditing ? 'px-4 py-2' : 'px-4 py-4'}>
          <RecordingDetail
            // key に録画 id を含めない。次のエピソードへ移るとき、プレイヤーの DOM を作り直すと
            // 全画面が解除される。録画ごとの state は RecordingDetail が id の変化で自分で戻す。
            key={location.hash}
            recording={recording}
            trash={trash}
            chase={location.hash === 'chase'}
            liveProfile={search.liveProfile}
            startAtBeginning={search.fromBeginning}
            chapterEditing={chapterEditing}
            onEnterChapterEditing={() => {
              setConfirmChapterExit(false)
              setEditingId(idNum)
            }}
            chapterEditorCommandsRef={chapterEditorCommandsRef}
            onChapterEditorStatusChange={onChapterEditorStatusChange}
            onSelectLiveProfile={selectLiveProfile}
            onNavigateToRecording={(nextId) =>
              void navigate({
                to: '/recordings/$id',
                params: { id: String(nextId) },
                hash: '',
              })
            }
          />
        </div>
      )}
    </>
  )
}
