import { Link, useParams } from '@tanstack/react-router'
import { ArrowLeft } from 'lucide-react'
import { useState } from 'react'

import { useGetRecording } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { ErrorState, ListSkeleton } from '@/components/page'
import { Button } from '@/components/ui/button'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { RecordingDetail, StatusBadge } from '@/pages/recordings'

/**
 * recordingDetailQueryKey は単体ページ自身のクエリキー。
 *
 * orval が生成する `getGetRecordingQueryKey`（`['/api/recordings/{id}']`、id を
 * 埋め込んだ 1 要素の文字列）は使わない。一覧側の mutater（`RecordingActions` の
 * `invalidate` / `AddEncodeProfilesAction` の `onSuccess`、両方
 * `pages/recordings.tsx`）はどちらも `queryClient.invalidateQueries({ queryKey:
 * ['/api/recordings'] })` で捨てる --- TanStack Query の既定の前方一致
 * （`partialMatchKey`）はフィルタキーに書いた要素を**前から順に**比較する
 * ため（ここではフィルタが `['/api/recordings']` という 1 要素なので、
 * 実質「先頭要素が等しいか」になる）、生成された 1 要素キー
 * （'/api/recordings/{id}' という別の文字列）はそこに前方一致しない。
 *
 * `RecordingDetail` の下に mutater を足すたびに単体ページへの配線
 * （`onMutated` のような prop）を手で通す形は、通し忘れても型エラーにも
 * ならず黒く抜ける（実際に `AddEncodeProfilesAction` がこの穴を最初に踏んだ
 * --- issue #232 のレビューで実機再現された）。**「覚えておく」を要求する
 * 代わりに、単体ページ自身のキーの先頭要素を一覧と同じ `'/api/recordings'` に
 * 揃えておけば、一覧側のどの mutater（今あるものも将来足されるものも）の
 * invalidate がこのページのキャッシュも自動的に巻き込む**（前方一致は
 * `getListRecordingsQueryKey` が返す `['/api/recordings', {...}]` にも同じ
 * 理屈で効いている）。`RecordingDetail` 配下に prop を新設する必要が無くなる。
 */
function recordingDetailQueryKey(id: number) {
  return ['/api/recordings', 'detail', id] as const
}

/**
 * RecordingDetailPage は録画単体の着地先。
 *
 * 録画は一覧内展開でしか見られず単体の URL を持たなかったため、skip 理由
 * （「重複（録画 #345）」）や予約 → 録画の導線がリンクの終点を持てなかった。
 *
 * `/recordings/$id` を宛先にする（推奨案どおり）。「一覧内スクロール + 展開」
 * は無限リストで対象が読み込み済みとは限らず成立しない。
 *
 * `reservations.id` のような導出 id とは違い、`recordings.id` は
 * ingest（watcher）が一度作ったら変わらない不可逆な事実の id なので、
 * `/reservations/$site/$programId` と違って id をそのまま URL に使ってよい。
 *
 * 本体（プレイヤー・メタデータ・削除系操作）は一覧の行展開
 * （`pages/recordings.tsx` の `RecordingDetail`）と同じ部品を再利用する ---
 * 「再生・操作が一覧の展開と同等に機能する」を、実装を 2 系統に分けずに満たす。
 */
export function RecordingDetailPage() {
  const { id } = useParams({ from: '/recordings/$id' })
  const idNum = Number(id)
  const [thumbFailed, setThumbFailed] = useState(false)

  const query = useGetRecording(idNum, { query: { queryKey: recordingDetailQueryKey(idNum) } })
  const recording = unwrap(query.data)
  // ごみ箱の録画（deletedAt 付き）も 200 で返る（getRecording の openapi.yaml
  // description）。この真偽で一覧の展開と同じ規律（再生系を出さない）を適用する。
  const trash = recording?.deletedAt != null

  return (
    <>
      <header
        className="sticky z-10 flex items-center gap-2 border-b border-border bg-background/95 px-2 py-2 backdrop-blur"
        style={{ top: 'var(--breaker-banner-height, 0px)' }}
      >
        <Button variant="ghost" size="icon" aria-label="戻る" render={<Link to="/recordings" />}>
          <ArrowLeft />
        </Button>
        <h1 className="text-base font-semibold tracking-tight">録画の詳細</h1>
      </header>

      {query.isError ? (
        <ErrorState>録画が見つかりません</ErrorState>
      ) : query.isPending || !recording ? (
        <ListSkeleton rows={4} />
      ) : (
        <div className="flex flex-col gap-4 px-4 py-4">
          <section className="flex gap-3">
            {/* サムネイルは一覧行と同じ規律: ごみ箱ではそもそもリクエストしない
                （配信側が deleted_at IS NOT NULL を 404 にする契約。docs/api/media.md）。 */}
            <div className="size-20 shrink-0 overflow-hidden rounded bg-muted">
              {!trash && !thumbFailed ? (
                <img
                  src={`/api/recordings/${recording.id}/thumbnail`}
                  alt=""
                  className="size-full object-cover"
                  onError={() => setThumbFailed(true)}
                />
              ) : (
                <div className="size-full bg-muted" aria-hidden />
              )}
            </div>
            <div className="min-w-0 flex-1">
              <h2 className="text-lg font-medium">{recording.title || '（番組名なし）'}</h2>
              <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                <StatusBadge status={recording.status} />
                <span className="shrink-0">{recording.serviceName}</span>
                <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
                <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
                {recording.sizeBytes !== undefined && (
                  <span className="shrink-0">{formatBytes(recording.sizeBytes)}</span>
                )}
              </div>
              {trash && recording.deletedAt && (
                <p className="mt-1 text-xs text-muted-foreground">
                  ごみ箱（削除 {formatDateTime(recording.deletedAt)}）
                </p>
              )}
            </div>
          </section>

          <RecordingDetail recording={recording} trash={trash} />
        </div>
      )}
    </>
  )
}
