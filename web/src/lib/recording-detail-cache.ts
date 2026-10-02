import type { QueryClient } from '@tanstack/react-query'

import type { Recording } from '@/api/generated'
import { recordingsQueryKeyPrefix } from '@/lib/events'

/**
 * recordingDetailQueryKey は録画詳細ページ自身のクエリキー。
 *
 * orval が生成する `getGetRecordingQueryKey`（`['/api/recordings/{id}']`、id を埋め込んだ
 * 1 要素の文字列）は使わない。一覧側の mutater（`RecordingActions` / `AddEncodeProfilesAction`）は
 * どちらも `[recordingsQueryKeyPrefix]` で invalidate する。TanStack Query の既定の前方一致は
 * 先頭要素から比べるので、生成キーはそこに掛からない。**先頭要素を一覧と同じ
 * `recordingsQueryKeyPrefix` に揃えておけば**、今ある mutater も将来足す mutater も、
 * 単体ページへの配線（`onMutated` のような prop）なしでこのキャッシュを巻き込む
 * （通し忘れは型エラーにならず黙って抜けるため、覚える運用にしない）。
 */
export function recordingDetailQueryKey(id: number) {
  return [recordingsQueryKeyPrefix, 'detail', id] as const
}

/**
 * seedRecordingDetail は一覧で既に持っている録画を、移動先の詳細ページのキャッシュへ先に入れる。
 *
 * 次のエピソードへ移るとき、詳細の取得が終わるまで空の画面（スケルトン）にするとプレイヤーの
 * DOM ごと消え、全画面が解除される。行の中身で先に描き、`updatedAt: 0` で stale にして
 * 描画後に取り直させる（一覧の行と詳細の差は取り直しで埋まる）。
 */
export function seedRecordingDetail(queryClient: QueryClient, recording: Recording) {
  const key = recordingDetailQueryKey(recording.id)
  if (queryClient.getQueryData(key) !== undefined) return
  queryClient.setQueryData(key, { status: 200, data: recording }, { updatedAt: 0 })
}
