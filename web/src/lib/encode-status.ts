/**
 * エンコードの待ち・実行中・失敗の表示モデル（issue #316）。
 *
 * `Recording.encodeStatus`（サーバーが recording_encode_attempts から毎回
 * 導出する。`openapi.yaml` の `EncodeJobStatus`）を画面に出す文言へ落とすだけの
 * 純粋関数を置く --- jsdom で測れる部分（文言）をコンポーネントから切り離して
 * 単体テストできるようにするため。
 *
 * `encodeStatus` に出るのは **完了していないプロファイルだけ**。完了済み
 * （`encodedAssets` に現れる）プロファイルはここに出ない --- 同じ情報を
 * 2 つの配列で主張しないため（openapi.yaml の `encodeStatus` description）。
 */

import type { EncodeJobStatusState } from '@/api/generated'

/**
 * encodeJobStatusLabel は 1 プロファイルの状態を文言に落とす。
 *
 * `failed` は「二度と来ない」の断定ではない --- 恒久的に失敗し続けるプロファイル
 * では `EncodeReconcileWorker` が再投入するたび `queued`/`running` に戻る
 * （internal/db/migrations/00041_recording_encode_attempts.sql 参照）。
 */
export function encodeJobStatusLabel(state: EncodeJobStatusState): string {
  switch (state) {
    case 'queued':
      return 'エンコード待ち'
    case 'running':
      return 'エンコード中'
    case 'failed':
      return 'エンコード失敗'
  }
}
